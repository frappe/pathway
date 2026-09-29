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
	"time"

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

// Past carryLimit the tee keeps only the tail of a body. The usage object is in that tail, so the
// request must still meter — under the limit and over it.
func TestUsageSurvivesABodyPastTheCarryLimit(t *testing.T) {
	for name, padding := range map[string]int{"under": carryLimit / 2, "over": carryLimit * 2} {
		body := `{"choices":[{"text":"` + strings.Repeat("a", padding) +
			`"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`
		tee := newUsageTee(io.NopCloser(strings.NewReader(body)), false)
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

// An Anthropic stream reports the prompt on its first event and the output on its last, so both
// lines reach the caller. A response with one usage line has no start.
func TestTheFirstAndLastUsageLinesAreBothKept(t *testing.T) {
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":66,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":26}}` + "\n\n"
	for name, tc := range map[string]struct{ contentType, body, start, last string }{
		"split": {"text/event-stream; charset=utf-8", stream, `"input_tokens":66`, `"output_tokens":26`},
		"whole": {"application/json", `{"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`, "", `"total_tokens":9`},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", tc.contentType)
			_, _ = io.WriteString(w, tc.body)
		}))
		t.Cleanup(server.Close)

		out := forward(New(Options{}, quiet()), server.URL, false)
		if !strings.Contains(out.Usage, tc.last) || (tc.start == "") != (out.UsageStart == "") ||
			!strings.Contains(out.UsageStart, tc.start) {
			t.Errorf("%s: start %q, last %q", name, out.UsageStart, out.Usage)
		}
	}
}

// A compressed body is one the tee cannot read, so the upstream is never offered the choice.
func TestTheUpstreamIsNeverAskedToCompress(t *testing.T) {
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.Header.Get("Accept-Encoding")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	r.Header.Set("Accept-Encoding", "gzip, br")
	var out Outcome
	New(Options{}, quiet()).Forward(httptest.NewRecorder(), r, server.URL, false, ModelSwap{}, &out)
	if out.Status != http.StatusOK || asked != "" {
		t.Errorf("status %d, upstream saw Accept-Encoding %q", out.Status, asked)
	}
}

// OpenAI prints a body that is not streamed over many lines, as measured on the wire: the line
// that names "usage" holds none of it. The body is one document and is read as one.
func TestUsageIsReadOutOfABodyPrintedOverManyLines(t *testing.T) {
	body := "{\n  \"id\": \"chatcmpl-1\",\n  \"object\": \"chat.completion\",\n  \"choices\": [\n    {\n" +
		"      \"message\": {\n        \"content\": \"the \\\"usage\\\" of a word\"\n      }\n    }\n  ],\n" +
		"  \"usage\": {\n    \"prompt_tokens\": 2328,\n    \"completion_tokens\": 16,\n    \"total_tokens\": 2344,\n" +
		"    \"prompt_tokens_details\": {\n      \"cached_tokens\": 0,\n      \"cache_write_tokens\": 2325\n    }\n  },\n" +
		"  \"system_fingerprint\": null\n}\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	out := forward(New(Options{}, quiet()), server.URL, false)
	u, ok := domain.ParseUsage([]byte(out.Usage))
	if !ok || u.Prompt != 2328 || u.Completion != 16 || u.CacheWrite != 2325 || out.UsageStart != "" {
		t.Errorf("got %+v ok=%v start=%q", u, ok, out.UsageStart)
	}
}

// The same printed body, too long to hold whole: its tail still carries the usage.
func TestUsageIsReadOutOfTheTailOfALongPrintedBody(t *testing.T) {
	body := "{\n  \"choices\": [\n    {\n      \"text\": \"" + strings.Repeat("a\\n", carryLimit) + "\"\n    }\n  ],\n" +
		"  \"usage\": {\n    \"prompt_tokens\": 7,\n    \"completion_tokens\": 2,\n    \"total_tokens\": 9\n  }\n}\n"
	tee := newUsageTee(io.NopCloser(strings.NewReader(body)), false)
	if _, err := io.Copy(io.Discard, tee); err != nil {
		t.Fatal(err)
	}
	if u, ok := domain.ParseUsage([]byte(tee.Usage())); !ok || u.Total != 9 || len(tee.Usage()) > carryLimit {
		t.Errorf("got %+v ok=%v from %d bytes", u, ok, len(tee.Usage()))
	}
}

// A client that leaves before the upstream answers is nobody's failure: the upstream is told to
// stop, the line reads 499 and not 502, and the hop carries no status to count against anyone.
func TestAClientThatLeavesBeforeTheAnswerIsNotAFailedHop(t *testing.T) {
	told := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// A server only notices its peer closing once the request body has been read.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		close(told)
	}))
	t.Cleanup(server.Close)

	ctx, leave := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)).WithContext(ctx)
	time.AfterFunc(50*time.Millisecond, leave)
	recorder := httptest.NewRecorder()
	var out Outcome
	New(Options{}, quiet()).Forward(recorder, r, server.URL, false, ModelSwap{}, &out)

	if out.Cut != domain.CutClientLeft || out.Status != 0 || recorder.Code != statusClientClosed {
		t.Errorf("cut %q, upstream status %d, answered %d", out.Cut, out.Status, recorder.Code)
	}
	select {
	case <-told:
	case <-time.After(2 * time.Second):
		t.Error("the upstream was never told to stop")
	}
}

// The read timeout bounds silence, not length: a stream that keeps talking outlives it, one that
// stops is cut and says so.
func TestAnUpstreamGoneSilentIsCutAndASlowOneIsNot(t *testing.T) {
	const limit = 200 * time.Millisecond
	frame := `data: {"choices":[{"delta":{"content":"w"}}],"usage":null}` + "\n\n"
	for name, tc := range map[string]struct {
		frames int
		silent bool
		cut    string
	}{
		"slow":   {frames: 6, cut: ""},
		"silent": {frames: 2, silent: true, cut: domain.CutUpstreamIdle},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			for range tc.frames {
				_, _ = io.WriteString(w, frame)
				w.(http.Flusher).Flush()
				time.Sleep(limit / 3)
			}
			if tc.silent {
				<-r.Context().Done()
			}
		}))
		t.Cleanup(server.Close)

		start := time.Now()
		out := forward(New(Options{ReadTimeout: limit}, quiet()), server.URL, false)
		if out.Cut != tc.cut || out.Status != http.StatusOK {
			t.Errorf("%s: cut %q, status %d, want cut %q", name, out.Cut, out.Status, tc.cut)
		}
		if elapsed := time.Since(start); tc.silent && elapsed > 10*limit {
			t.Errorf("%s: held for %s past a limit of %s", name, elapsed, limit)
		}
	}
}

// An upstream that answers 200 and then breaks off is marked, so the request can be found.
func TestAnUpstreamThatBreaksOffIsMarked(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"w"}}],"usage":null}`+"\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(server.Close)

	if out := forward(New(Options{}, quiet()), server.URL, false); out.Cut != domain.CutUpstream || out.Status != http.StatusOK {
		t.Errorf("cut %q, status %d", out.Cut, out.Status)
	}
}
