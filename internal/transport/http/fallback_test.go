package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phot0n/pathway/internal/config"
	"github.com/phot0n/pathway/internal/domain"
)

// backupEngine is a second model's engine: it keeps the bodies it was sent.
type backupEngine struct {
	url    string
	bodies [][]byte
}

// addBackup serves the model "backup" from an engine of its own and grants it to the caller.
func addBackup(t *testing.T, f *fixture, handler http.HandlerFunc) *backupEngine {
	t.Helper()
	return addModel(t, f, "backup", handler)
}

// addModel serves one more model from an engine of its own and grants it to the caller.
func addModel(t *testing.T, f *fixture, model string, handler http.HandlerFunc) *backupEngine {
	t.Helper()
	backup := &backupEngine{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		backup.bodies = append(backup.bodies, body)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	backup.url = server.URL
	f.store.Groups["acme"].Models[model] = true
	f.store.Routes[model] = []domain.Route{{
		EngineURL: server.URL, InternalKey: "backup-key", Healthy: true,
		Deployment: "MD-00008", Server: "INF-2", Kind: "direct",
		Pricing: &domain.Pricing{ID: "mp-backup", Rates: map[string]int64{"prompt_tokens": 3e9}, Counters: counters(t)},
	}}
	return backup
}

// fallbackFixture is a primary engine answering `primary` and a healthy backup.
func fallbackFixture(t *testing.T, primary http.HandlerFunc) (*fixture, *backupEngine) {
	t.Helper()
	f := newFixture(t, primary)
	f.store.Routes["qwen3-4b"][0].Pricing = &domain.Pricing{
		ID: "mp-primary", Rates: map[string]int64{"prompt_tokens": 3e9}, Counters: counters(t),
	}
	return f, addBackup(t, f, jsonEngine(`{"id":"chatcmpl-2","model":"backup",`+usageObject+`}`))
}

func failing(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Primary", "1")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"primary said no"}}`)
	}
}

const fallbackBody = `{"model":"qwen3-4b","messages":[],"fallbacks":["backup"]}`

// captureAccess rebuilds the fixture's handler with its access log kept.
func captureAccess(t *testing.T, f *fixture) *bytes.Buffer {
	t.Helper()
	access := &bytes.Buffer{}
	f.handler = buildHandler(t, f.store, config.Config{}, 0, func(s *Services) {
		s.Access = slog.New(slog.NewJSONHandler(access, nil))
	})
	return access
}

// pricedUnder reports whether any usage was tagged with this pricing id.
func pricedUnder(usage map[string]int64, id string) bool {
	for field := range usage {
		if strings.Contains(field, ":"+id+":") {
			return true
		}
	}
	return false
}

