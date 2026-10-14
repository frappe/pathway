package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository/memory"
)

// keyedVendor answers by the key it was dialled with: the status in `answers` for that
// Bearer, 200 with usage for one it does not list. It remembers the keys of the last request's
// dials, in order; the fixture's `seen` holds the last dial whole.
type keyedVendor struct {
	answers map[string]int
	// bodies is what a refusal says, per key, when the test cares; else "refused <key>".
	bodies map[string]string
	keys   []string
}

func (v *keyedVendor) handler(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	v.keys = append(v.keys, key)
	if status, refused := v.answers[key]; refused {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", key)
		w.WriteHeader(status)
		body, said := v.bodies[key]
		if !said {
			body = `{"error":{"message":"refused ` + key + `"}}`
		}
		_, _ = w.Write([]byte(body))
		return
	}
	jsonEngine(`{"id":"chatcmpl-1",`+usageObject+`}`)(w, r)
}

// keyedFixture points one vendor model at a route carrying `keys`, each answering per `answers`.
func keyedFixture(t *testing.T, answers map[string]int, keys ...string) (*fixture, *keyedVendor) {
	t.Helper()
	vendor := &keyedVendor{answers: answers}
	f := newFixture(t, vendor.handler)
	f.store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b,deepseek/chat")}
	var credentials []domain.Credential
	for _, key := range keys {
		credentials = append(credentials, domain.Credential{ID: "id-" + key, Secret: key})
	}
	f.store.Routes["deepseek/chat"] = []domain.Route{{
		EngineURL: f.engine.URL, Credentials: credentials, KeySelection: "round_robin", Healthy: true,
		Vendor: "deepseek", Kind: "provider", Dialect: "openai",
		UpstreamModel: "deepseek-chat",
	}}
	return f, vendor
}

const chatBody = `{"model":"deepseek/chat","messages":[{"role":"user","content":"hi"}]}`

// chat posts one request and hands back the vendor's view of it.
func (v *keyedVendor) chat(f *fixture) *httptest.ResponseRecorder {
	v.keys = nil
	return f.post("/v1/chat/completions", chatBody)
}

// rotated posts one request on a fresh fixture: the turn starts at the first key, the refused one,
// so the request rotates and must still succeed.
func rotated(t *testing.T, f *fixture, vendor *keyedVendor) *httptest.ResponseRecorder {
	t.Helper()
	resp := vendor.chat(f)
	if resp.Code != http.StatusOK || len(vendor.keys) < 2 {
		t.Fatalf("status = %d, body = %s, keys = %v", resp.Code, resp.Body, vendor.keys)
	}
	return resp
}

func (f *fixture) keyStats(id string) domain.KeyStats { return f.store.KeyStats["id-"+id] }

// keyCounts is a key's stats by field name, so a test can name the class it expects.
func (f *fixture) keyCounts(t *testing.T, id string) map[string]int64 {
	t.Helper()
	raw, _ := json.Marshal(f.keyStats(id))
	counts := map[string]int64{}
	if err := json.Unmarshal(raw, &counts); err != nil {
		t.Fatalf("stats %s: %v", id, err)
	}
	return counts
}

// Requests take the keys in turn, so a vendor's per-key limits are drawn on evenly.
func TestRequestsTakeTheKeysInTurn(t *testing.T) {
	f, vendor := keyedFixture(t, nil, "A", "B", "C")
	var dialled []string
	for range 4 {
		if resp := vendor.chat(f); resp.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.Code, resp.Body)
		}
		dialled = append(dialled, vendor.keys...)
	}
	if got := strings.Join(dialled, ""); got != "ABCA" {
		t.Errorf("keys dialled = %q", got)
	}
}

// A key the vendor rate-limits is swapped for another, and the client never learns there were two
// attempts: one bill, the answer from the key that worked, none of the loser's headers.
func TestARateLimitedKeyIsSwappedForAnother(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": 429}, "A", "B")
	resp := rotated(t, f, vendor)

	if strings.Join(vendor.keys, ",") != "A,B" {
		t.Errorf("keys dialled = %v", vendor.keys)
	}
	if resp.Header().Get("Retry-After") != "" {
		t.Errorf("the loser's headers reached the client: %v", resp.Header())
	}
	if a, b := f.keyStats("A"), f.keyStats("B"); a.RateLimited != a.Requests || a.LastRateLimited == 0 || b.OK != b.Requests || b.OK == 0 {
		t.Errorf("stats A=%+v B=%+v", a, b)
	}
}

