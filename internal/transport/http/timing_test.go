package http

import (
	"bytes"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/frappe/pathway/internal/config"
)

func group(t *testing.T, line map[string]any, name string) map[string]any {
	t.Helper()
	g, ok := line[name].(map[string]any)
	if !ok {
		t.Fatalf("%s = %v, want a group; line = %v", name, line[name], line)
	}
	return g
}

// Where a request's time went is on its access line in three groups: the split between the gateway
// and the upstream, the gateway's share by stage, the hop's legs. A refusal above the proxy has the
// stages up to the one that refused, and no hop.
func TestTheAccessLineSaysWhereTheTimeWent(t *testing.T) {
	f := providerFixtureAnswering(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		jsonEngine(`{`+usageObject+`}`)(w, r)
	})
	access := captureAccess(t, f)
	if resp := f.post("/anthropic/v1/messages", `{"model":"anthropic/claude-4-5","max_tokens":16}`); resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body)
	}
	line := jsonLine(t, access)
	if _, present := line["upstream_ms"]; present {
		t.Error("upstream_ms still on the line; the vendor's clock is latency.vendor_ms now")
	}
	latency := group(t, line, "latency")
	total, gateway, upstream := latency["total_ms"].(float64), latency["gateway_ms"].(float64), latency["upstream_ms"].(float64)
	if math.Abs(total-(gateway+upstream)) > 0.002 || upstream < 20 || gateway <= 0 {
		t.Errorf("latency = %v, want total = gateway + upstream with the upstream's 20ms", latency)
	}
	if ttfb, _ := latency["ttfb_ms"].(float64); ttfb < 20 || ttfb > total {
		t.Errorf("latency.ttfb_ms = %v, want between the upstream's 20ms and the total", latency["ttfb_ms"])
	}
	if latency["vendor_ms"] != float64(0) {
		t.Errorf("latency.vendor_ms = %v from a vendor that sends no clock", latency["vendor_ms"])
	}
	stages := group(t, line, "gateway")
	var named float64
	for _, name := range []string{"auth", "quota", "body", "route", "fallback", "retry", "transform", "upstreamauth", "respond", "other"} {
		seconds, ok := stages[name].(float64)
		if !ok || seconds < 0 {
			t.Errorf("gateway.%s = %v, want a duration", name, stages[name])
		}
		named += seconds
	}
	if math.Abs(named-gateway) > 0.02 {
		t.Errorf("gateway phases sum to %v, gateway_ms = %v; the shares must add up", named, gateway)
	}
	for _, name := range []string{"recover", "accesslog", "drain", "meter", "handler"} {
		if _, present := stages[name]; present {
			t.Errorf("gateway.%s present; plumbing belongs in other", name)
		}
	}
	hop := group(t, line, "hop")
	if hop["reused"] != false {
		t.Errorf("hop.reused = %v on the first dial to a host", hop["reused"])
	}
	if connect, _ := hop["connect"].(float64); connect <= 0 {
		t.Errorf("hop.connect = %v, want the dial", hop["connect"])
	}
	if wait, _ := hop["wait"].(float64); wait < 20 {
		t.Errorf("hop.wait = %v, want the upstream's 20ms", hop["wait"])
	}
	for _, leg := range []string{"tls", "send", "stream", "gap_max"} {
		if _, ok := hop[leg].(float64); !ok {
			t.Errorf("hop.%s = %v, want a number", leg, hop[leg])
		}
	}
	if chunks, _ := hop["chunks"].(float64); chunks < 1 {
		t.Errorf("hop.chunks = %v, want the body's reads", hop["chunks"])
	}

	access = captureAccess(t, f)
	if resp := f.post("/anthropic/v1/messages", `{}`, "Authorization", "Bearer nope"); resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.Code)
	}
	line = jsonLine(t, access)
	stages = group(t, line, "gateway")
	if _, ok := stages["auth"]; !ok {
		t.Errorf("gateway = %v, want auth, where the request was refused", stages)
	}
	for _, past := range []string{"quota", "respond"} {
		if _, present := stages[past]; present {
			t.Errorf("gateway = %v, has %s past the refusal", stages, past)
		}
	}
	if _, present := line["hop"]; present {
		t.Errorf("hop = %v on a request that dialled nothing", line["hop"])
	}
	if group(t, line, "latency")["upstream_ms"] != float64(0) {
		t.Errorf("latency = %v, want no upstream share", line["latency"])
	}
}