// The model asked for is down, so the next one the caller named answers — and everything about the
// request follows it: the body's model, the usage, the price, the freed slots, the access line.
func TestAFailedModelFallsBackToTheNext(t *testing.T) {
	f, backup := fallbackFixture(t, failing(http.StatusBadGateway))
	access := captureAccess(t, f)

	resp := f.post("/v1/chat/completions", fallbackBody)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"model":"backup"`) {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body)
	}
	if resp.Header().Get("X-Primary") != "" || resp.Header().Get("X-Grove-Fallback") != "backup" {
		t.Errorf("headers = %v, want none of the loser's and the fallback named", resp.Header())
	}
	var sent map[string]any
	if len(backup.bodies) != 1 || json.Unmarshal(backup.bodies[0], &sent) != nil {
		t.Fatalf("backup was sent %q", backup.bodies)
	}
	if _, leaked := sent["fallbacks"]; leaked || sent["model"] != "backup" || sent["messages"] == nil {
		t.Errorf("backup's body = %v, want the client's, naming backup, without fallbacks", sent)
	}

	usage := f.store.Usage["abc123"]
	for field, want := range map[string]int64{
		"request_count": 1, "m:request_count:backup": 1, "m:request_count:qwen3-4b": 0, "m:total_tokens:backup": 120,
	} {
		if usage[field] != want {
			t.Errorf("usage[%s] = %d, want %d", field, usage[field], want)
		}
	}
	if !pricedUnder(usage, "mp-backup") || pricedUnder(usage, "mp-primary") {
		t.Errorf("usage = %v, want the backup's pricing only", usage)
	}

	primaryURL := f.store.Routes["qwen3-4b"][0].EngineURL
	if f.store.Failures[primaryURL] != 1 {
		t.Errorf("primary failures = %d, want 1", f.store.Failures[primaryURL])
	}
	if len(f.store.InFlight[primaryURL]) != 0 || len(f.store.InFlight[backup.url]) != 0 {
		t.Errorf("slots left claimed: %v", f.store.InFlight)
	}
	lines := accessLines(t, access)
	if len(lines) != 2 {
		t.Fatalf("access log = %v, want the held attempt's line and the request's", lines)
	}
	held, line := lines[0], lines[1]
	if held["msg"] != "attempt" || held["rid"] != line["rid"] || held["attempt"] != float64(1) ||
		held["model"] != "qwen3-4b" || held["upstream"] != primaryURL || held["upstream_status"] != float64(502) ||
		held["moved"] != "model" || held["to"] != "backup" {
		t.Errorf("attempt line = %v", held)
	}
	if line["model"] != "qwen3-4b" || line["fallback"] != "backup" || line["attempts"] != float64(2) {
		t.Errorf("access line model=%v fallback=%v attempts=%v", line["model"], line["fallback"], line["attempts"])
	}
}

// A request that was wrong is wrong for every model: relayed, and the fallback never dialled.
func TestARequestsOwnFaultIsNotMoved(t *testing.T) {
	f, backup := fallbackFixture(t, failing(http.StatusBadRequest))
	resp := f.post("/v1/chat/completions", fallbackBody)

	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "primary said no") {
		t.Errorf("status = %d, body = %s", resp.Code, resp.Body)
	}
	if len(backup.bodies) != 0 {
		t.Errorf("backup was dialled for a 400: %q", backup.bodies)
	}
}

// A model that answers is sent the client's body without the gateway's own field, and a request
// naming no fallbacks is forwarded byte for byte.
func TestFallbacksNeverReachAnUpstream(t *testing.T) {
	f, backup := fallbackFixture(t, jsonEngine(`{`+usageObject+`}`))
	access := captureAccess(t, f)
	resp := f.post("/v1/chat/completions", fallbackBody)
	if resp.Code != http.StatusOK || resp.Header().Get("X-Grove-Fallback") != "" {
		t.Fatalf("status = %d, headers = %v, body = %s", resp.Code, resp.Header(), resp.Body)
	}
	if lines := accessLines(t, access); len(lines) != 1 || lines[0]["fallback"] != "-" {
		t.Errorf("access log = %v, want the request's one line", lines)
	}
	if bytes.Contains(f.seen.body, []byte("fallbacks")) || !bytes.Contains(f.seen.body, []byte(`"qwen3-4b"`)) {
		t.Errorf("primary's body = %s", f.seen.body)
	}
	if len(backup.bodies) != 0 {
		t.Errorf("backup was dialled though the primary answered: %q", backup.bodies)
	}

	plain := `{"model":"qwen3-4b",  "messages":[]}`
	f.post("/v1/chat/completions", plain)
	if string(f.seen.body) != plain {
		t.Errorf("body = %s, want the client's bytes", f.seen.body)
	}
}

// Fallback is what is left when retry has nowhere to go: every key of the model is walked first.
func TestAModelsKeysAreWalkedBeforeTheFallback(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": 429, "B": 429}, "A", "B")
	backup := addBackup(t, f, jsonEngine(`{`+usageObject+`}`))
	resp := f.post("/v1/chat/completions", `{"model":"deepseek/chat","messages":[],"fallbacks":["backup"]}`)

	if resp.Code != http.StatusOK || len(backup.bodies) != 1 {
		t.Fatalf("status = %d, body = %s, backup dials = %d", resp.Code, resp.Body, len(backup.bodies))
	}
	if len(vendor.keys) != 4 {
		t.Errorf("keys dialled = %v, want each twice before the fallback", vendor.keys)
	}
	if got := f.store.Usage["abc123"]["request_count"]; got != 1 {
		t.Errorf("request_count = %d", got)
	}
}

// A model with no healthy server never gets a dial: the fallback is the first attempt.
func TestAModelWithNowhereToGoFallsBackBeforeAnyDial(t *testing.T) {
	f, backup := fallbackFixture(t, failing(http.StatusBadGateway))
	f.store.Routes["qwen3-4b"][0].Healthy = false
	access := captureAccess(t, f)
	resp := f.post("/v1/chat/completions", fallbackBody)

	if resp.Code != http.StatusOK || len(backup.bodies) != 1 || f.seen.path != "" {
		t.Fatalf("status = %d, backup dials = %d, primary saw %q", resp.Code, len(backup.bodies), f.seen.path)
	}
	if !bytes.Contains(backup.bodies[0], []byte(`"model":"backup"`)) {
		t.Errorf("backup's body = %s", backup.bodies[0])
	}
	lines := accessLines(t, access)
	if len(lines) != 2 || resp.Header().Get("X-Grove-Fallback") != "backup" {
		t.Fatalf("access log = %v, headers = %v", lines, resp.Header())
	}
	if refused := lines[0]; refused["msg"] != "attempt" || refused["attempt"] != float64(0) ||
		refused["upstream"] != "-" || refused["reason"] != "no healthy server for model" || refused["to"] != "backup" {
		t.Errorf("attempt line = %v", refused)
	}
	if line := lines[1]; line["fallback"] != "backup" || line["attempts"] != float64(1) {
		t.Errorf("access line fallback=%v attempts=%v", line["fallback"], line["attempts"])
	}
}

// A fallback is granted like any model: one the caller may not use is passed over, as is one with
// no route, and the next in the list answers.
func TestAFallbackTheCallerMayNotUseIsSkipped(t *testing.T) {
	f, backup := fallbackFixture(t, failing(http.StatusBadGateway))
	f.store.Routes["ungranted"] = f.store.Routes["backup"]
	resp := f.post("/v1/chat/completions",
		`{"model":"qwen3-4b","messages":[],"fallbacks":["ungranted","unknown","backup"]}`)

	usage := f.store.Usage["abc123"]
	if resp.Code != http.StatusOK || usage["m:request_count:backup"] != 1 || usage["m:request_count:ungranted"] != 0 {
		t.Fatalf("status = %d, usage = %v", resp.Code, usage)
	}
	if len(backup.bodies) != 1 {
		t.Errorf("backup dials = %d, want 1: the ungranted model shares its engine", len(backup.bodies))
	}

	delete(f.store.Groups["acme"].Models, "backup")
	if resp := f.post("/v1/chat/completions", fallbackBody); resp.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want the primary's 502 when no fallback is the caller's", resp.Code)
	}
}

// Every model failing leaves the last one's answer as the client's, body and headers, billed once.
func TestEveryModelFailingRelaysTheLastAnswer(t *testing.T) {
	f := newFixture(t, failing(http.StatusBadGateway))
	addBackup(t, f, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Backup", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"backup said no"}}`)
	})
	resp := f.post("/v1/chat/completions", fallbackBody)

	if resp.Code != http.StatusServiceUnavailable || !strings.Contains(resp.Body.String(), "backup said no") {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body)
	}
	if resp.Header().Get("X-Backup") == "" || resp.Header().Get("X-Primary") != "" {
		t.Errorf("headers = %v, want the backup's only", resp.Header())
	}
	if got := f.store.Usage["abc123"]["request_count"]; got != 1 {
		t.Errorf("request_count = %d", got)
	}
}