// The second attempt carries the client's body, transformed afresh — not the drained reader the
// first attempt left behind.
func TestTheWinnerGetsTheBodyAgain(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": 429}, "A", "B")
	rotated(t, f, vendor)

	var body map[string]any
	if err := json.Unmarshal(f.seen.body, &body); err != nil {
		t.Fatalf("winner's body %q: %v", f.seen.body, err)
	}
	if body["model"] != "deepseek-chat" || body["messages"] == nil {
		t.Errorf("winner's body = %v, want the client's, with the upstream id", body)
	}
	if got := f.store.UsageOf("abc123")["request_count"]; got == 0 {
		t.Errorf("request_count = %d", got)
	}
}

// Every attempt is billed once: however many keys a request walked, it is one request.
func TestARotatedRequestIsBilledOnce(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": 429}, "A", "B")
	rotated(t, f, vendor)
	runs := f.store.UsageOf("abc123")["request_count"]
	// One post, two dials: the rotated request must not have been billed as two.
	var attempts int64
	for id := range f.store.KeyStats {
		attempts += f.store.KeyStats[id].Requests
	}
	if attempts <= runs {
		t.Fatalf("attempts = %d, requests = %d: the rotation never happened", attempts, runs)
	}
	if a, b := f.keyStats("A"), f.keyStats("B"); a.Requests+b.Requests != runs+a.RateLimited {
		t.Errorf("A=%+v B=%+v requests=%d: a rotation was billed twice", a, b, runs)
	}
}

// A key the vendor refuses outright is dead for the rest of the request: never dialled again,
// however the rate-limited ones are re-opened.
func TestARefusedKeyIsNeverTriedAgain(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": 401, "B": 429}, "A", "B")
	resp := vendor.chat(f)

	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the 429 relayed once every key is spent; body = %s", resp.Code, resp.Body)
	}
	dialled := map[string]int{}
	for _, key := range vendor.keys {
		dialled[key]++
	}
	if dialled["A"] != 1 {
		t.Errorf("A dialled %d times (%v), want once: a 401 retires the key", dialled["A"], vendor.keys)
	}
	if dialled["B"] != 2 {
		t.Errorf("B dialled %d times (%v), want twice: once, then again after the ring re-opened", dialled["B"], vendor.keys)
	}
	if a := f.keyStats("A"); a.Rejected != 1 {
		t.Errorf("stats A = %+v", a)
	}
}

// Every key rate-limited: each is tried once, the ring opens once more, and the second refusal is
// the client's answer — body and headers as the vendor sent them.
func TestEveryKeyRateLimitedRelaysTheLastAnswer(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": 429, "B": 429}, "A", "B")
	resp := vendor.chat(f)

	if resp.Code != http.StatusTooManyRequests || !strings.Contains(resp.Body.String(), "refused") {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body)
	}
	if resp.Header().Get("Retry-After") == "" {
		t.Errorf("the relayed answer lost the vendor's headers: %v", resp.Header())
	}
	if len(vendor.keys) != 4 {
		t.Errorf("keys dialled = %v, want each twice", vendor.keys)
	}
	if got := f.store.UsageOf("abc123")["request_count"]; got != 0 {
		t.Errorf("request_count = %d, want 0: nobody served the caller", got)
	}
}

// One key has nowhere to go: its refusal is relayed at once, and still counted.
func TestASingleKeyIsNotRetried(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": 429}, "A")
	resp := vendor.chat(f)

	if resp.Code != http.StatusTooManyRequests || len(vendor.keys) != 1 {
		t.Errorf("status = %d, keys dialled = %v", resp.Code, vendor.keys)
	}
	if a := f.keyStats("A"); a.Requests != 1 || a.RateLimited != 1 {
		t.Errorf("stats A = %+v", a)
	}
}