// An attempt the client never saw still says where its time went, without `ttfb_ms` or `respond`:
// nothing of it reached the client. The request's own line charges that attempt's time to `retry`.
func TestAnAttemptLineHasItsOwnTiming(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": http.StatusTooManyRequests}, "A", "B")
	// Slow the vendor down so the losing attempt's time is visible. Set before any request is
	// served, which is the only time the server's handler may change.
	f.engine.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		vendor.handler(w, r)
	})
	access := captureAccess(t, f)
	rotated(t, f, vendor)

	lines := accessLines(t, access)
	if len(lines) != 2 || lines[0]["msg"] != "attempt" || lines[1]["msg"] != "access" {
		t.Fatalf("lines = %v, want the held attempt's then the request's", lines)
	}
	attempt := lines[0]
	if wait, _ := group(t, attempt, "hop")["wait"].(float64); wait < 20 {
		t.Errorf("attempt hop.wait = %v, want the vendor's 20ms", wait)
	}
	if _, present := group(t, attempt, "gateway")["respond"]; present {
		t.Error("attempt gateway.respond present for an answer the client never received")
	}
	if _, present := group(t, attempt, "latency")["ttfb_ms"]; present {
		t.Error("attempt latency.ttfb_ms present for an answer the client never received")
	}
	request := lines[1]
	if retry, _ := group(t, request, "gateway")["retry"].(float64); retry < 20 {
		t.Errorf("gateway.retry = %v, want the losing attempt charged to it", retry)
	}
	if _, ok := group(t, request, "gateway")["respond"].(float64); !ok {
		t.Error("gateway.respond missing on the answer the client received")
	}
}

// A line the gateway answered itself has a split with no upstream share, nothing named under
// gateway but other, and no hop group.
func TestAGatewayAnsweredLineHasNoHop(t *testing.T) {
	f := newFixture(t, jsonEngine(`{}`))
	access := accessFixture(t, f)
	r := httptest.NewRequest(http.MethodGet, "/openai/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	f.handler.ServeHTTP(httptest.NewRecorder(), r)

	line := accessLines(t, access)[0]
	if latency := group(t, line, "latency"); latency["upstream_ms"] != float64(0) || latency["total_ms"].(float64) < latency["gateway_ms"].(float64) {
		t.Errorf("latency = %v", latency)
	}
	if stages := group(t, line, "gateway"); len(stages) != 1 || stages["other"] == nil {
		t.Errorf("gateway = %v, want only other", stages)
	}
	if _, present := line["hop"]; present {
		t.Errorf("hop = %v on a request the gateway answered", line["hop"])
	}
}

// A line the listener answered outside the chain — a health probe — ran no stage and dialled
// nothing, so it says nothing of where the time went: no groups rather than zeros, every 15s.
func TestAProbeLineHasNoTiming(t *testing.T) {
	f := newFixture(t, jsonEngine(`{}`))
	access := &bytes.Buffer{}
	listener, err := newServer(t, f.store, config.Config{}, 0, func(s *Services) {
		s.Access = slog.New(slog.NewJSONHandler(access, nil))
	}).Handlers(config.Config{}, nil)
	if err != nil {
		t.Fatalf("Handlers: %v", err)
	}
	listener.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	line := jsonLine(t, access)
	for _, group := range []string{"latency", "gateway", "hop"} {
		if _, present := line[group]; present {
			t.Errorf("%s = %v on a probe that ran no stage", group, line[group])
		}
	}
}
