package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	return p.Forward(httptest.NewRecorder(), r, target, external)
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
