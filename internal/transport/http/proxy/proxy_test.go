package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phot0n/pathway/internal/domain"
)

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func tlsEngine(t *testing.T) *httptest.Server {
	t.Helper()
	// httptest signs its own certificate, which is exactly what an engine inside the VPC does and
	// exactly what a vendor on the public internet must never be able to get away with.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func forward(p *Proxy, target string, external bool) Outcome {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	var out Outcome
	p.Forward(httptest.NewRecorder(), r, target, external, ModelSwap{}, &out)
	return out
}

// The fleet default is off because an engine answers on a certificate we signed ourselves. A
// vendor hop carries that vendor's API key over the public internet, so an unverified one hands the
// key to whoever answers the address.
func TestAnExternalHopVerifiesTheCertificateWhateverTheFleetDefaultIs(t *testing.T) {
	engine := tlsEngine(t)
	p := New(Options{VerifyUpstream: false}, quiet())

	if outcome := forward(p, engine.URL, true); outcome.Status != 0 {
		t.Errorf("external hop to an untrusted certificate returned %d, want a failed hop", outcome.Status)
	}
}

// The same address, dialled as one of ours, still trusts the self-signed certificate — otherwise
// this change would take every engine hop down with it.
func TestAnInternalHopStillHonoursTheFleetDefault(t *testing.T) {
	engine := tlsEngine(t)
	p := New(Options{VerifyUpstream: false}, quiet())

	if outcome := forward(p, engine.URL, false); outcome.Status != http.StatusOK {
		t.Errorf("internal hop returned %d, want 200", outcome.Status)
	}
}

// Both hops reach one host. Pooling on the address alone would let the external one ride a
// connection already dialled without checks.
func TestOneHostGetsATransportPerVerificationSetting(t *testing.T) {
	engine := tlsEngine(t)
	p := New(Options{VerifyUpstream: false}, quiet())

	if outcome := forward(p, engine.URL, false); outcome.Status != http.StatusOK {
		t.Fatalf("internal hop returned %d, want 200", outcome.Status)
	}
	if outcome := forward(p, engine.URL, true); outcome.Status != 0 {
		t.Errorf("external hop reused the unverified pool and returned %d", outcome.Status)
	}
}

// deadClient is a browser tab closed mid-stream: the response writer refuses the copy, which is
// what makes net/http's ReverseProxy panic with http.ErrAbortHandler.
type deadClient struct{ *httptest.ResponseRecorder }

func (deadClient) Write([]byte) (int, error) { return 0, errors.New("client gone") }

func (d deadClient) Unwrap() http.ResponseWriter { return d.ResponseRecorder }

// A non-streaming body is one line, and past carryLimit the tee keeps only its tail. The usage
// object is in that tail, so the request must still meter — under the limit and over it.
func TestUsageSurvivesABodyPastTheCarryLimit(t *testing.T) {
	for name, padding := range map[string]int{"under": carryLimit / 2, "over": carryLimit * 2} {
		body := `{"choices":[{"text":"` + strings.Repeat("a", padding) +
			`"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`
		tee := newUsageTee(io.NopCloser(strings.NewReader(body)))
		if _, err := io.Copy(io.Discard, tee); err != nil {
			t.Fatal(err)
		}
		if u, ok := domain.ParseUsage([]byte(tee.Usage())); !ok || u.Total != 9 {
			t.Errorf("%s the limit: got %+v ok=%v, want Total=9", name, u, ok)
		}
	}
}

// A client that hangs up mid-stream unwinds Forward through the panic, but the vendor billed
// whatever it had already generated — so the usage frame the tee caught must still reach the
// caller, or every abandoned stream meters as zero tokens.
func TestUsageSurvivesAClientHangup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`+"\n\n")
	}))
	t.Cleanup(server.Close)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	// ReverseProxy only panics on a copy error when it can tell it is serving a real connection.
	r = r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, &http.Server{}))

	var out Outcome
	func() {
		defer func() {
			if p := recover(); p == nil {
				t.Fatal("a refused copy did not abort the handler")
			} else if !errors.Is(p.(error), http.ErrAbortHandler) {
				t.Fatalf("panic = %v, want ErrAbortHandler", p)
			}
		}()
		New(Options{}, quiet()).Forward(deadClient{httptest.NewRecorder()}, r, server.URL, false,
			ModelSwap{}, &out)
	}()

	if !strings.Contains(out.Usage, `"total_tokens":9`) {
		t.Errorf("usage lost to the hangup: %q", out.Usage)
	}
}