// A vendor fallback is sent the vendor's own id and answers under its Grove name, whatever spelling
// the vendor answered with: the client reads which model served.
func TestAVendorFallbackAnswersUnderItsOwnName(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/e/md1") {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		jsonEngine(`{"model":"deepseek-chat-0324",`+usageObject+`}`)(w, r)
	})
	f.store.Groups["acme"].Models["deepseek/chat"] = true
	f.store.Routes["deepseek/chat"] = []domain.Route{{
		EngineURL: f.engine.URL, InternalKey: "vendor-key", Healthy: true, Deployment: "deepseek",
		Server: "deepseek", Kind: "provider", Dialect: "openai", UpstreamModel: "deepseek-chat",
	}}
	resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","messages":[],"fallbacks":["deepseek/chat"]}`)

	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"model":"deepseek/chat"`) {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body)
	}
	if !bytes.Contains(f.seen.body, []byte(`"model":"deepseek-chat"`)) {
		t.Errorf("vendor's body = %s, want its own id", f.seen.body)
	}
	if got := f.store.Usage["abc123"]["m:request_count:deepseek/chat"]; got != 1 {
		t.Errorf("usage = %v", f.store.Usage["abc123"])
	}
}

// A stream that has begun is the client's already: its break is not another model's to answer.
func TestABegunStreamIsNotMoved(t *testing.T) {
	f, backup := fallbackFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"1\"}\n\n")
		_ = http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler)
	})
	resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","stream":true,"messages":[],"fallbacks":["backup"]}`)

	if resp.Code != http.StatusOK || len(backup.bodies) != 0 {
		t.Errorf("status = %d, backup dials = %d", resp.Code, len(backup.bodies))
	}
}

