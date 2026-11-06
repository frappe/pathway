package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frappe/pathway/internal/config"
	"github.com/frappe/pathway/internal/domain"
	"github.com/frappe/pathway/internal/repository/memory"
)

const limitedBody = `{"model":"qwen3-4b","messages":[]}`

// limitedFixture is newFixture with its key under limits, and the windows read off a clock the
// test moves. The engine answers 120 tokens a request.
func limitedFixture(t *testing.T, limits string, clock *time.Time) *fixture {
	t.Helper()
	f := newFixture(t, jsonEngine(`{`+usageObject+`}`))
	parsed, err := domain.ParseLimits(limits)
	if err != nil {
		t.Fatal(err)
	}
	f.withKey(func(k *domain.KeyRecord) { k.Limits = parsed })
	now := func() time.Time { return *clock }
	f.handler = buildHandler(t, f.store, config.Config{}, 0, func(s *Services) {
		s.Admission.Now, s.Metering.Now = now, now
	})
	return f
}

func refusal(t *testing.T, body []byte) (message, errorType string) {
	t.Helper()
	var parsed struct {
		Error struct{ Message, Type string } `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("body = %s", body)
	}
	return parsed.Error.Message, parsed.Error.Type
}

// A request limit admits its ceiling and no more, refuses before the engine with the seconds left
// in the window, counts nothing for the refusal, and admits again when the window turns. The rows
// on the key record are the team's, so the counter is the team's on this model.
func TestARequestLimitRefusesPastItsCeiling(t *testing.T) {
	clock := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	f := limitedFixture(t, "requests:1m:2", &clock)

	for i := 0; i < 2; i++ {
		if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
			t.Fatalf("request %d = %d: %s", i+1, resp.Code, resp.Body)
		}
	}
	*f.seen = engineRecord{}
	metered := f.store.UsageOf("abc123")["request_count"]

	resp := f.post("/openai/v1/chat/completions", limitedBody)
	if resp.Code != http.StatusTooManyRequests || resp.Header().Get("Retry-After") != "30" {
		t.Fatalf("third = %d, Retry-After %q; want 429, 30", resp.Code, resp.Header().Get("Retry-After"))
	}
	if message, errorType := refusal(t, resp.Body.Bytes()); message != "rate limit exceeded: 2 requests per 1m on qwen3-4b (team)" || errorType != "rate_limit_error" {
		t.Errorf("error = %q, %q", message, errorType)
	}
	if f.seen.path != "" || f.store.UsageOf("abc123")["request_count"] != metered {
		t.Error("a refused request reached the engine or was metered")
	}
	for key, count := range f.store.Limits {
		if count != 2 {
			t.Errorf("%s = %d, want 2: a refusal counts nothing", key, count)
		}
	}

	clock = clock.Add(30 * time.Second)
	if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("next minute = %d, want 200", resp.Code)
	}
}

// Tokens are known only after the answer: the request that crosses the limit completes, and the
// next is refused. The key is far from its cap — the limit is the only gate that fires.
func TestATokenLimitRefusesOnceADebitCrossesIt(t *testing.T) {
	clock := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	f := limitedFixture(t, "total_tokens:1h:200", &clock)

	for i := 0; i < 2; i++ {
		if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
			t.Fatalf("request %d = %d: %s", i+1, resp.Code, resp.Body)
		}
	}
	resp := f.post("/openai/v1/chat/completions", limitedBody)
	if resp.Code != http.StatusTooManyRequests || resp.Header().Get("Retry-After") != "3570" {
		t.Fatalf("third = %d, Retry-After %q; want 429, 3570", resp.Code, resp.Header().Get("Retry-After"))
	}
	if message, _ := refusal(t, resp.Body.Bytes()); message != "rate limit exceeded: 200 tokens per 1h on qwen3-4b (team)" {
		t.Errorf("message = %q", message)
	}
	for key, count := range f.store.Limits {
		if count != 240 {
			t.Errorf("%s = %d, want 240", key, count)
		}
	}
}

// Over two limits at once, the answer names the one that resets last.
func TestTheLimitThatResetsLastIsNamed(t *testing.T) {
	clock := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	f := limitedFixture(t, "requests:1d:1,requests:1m:1", &clock)

	if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("first = %d", resp.Code)
	}
	resp := f.post("/openai/v1/chat/completions", limitedBody)
	if message, _ := refusal(t, resp.Body.Bytes()); resp.Code != http.StatusTooManyRequests || message != "rate limit exceeded: 1 requests per 1d on qwen3-4b (team)" {
		t.Fatalf("second = %d, %q", resp.Code, message)
	}
	if resp.Header().Get("Retry-After") != "43170" {
		t.Errorf("Retry-After = %q, want the seconds to midnight UTC", resp.Header().Get("Retry-After"))
	}
}

// A key with no limits never touches the limit store; one with limits is refused when it is down.
func TestTheLimitStoreIsOnlyReadForALimitedHolder(t *testing.T) {
	clock := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	f := limitedFixture(t, "", &clock)
	f.store.Fail["limits"] = true
	if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("unlimited = %d, want 200 with the limit store down", resp.Code)
	}

	f.withKey(func(k *domain.KeyRecord) {
		k.Limits = []domain.Limit{{Metric: domain.LimitRequests, Window: "1m", Value: 5}}
	})
	resp := f.post("/openai/v1/chat/completions", limitedBody)
	if message, _ := refusal(t, resp.Body.Bytes()); resp.Code != http.StatusServiceUnavailable || message != "limit store error" {
		t.Fatalf("limited = %d, %q; want 503 limit store error", resp.Code, message)
	}
}

// The keys section of a state push carries the team's limits and the key's own; a blank one clears
// them; one this binary cannot read is refused by name and nothing is stored.
func TestAKeysPushCarriesLimits(t *testing.T) {
	store := memory.New()
	handler := adminFixture(t, store)
	push := func(limits, own string) (int, string) {
		body := fmt.Sprintf(`{"keys": {"buckets": {"%s": {"hash": "kh", "records": [
			{"key_hash": "aa", "prefix": "K-1", "team": "T-1", "status": "active", "group": "acme", "limits": "%s", "key_limits": "%s"}]}}}}`, domain.BucketOf("aa"), limits, own)
		w := adminCall(t, handler, http.MethodPost, "/grove-admin/state", body)
		return w.Code, w.Body.String()
	}

	if code, body := push("requests:1m:200,total_tokens:1h:50000:qwen3-4b", "requests:1m:5"); code != http.StatusOK {
		t.Fatalf("push = %d: %s", code, body)
	}
	if limits := store.Keys["aa"].Limits; len(limits) != 2 || limits[1] != (domain.Limit{Metric: "total_tokens", Window: "1h", Value: 50000, Model: "qwen3-4b"}) {
		t.Fatalf("limits = %+v", limits)
	}
	if own := store.Keys["aa"].KeyLimits; len(own) != 1 || own[0].Value != 5 {
		t.Fatalf("key_limits = %+v", own)
	}
	if code, body := push("requests:1w:200", ""); code != http.StatusBadRequest || body != "bad body: key K-1: limit \"requests:1w:200\": unknown metric or window, or a value not above zero\n" {
		t.Fatalf("unreadable limit = %d: %q", code, body)
	}
	if code, body := push("", "requests:1m:5:"); code != http.StatusBadRequest || body != "bad body: key K-1: limit \"requests:1m:5:\": want metric:window:value[:model]\n" {
		t.Fatalf("unreadable key limit = %d: %q", code, body)
	}
	if len(store.Keys["aa"].Limits) != 2 || len(store.Keys["aa"].KeyLimits) != 1 {
		t.Error("a refused push moved the stored limits")
	}
	if code, _ := push("", ""); code != http.StatusOK || len(store.Keys["aa"].Limits) != 0 || len(store.Keys["aa"].KeyLimits) != 0 {
		t.Errorf("blank push = %d, limits = %+v / %+v; want cleared", code, store.Keys["aa"].Limits, store.Keys["aa"].KeyLimits)
	}
}

// The key's own rows are a second gate under the team's: a row naming another model binds nothing
// here, a blank one does, and the refusal says whose it was. The team's counter keeps its own count.
func TestAKeysOwnLimitSitsUnderTheTeams(t *testing.T) {
	clock := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	f := limitedFixture(t, "requests:1m:5", &clock)
	own := func(list string) {
		f.withKey(func(k *domain.KeyRecord) { k.KeyLimits, _ = domain.ParseLimits(list) })
	}

	own("requests:1m:1:other-model")
	for i := 0; i < 2; i++ {
		if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
			t.Fatalf("request %d under a row for another model = %d: %s", i+1, resp.Code, resp.Body)
		}
	}
	own("requests:1m:1")
	if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("first under the key's own row = %d: %s", resp.Code, resp.Body)
	}
	resp := f.post("/openai/v1/chat/completions", limitedBody)
	if message, _ := refusal(t, resp.Body.Bytes()); resp.Code != http.StatusTooManyRequests || message != "rate limit exceeded: 1 requests per 1m on qwen3-4b (key)" {
		t.Fatalf("second = %d, %q; want 429 from the key's own row", resp.Code, message)
	}
	counts := map[string]int64{}
	for counter, count := range f.store.Limits {
		counts[counter[:strings.Index(counter, ":")]] = count
	}
	if counts["team"] != 3 || counts["key"] != 1 {
		t.Errorf("counters = %v, want team 3 and key 1", f.store.Limits)
	}
}

// Every key of a team draws on the team's one window per model.
func TestKeysOfOneTeamShareTheWindow(t *testing.T) {
	clock := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	f := limitedFixture(t, "requests:1m:2", &clock)
	twin := f.store.Keys[domain.SHA256Hex(secret)]
	twin.KeyPrefix = "def456"
	f.store.Keys[domain.SHA256Hex("twin-secret")] = twin

	if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("first key = %d", resp.Code)
	}
	if resp := f.post("/openai/v1/chat/completions", limitedBody, "Authorization", "Bearer twin-secret"); resp.Code != http.StatusOK {
		t.Fatalf("second key = %d", resp.Code)
	}
	resp := f.post("/openai/v1/chat/completions", limitedBody)
	if message, _ := refusal(t, resp.Body.Bytes()); resp.Code != http.StatusTooManyRequests || message != "rate limit exceeded: 2 requests per 1m on qwen3-4b (team)" {
		t.Fatalf("third across the two = %d, %q; want the team's 429", resp.Code, message)
	}
}

// A key with no limits of its own runs under its team's; one with its own keeps them whatever the
// team says.
func TestTeamLimitsStandInWhereTheKeyNamesNone(t *testing.T) {
	clock := time.Date(2026, 10, 13, 12, 0, 30, 0, time.UTC)
	teamLimit, _ := domain.ParseLimits("requests:1m:1")

	f := limitedFixture(t, "", &clock)
	f.store.Teams["test-team"] = domain.TeamRecord{Limits: teamLimit}
	if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("first = %d: %s", resp.Code, resp.Body)
	}
	if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusTooManyRequests {
		t.Fatalf("second under the team's limit = %d, want 429: %s", resp.Code, resp.Body)
	}

	own := limitedFixture(t, "requests:1m:2", &clock)
	own.store.Teams["test-team"] = domain.TeamRecord{Limits: teamLimit}
	for i := 0; i < 2; i++ {
		if resp := own.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
			t.Fatalf("request %d under the key's own limit = %d: %s", i+1, resp.Code, resp.Body)
		}
	}
	if resp := own.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusTooManyRequests {
		t.Fatalf("third = %d, want 429 from the key's own limit: %s", resp.Code, resp.Body)
	}
}
