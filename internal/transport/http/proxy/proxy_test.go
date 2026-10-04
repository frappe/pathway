package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/transport/respond"
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

// A stream cut inside a frame is billed by the last frame that arrived whole: an engine of ours
// reports the running count on every chunk.
func TestACutStreamKeepsItsLastWholeUsageFrame(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":8}}`+"\n\n"+
			`data: {"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`+"\n\n"+
			`data: {"usage":{"prompt_tokens":7,"comple`)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(server.Close)

	out := forward(New(Options{}, quiet()), server.URL, false)
	if out.Cut != domain.CutUpstream || !strings.Contains(out.Usage, `"total_tokens":9`) ||
		!strings.Contains(out.UsageStart, `"total_tokens":8`) {
		t.Errorf("cut %q, start %q, last %q", out.Cut, out.UsageStart, out.Usage)
	}
}

// forwardOn is forward on a surface, keeping what the client received.
func forwardOn(p *Proxy, target, dialect string) (Outcome, *httptest.ResponseRecorder) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	r = r.WithContext(respond.WithDialect(r.Context(), dialect))
	w := httptest.NewRecorder()
	var out Outcome
	p.Forward(w, r, target, false, ModelSwap{}, &out)
	return out, w
}

// A wait for headers that runs out is a 504 that says so, not the 502 of a refused connection.
func TestAHeaderTimeoutIsA504(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // read to the end, or the server never sees the hop leave
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	out, w := forwardOn(New(Options{ReadTimeout: 100 * time.Millisecond}, quiet()), server.URL, domain.DialectOpenAI)
	if w.Code != http.StatusGatewayTimeout || !strings.Contains(w.Body.String(), "upstream timed out") {
		t.Errorf("status %d, body %s", w.Code, w.Body)
	}
	if out.Status != 0 {
		t.Errorf("outcome status = %d; a hop with no answer must stay 0 to count against the upstream", out.Status)
	}
}

// mutedListener hands out connections whose writes can be switched off: the far end still reads
// and thinks it answers, and nothing arrives — a peer that vanished without a FIN.
type mutedListener struct {
	net.Listener
	muted *atomic.Bool
}

func (l mutedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return mutedConn{Conn: conn, muted: l.muted}, nil
}

type mutedConn struct {
	net.Conn
	muted *atomic.Bool
}

func (c mutedConn) Write(p []byte) (int, error) {
	if c.muted.Load() {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

// An HTTP/2 connection that dies without a FIN would otherwise keep taking requests, each one
// hanging for the read timeout, until the kernel gave up on it. The ping closes it instead, and the
// next request dials a fresh one.
func TestAnUpstreamConnectionThatDiesQuietlyIsPingedOut(t *testing.T) {
	var muted, sawHTTP2 atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHTTP2.Store(r.ProtoMajor == 2)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	server.Listener = mutedListener{Listener: server.Listener, muted: &muted}
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	const ping = 50 * time.Millisecond
	p := New(Options{ReadTimeout: 100 * ping, PingAfter: ping, PingTimeout: ping}, quiet())
	if out := forward(p, server.URL, false); out.Status != http.StatusOK || !sawHTTP2.Load() {
		t.Fatalf("first hop: status %d, HTTP/2 %t, want 200 over HTTP/2", out.Status, sawHTTP2.Load())
	}

	muted.Store(true)
	start := time.Now()
	out, w := forwardOn(p, server.URL, domain.DialectOpenAI)
	if out.Status != 0 || w.Code != http.StatusBadGateway {
		t.Errorf("hop on the dead connection: outcome %d, client saw %d, want a failed hop and a 502", out.Status, w.Code)
	}
	if elapsed := time.Since(start); elapsed > 20*ping {
		t.Errorf("the dead connection held its request for %s, want it pinged out in about %s", elapsed, 2*ping)
	}

	muted.Store(false)
	if out := forward(p, server.URL, false); out.Status != http.StatusOK {
		t.Errorf("hop after the dead connection was closed returned %d, want 200 on a fresh one", out.Status)
	}
}

// A stream the upstream abandons ends in an error event the client's SDK raises on, in its own
// surface's shape, after whatever had already arrived. Usage and the cut are read as before.
func TestAnAbandonedStreamEndsInAnErrorEvent(t *testing.T) {
	const limit = 100 * time.Millisecond
	first := `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}` + "\n\n"
	for name, tc := range map[string]struct {
		dialect, sent, cut, want string
		silent                   bool
	}{
		"silent, openai": {domain.DialectOpenAI, first, domain.CutUpstreamIdle,
			first + `data: {"error":{"message":"upstream went silent","type":"api_error"}}` + "\n\n", true},
		"silent, anthropic": {domain.DialectAnthropic, first, domain.CutUpstreamIdle,
			first + "event: error\ndata: " + `{"error":{"message":"upstream went silent","type":"api_error"},"type":"error"}` + "\n\n", true},
		"broke off mid-line": {domain.DialectOpenAI, first + `data: {"cho`, domain.CutUpstream,
			first + `data: {"cho` + "\n\n" + `data: {"error":{"message":"upstream broke off the stream","type":"api_error"}}` + "\n\n", false},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, tc.sent)
			w.(http.Flusher).Flush()
			if tc.silent {
				<-r.Context().Done()
				return
			}
			panic(http.ErrAbortHandler)
		}))
		t.Cleanup(server.Close)

		out, w := forwardOn(New(Options{ReadTimeout: limit}, quiet()), server.URL, tc.dialect)
		if got := w.Body.String(); got != tc.want {
			t.Errorf("%s: client got\n%q\nwant\n%q", name, got, tc.want)
		}
		if out.Cut != tc.cut {
			t.Errorf("%s: cut %q, want %q", name, out.Cut, tc.cut)
		}
		if !strings.Contains(out.Usage, `"prompt_tokens":3`) {
			t.Errorf("%s: usage lost: %q", name, out.Usage)
		}
	}
}