// A list that is not a few model names is refused before anything is dialled.
func TestAMalformedFallbackListIsRefused(t *testing.T) {
	f, backup := fallbackFixture(t, jsonEngine(`{}`))
	for _, fallbacks := range []string{`"backup"`, `["a","b","c","d"]`, `[""]`, `[1]`} {
		resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","fallbacks":`+fallbacks+`}`)
		if resp.Code != http.StatusBadRequest {
			t.Errorf("fallbacks %s: status = %d, want 400", fallbacks, resp.Code)
		}
	}
	if f.seen.path != "" || len(backup.bodies) != 0 {
		t.Errorf("a refused list was dialled: primary %q, backup %d", f.seen.path, len(backup.bodies))
	}
}

// A client that left is not moved: the next model would answer nobody.
func TestAGoneClientIsNotMoved(t *testing.T) {
	f, backup := fallbackFixture(t, failing(http.StatusBadGateway))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fallbackBody)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+secret)
	r.Header.Set("Content-Type", "application/json")
	f.handler.ServeHTTP(httptest.NewRecorder(), r)

	if len(backup.bodies) != 0 {
		t.Errorf("backup dials = %d, want none for a client that hung up", len(backup.bodies))
	}
}

// brokenAnswer declares more body than it sends, so the connection closes inside the body.
func brokenAnswer(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Length", "100")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "short")
}

// serve posts one request through a real server — where a broken upstream body aborts the handler,
// which a direct call never does — and waits for the handler to finish.
func (f *fixture) serve(body string) *http.Response {
	f.t.Helper()
	gateway := httptest.NewServer(f.handler)
	defer gateway.Close()
	r, _ := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+secret)
	r.Header.Set("Content-Type", "application/json")
	resp, err := gateway.Client().Do(r)
	if err != nil {
		f.t.Fatalf("the request was dropped: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

// A failed answer is held, so its body is read only to be thrown away: one that breaks off must
// not take the request with it, nor leave the fallback's slot claimed.
func TestAHeldAnswerThatBreaksOffStillFallsBack(t *testing.T) {
	f, backup := fallbackFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		brokenAnswer(w, http.StatusBadGateway)
	})
	resp := f.serve(fallbackBody)

	if resp.StatusCode != http.StatusOK || len(backup.bodies) != 1 {
		t.Fatalf("status = %d, backup dials = %d", resp.StatusCode, len(backup.bodies))
	}
	primaryURL := f.store.Routes["qwen3-4b"][0].EngineURL
	if len(f.store.InFlight[primaryURL]) != 0 || len(f.store.InFlight[backup.url]) != 0 {
		t.Errorf("slots left claimed: %v", f.store.InFlight)
	}
}

// The same for a key: a rate limit whose body breaks off still moves the request to the next key.
func TestAHeldRateLimitThatBreaksOffStillRotates(t *testing.T) {
	f, vendor := keyedFixture(t, nil, "A", "B")
	answer := f.engine.Config.Handler
	f.engine.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer A" {
			brokenAnswer(w, http.StatusTooManyRequests)
			return
		}
		answer.ServeHTTP(w, r)
	})
	resp := f.serve(chatBody)

	if resp.StatusCode != http.StatusOK || strings.Join(vendor.keys, ",") != "B" {
		t.Errorf("status = %d, keys answered = %v", resp.StatusCode, vendor.keys)
	}
}