// An engine's 429 is its own: no ring to walk, and nothing counted — an engine's one credential
// has no id.
func TestAnEngineRefusalIsNotAKeyFailure(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
	resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","messages":[]}`)

	if resp.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d", resp.Code)
	}
	if len(f.store.KeyStats) != 0 {
		t.Errorf("an engine was counted as a provider key: %v", f.store.KeyStats)
	}
}

// A client that left is not retried on: the next key would answer nobody.
func TestAGoneClientIsNotRetried(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": 429, "B": 429}, "A", "B")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+secret)
	r.Header.Set("Content-Type", "application/json")
	f.handler.ServeHTTP(httptest.NewRecorder(), r)

	if len(vendor.keys) > 1 {
		t.Errorf("keys dialled = %v, want no second attempt for a client that hung up", vendor.keys)
	}
}

// The control plane takes the counts by id; a key never dialled is zeros, not absent, and a
// second read starts from zero.
func TestProviderKeyStatsReadBack(t *testing.T) {
	store := memory.New()
	taken := domain.KeyStats{Requests: 3, OK: 2, RateLimited: 1, LastUsed: 100, LastRateLimited: 90}
	store.KeyStats["k1"] = taken
	handler := adminFixture(t, store)

	w := adminCall(t, handler, http.MethodGet, "/grove-admin/provider-keys?ids=k1,k2", "")
	var stats map[string]domain.KeyStats
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &stats) != nil {
		t.Fatalf("GET = %d: %s", w.Code, w.Body)
	}
	if len(stats) != 2 || stats["k1"] != taken || stats["k2"] != (domain.KeyStats{}) {
		t.Errorf("stats = %+v", stats)
	}

	w = adminCall(t, handler, http.MethodGet, "/grove-admin/provider-keys?ids=k1", "")
	if json.Unmarshal(w.Body.Bytes(), &stats) != nil || stats["k1"] != (domain.KeyStats{}) {
		t.Errorf("second read = %s, want zeros", w.Body)
	}
}

// A credential turned away on every key — unauthorised, unpaid or forbidden — is the gateway's
// answer, in its own words: the vendor's text and headers never reach the client, the access
// line says which refusal it was, and the key's stats count it under that class.
func TestEveryKeyRefusedIsTheGatewaysAnswer(t *testing.T) {
	for status, want := range map[int]struct{ class, message string }{
		401: {"unauthorized", "upstream credential rejected"},
		402: {"unpaid", "upstream account unpaid"},
		403: {"forbidden", "upstream credential lacks permission"},
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f, vendor := keyedFixture(t, map[string]int{"A": status, "B": status}, "A", "B")
			access := captureAccess(t, f)
			resp := vendor.chat(f)

			if resp.Code != http.StatusBadGateway || !strings.Contains(resp.Body.String(), want.message) {
				t.Fatalf("status = %d, body = %s", resp.Code, resp.Body)
			}
			if strings.Contains(resp.Body.String(), "refused") || resp.Header().Get("Retry-After") != "" {
				t.Errorf("the vendor's answer reached the client: %s %v", resp.Body, resp.Header())
			}
			if len(vendor.keys) != 2 {
				t.Errorf("keys dialled = %v, want each once", vendor.keys)
			}
			for _, key := range []string{"A", "B"} {
				if counts := f.keyCounts(t, key); counts[want.class] != 1 || counts["rejected"] != 1 || counts["ok"] != 0 {
					t.Errorf("stats %s = %v, want one %s", key, counts, want.class)
				}
			}
			lines := accessLines(t, access)
			last := lines[len(lines)-1]
			if last["status"] != 502. || last["upstream_status"] != float64(status) || last["reason"] != "credential "+want.class {
				t.Errorf("access line = %v", last)
			}
		})
	}
}

// One key has nowhere to go, and its refusal is still not the client's to read.
func TestASingleKeyRefusedIsTheGatewaysAnswer(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": 402}, "A")
	resp := vendor.chat(f)

	if resp.Code != http.StatusBadGateway || strings.Contains(resp.Body.String(), "refused") || len(vendor.keys) != 1 {
		t.Errorf("status = %d, body = %s, keys dialled = %v", resp.Code, resp.Body, vendor.keys)
	}
	if a := f.keyStats("A"); a.Requests != 1 || a.Unpaid != 1 || a.Rejected != 1 {
		t.Errorf("stats A = %+v", a)
	}
}

