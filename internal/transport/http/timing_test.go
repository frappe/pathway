package http

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/frappe/pathway/internal/transport/http/middleware"
)

// Where a request's time went is on its access line: every stage's own time, down to the proxy,
// and the hop's legs. A refusal above the proxy has the stages up to the one that refused, and no
// hop.
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
	stage, _ := line["stage"].(map[string]any)
	for _, name := range append(append([]string{}, middleware.GatewayChain...), "handler") {
		if seconds, ok := stage[name].(float64); !ok || seconds < 0 {
			t.Errorf("stage.%s = %v, want a duration", name, stage[name])
		}
	}
	if handler, _ := stage["handler"].(float64); handler < 0.02 {
		t.Errorf("stage.handler = %v, want the upstream's 20ms", handler)
	}
	hop, _ := line["hop"].(map[string]any)
	if hop["reused"] != false {
		t.Errorf("hop.reused = %v on the first dial to a host", hop["reused"])
	}
	for _, leg := range []string{"connect", "send", "wait", "respond", "stream"} {
		if _, ok := hop[leg].(float64); !ok {
			t.Errorf("hop.%s = %v, want a duration", leg, hop[leg])
		}
	}
	if wait, _ := hop["wait"].(float64); wait < 0.02 {
		t.Errorf("hop.wait = %v, want the upstream's 20ms", wait)
	}

	access = captureAccess(t, f)
	if resp := f.post("/anthropic/v1/messages", `{}`, "Authorization", "Bearer nope"); resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.Code)
	}
	line = jsonLine(t, access)
	stage, _ = line["stage"].(map[string]any)
	if _, ok := stage["auth"]; !ok {
		t.Errorf("stage = %v, want auth, where the request was refused", stage)
	}
	if _, past := stage["quota"]; past {
		t.Errorf("stage = %v, has stages past the refusal", stage)
	}
	if _, present := line["hop"]; present {
		t.Errorf("hop = %v on a request that dialled nothing", line["hop"])
	}
}

// An attempt the client never saw still says where its time went, without `respond`: nothing of it
// reached the client. The request's own line charges that attempt's time to `retry`.
func TestAnAttemptLineHasItsOwnHop(t *testing.T) {
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
	hop, _ := lines[0]["hop"].(map[string]any)
	if wait, _ := hop["wait"].(float64); wait < 0.02 {
		t.Errorf("attempt hop.wait = %v, want the vendor's 20ms", hop["wait"])
	}
	if _, present := hop["respond"]; present {
		t.Errorf("attempt hop = %v, has respond for an answer the client never received", hop)
	}
	stage, _ := lines[1]["stage"].(map[string]any)
	if retry, _ := stage["retry"].(float64); retry < 0.02 {
		t.Errorf("stage.retry = %v, want the losing attempt charged to it", stage["retry"])
	}
	if hop, _ := lines[1]["hop"].(map[string]any); hop["respond"] == nil {
		t.Errorf("request hop = %v, want respond on the answer the client received", hop)
	}
}

// A line the gateway answered itself has the stages it passed, and no hop.
func TestAGatewayAnsweredLineHasNoHop(t *testing.T) {
	f := newFixture(t, jsonEngine(`{}`))
	access := accessFixture(t, f)
	r := httptest.NewRequest(http.MethodGet, "/openai/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	f.handler.ServeHTTP(httptest.NewRecorder(), r)

	line := accessLines(t, access)[0]
	stage, _ := line["stage"].(map[string]any)
	if _, ok := stage["drain"]; !ok || len(stage) != 2 {
		t.Errorf("stage = %v, want drain and handler", stage)
	}
	if _, present := line["hop"]; present {
		t.Errorf("hop = %v on a request the gateway answered", line["hop"])
	}
}