// An ingress reads no body: the header must name the model that is serving, or it would pick a
// replica of the one that just failed.
func TestAFallbackBehindAnIngressIsAskedForByName(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/e/md1") {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		jsonEngine(`{`+usageObject+`}`)(w, r)
	})
	f.store.Groups["acme"].Models["backup"] = true
	f.store.Routes["backup"] = []domain.Route{{
		EngineURL: f.engine.URL + "/ingress", InternalKey: "ingress-key", Healthy: true,
		Deployment: "ING-1", Server: "ING-1", Kind: "ingress",
	}}
	resp := f.post("/v1/chat/completions", fallbackBody)

	if resp.Code != http.StatusOK || f.seen.groveModel != "backup" {
		t.Errorf("status = %d, ingress was asked for %q", resp.Code, f.seen.groveModel)
	}
}

// The payload line stores the fallback's output, so it names the fallback beside the model asked for.
func TestThePayloadLineNamesTheFallback(t *testing.T) {
	f, buf := payloadFixture(t, failing(http.StatusBadGateway), true)
	addBackup(t, f, jsonEngine(`{`+usageObject+`}`))
	f.post("/v1/chat/completions", fallbackBody)

	if line := jsonLine(t, buf); line["model"] != "qwen3-4b" || line["fallback"] != "backup" {
		t.Errorf("payload line model=%v fallback=%v", line["model"], line["fallback"])
	}
}

// A rotated key leaves the same record: which credential was refused, and which took over.
func TestARotatedKeyLeavesAnAttemptLine(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": 429}, "A", "B")
	access := captureAccess(t, f)
	rotated(t, f, vendor)

	lines := accessLines(t, access)
	if len(lines) != 2 {
		t.Fatalf("access log = %v", lines)
	}
	if held := lines[0]; held["msg"] != "attempt" || held["upstream_key"] != "id-A" ||
		held["upstream_status"] != float64(429) || held["moved"] != "key" || held["to"] != "id-B" {
		t.Errorf("attempt line = %v", held)
	}
}

// A fallback is a stand-in: what it refuses, the next may take. Only the model asked for has its
// own refusal relayed at once.
func TestAFallbacksRefusalMovesOn(t *testing.T) {
	f, backup := fallbackFixture(t, failing(http.StatusBadGateway))
	picky := addModel(t, f, "picky", failing(http.StatusBadRequest))
	access := captureAccess(t, f)
	resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","messages":[],"fallbacks":["picky","backup"]}`)

	if resp.Code != http.StatusOK || resp.Header().Get("X-Grove-Fallback") != "backup" {
		t.Fatalf("status = %d, headers = %v, body = %s", resp.Code, resp.Header(), resp.Body)
	}
	if len(picky.bodies) != 1 || len(backup.bodies) != 1 {
		t.Errorf("dials: picky %d, backup %d", len(picky.bodies), len(backup.bodies))
	}
	lines := accessLines(t, access)
	if len(lines) != 3 || lines[0]["upstream_status"] != float64(502) || lines[1]["upstream_status"] != float64(400) ||
		lines[1]["model"] != "picky" || lines[1]["to"] != "backup" {
		t.Errorf("access log = %v", lines)
	}

	// With nothing after it, the refusal is the client's answer, named as the fallback's.
	f.handler = buildHandler(t, f.store, config.Config{}, 0)
	last := f.post("/v1/chat/completions", `{"model":"qwen3-4b","messages":[],"fallbacks":["picky"]}`)
	if last.Code != http.StatusBadRequest || last.Header().Get("X-Grove-Fallback") != "picky" {
		t.Errorf("status = %d, headers = %v", last.Code, last.Header())
	}
}