// An engine refusing our key is as little the caller's fault as a vendor doing it: the same
// answer, and nothing counted — an engine's one credential has no id.
func TestAnEngineRefusingItsKeyIsTheGatewaysAnswer(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad api key"}`))
	})
	resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","messages":[]}`)

	if resp.Code != http.StatusBadGateway || !strings.Contains(resp.Body.String(), "upstream credential rejected") {
		t.Errorf("status = %d, body = %s", resp.Code, resp.Body)
	}
	if strings.Contains(resp.Body.String(), "bad api key") || len(f.store.KeyStats) != 0 {
		t.Errorf("body = %s, stats = %v", resp.Body, f.store.KeyStats)
	}
}

const (
	quotaBody  = `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","code":"insufficient_quota"}}`
	creditBody = `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}}`
)

// OpenAI reports an exhausted account as a 429: read, it retires the key like a 402 would — the
// next key answers, and the first is counted unpaid, not rate-limited.
func TestAnExhaustedAccountUnderA429RetiresTheKey(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": 429}, "A", "B")
	vendor.bodies = map[string]string{"A": quotaBody}
	access := captureAccess(t, f)
	resp := vendor.chat(f)

	if resp.Code != http.StatusOK || strings.Join(vendor.keys, ",") != "A,B" {
		t.Fatalf("status = %d, keys dialled = %v", resp.Code, vendor.keys)
	}
	if a := f.keyStats("A"); a.Unpaid != 1 || a.Rejected != 1 || a.RateLimited != 0 {
		t.Errorf("stats A = %+v", a)
	}
	attempt := accessLines(t, access)[0]
	if attempt["upstream_status"] != 429. || attempt["reason"] != "credential unpaid" || attempt["moved"] != "key" {
		t.Errorf("attempt line = %v", attempt)
	}
}

// Anthropic reports one as a 400. Every key out of credit is the gateway's answer, and the
// vendor's billing page never reaches the client.
func TestAnExhaustedAccountUnderA400IsTheGatewaysAnswer(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": 400, "B": 400}, "A", "B")
	vendor.bodies = map[string]string{"A": creditBody, "B": creditBody}
	resp := vendor.chat(f)

	if resp.Code != http.StatusBadGateway || !strings.Contains(resp.Body.String(), "upstream account unpaid") {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body)
	}
	if strings.Contains(resp.Body.String(), "Billing") || len(vendor.keys) != 2 {
		t.Errorf("body = %s, keys dialled = %v", resp.Body, vendor.keys)
	}
}

// A 400 the request earned is the caller's, as the vendor wrote it: read, not moved, not counted
// against the key.
func TestACallersOwn400IsRelayed(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": 400}, "A", "B")
	vendor.bodies = map[string]string{"A": `{"error":{"message":"messages[0].content is invalid","type":"invalid_request_error"}}`}
	resp := vendor.chat(f)

	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "messages[0]") || len(vendor.keys) != 1 {
		t.Errorf("status = %d, body = %s, keys dialled = %v", resp.Code, resp.Body, vendor.keys)
	}
	if resp.Header().Get("Retry-After") != "A" {
		t.Errorf("the vendor's headers were lost: %v", resp.Header())
	}
	if a := f.keyStats("A"); a.OK != 1 || a.Rejected != 0 {
		t.Errorf("stats A = %+v", a)
	}
}

// A 400 longer than the read cap is decided on its head and still reaches the client whole.
func TestALong400IsRelayedWhole(t *testing.T) {
	f, vendor := keyedFixture(t, map[string]int{"A": 400}, "A")
	long := `{"error":{"message":"` + strings.Repeat("x", 100<<10) + `"}}`
	vendor.bodies = map[string]string{"A": long}
	resp := vendor.chat(f)

	if resp.Code != http.StatusBadRequest || resp.Body.String() != long {
		t.Errorf("status = %d, body length = %d of %d", resp.Code, resp.Body.Len(), len(long))
	}
}