// A request that moves to another vendor is named afresh for that one: DeepSeek takes
// `max_tokens`, OpenAI only `max_completion_tokens`.
func TestAFallbackIsSentTheFieldsItsVendorTakes(t *testing.T) {
	f := newFixture(t, failing(http.StatusBadGateway))
	vendorRoute := func(vendor, url string) []domain.Route {
		return []domain.Route{{
			EngineURL: url, InternalKey: "vendor-key", Healthy: true, Deployment: vendor,
			Server: vendor, Kind: "provider", Dialect: "openai", UpstreamModel: "upstream-" + vendor,
		}}
	}
	f.store.Groups["acme"].Models["deepseek/chat"] = true
	f.store.Routes["deepseek/chat"] = vendorRoute("deepseek", f.engine.URL)
	luna := addModel(t, f, "openai/luna", jsonEngine(`{`+usageObject+`}`))
	f.store.Routes["openai/luna"] = vendorRoute("openai", luna.url)

	resp := f.post("/v1/chat/completions",
		`{"model":"deepseek/chat","messages":[],"max_tokens":16,"fallbacks":["openai/luna"]}`)
	if resp.Code != http.StatusOK || len(luna.bodies) != 1 {
		t.Fatalf("status = %d, body = %s, luna dials = %d", resp.Code, resp.Body, len(luna.bodies))
	}
	var first, second map[string]any
	if json.Unmarshal(f.seen.body, &first) != nil || json.Unmarshal(luna.bodies[0], &second) != nil {
		t.Fatalf("bodies: %s / %s", f.seen.body, luna.bodies[0])
	}
	if first["max_tokens"] != float64(16) || first["max_completion_tokens"] != nil {
		t.Errorf("deepseek's body = %v, want max_tokens", first)
	}
	if second["max_completion_tokens"] != float64(16) || second["max_tokens"] != nil {
		t.Errorf("openai's body = %v, want max_completion_tokens only", second)
	}
}

// The list is not judged when the request arrives. A fallback that cannot serve is passed over at
// its turn — one with no route on this surface is not even dialled — and the next one answers.
func TestAFallbackThatCannotServeIsPassedOver(t *testing.T) {
	f, backup := fallbackFixture(t, failing(http.StatusBadGateway))
	f.store.Groups["acme"].Models["claude"] = true
	f.store.Routes["claude"] = []domain.Route{{
		EngineURL: backup.url, InternalKey: "k", Healthy: true, Deployment: "anthropic",
		Server: "anthropic", Kind: "provider", Dialect: "anthropic",
	}}
	resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","messages":[],"fallbacks":["claude","backup"]}`)
	if resp.Code != http.StatusOK || resp.Header().Get("X-Grove-Fallback") != "backup" {
		t.Fatalf("other shape: status = %d, fallback = %q, body = %s",
			resp.Code, resp.Header().Get("X-Grove-Fallback"), resp.Body)
	}
	if len(backup.bodies) != 1 {
		t.Errorf("dials = %d, want 1: claude shares the engine and has no route on this surface", len(backup.bodies))
	}

	// A stand-in that takes less than the model asked for is nobody's to refuse up front.
	f, backup = fallbackFixture(t, jsonEngine(`{`+usageObject+`}`))
	f.store.Routes["qwen3-4b"][0].InputModalities = []string{"text", "image"}
	f.store.Routes["backup"][0].InputModalities = []string{"text"}
	if resp = f.post("/v1/chat/completions", fallbackBody); resp.Code != http.StatusOK || len(backup.bodies) != 0 {
		t.Errorf("takes less: status = %d, backup dials = %d, body = %s", resp.Code, len(backup.bodies), resp.Body)
	}
}

// A dial that gave no status still says why on its attempt line.
func TestADeadDialLeavesItsReasonOnTheAttemptLine(t *testing.T) {
	f, _ := fallbackFixture(t, failing(http.StatusBadGateway))
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	f.store.Routes["qwen3-4b"][0].EngineURL = dead.URL
	access := captureAccess(t, f)
	if resp := f.post("/v1/chat/completions", fallbackBody); resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body)
	}

	held := accessLines(t, access)[0]
	if held["msg"] != "attempt" || held["upstream_status"] != float64(0) || held["reason"] != "upstream unavailable" {
		t.Errorf("attempt line = %v", held)
	}
}
