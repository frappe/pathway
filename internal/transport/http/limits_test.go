package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/phot0n/pathway/internal/config"
	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository/memory"
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
// in the window, counts nothing for the refusal, and admits again when the window turns.
func TestARequestLimitRefusesPastItsCeiling(t *testing.T) {
	clock := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	f := limitedFixture(t, "requests:1m:2", &clock)

	for i := 0; i < 2; i++ {
		if resp := f.post("/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
			t.Fatalf("request %d = %d: %s", i+1, resp.Code, resp.Body)
		}
	}
	*f.seen = engineRecord{}
	metered := f.store.Usage["abc123"]["request_count"]

	resp := f.post("/v1/chat/completions", limitedBody)
	if resp.Code != http.StatusTooManyRequests || resp.Header().Get("Retry-After") != "30" {
		t.Fatalf("third = %d, Retry-After %q; want 429, 30", resp.Code, resp.Header().Get("Retry-After"))
	}
	if message, errorType := refusal(t, resp.Body.Bytes()); message != "rate limit exceeded: 2 requests per 1m" || errorType != "rate_limit_error" {
		t.Errorf("error = %q, %q", message, errorType)
	}
	if f.seen.path != "" || f.store.Usage["abc123"]["request_count"] != metered {
		t.Error("a refused request reached the engine or was metered")
	}
	for key, count := range f.store.Limits {
		if count != 2 {
			t.Errorf("%s = %d, want 2: a refusal counts nothing", key, count)
		}
	}

	clock = clock.Add(30 * time.Second)
	if resp := f.post("/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("next minute = %d, want 200", resp.Code)
	}
}

// Tokens are known only after the answer: the request that crosses the limit completes, and the
// next is refused. The key is not prepaid — a limit is the only gate a free team has.
func TestATokenLimitRefusesOnceADebitCrossesIt(t *testing.T) {
	clock := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	f := limitedFixture(t, "total_tokens:1h:200", &clock)

	for i := 0; i < 2; i++ {
		if resp := f.post("/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
			t.Fatalf("request %d = %d: %s", i+1, resp.Code, resp.Body)
		}
	}
	resp := f.post("/v1/chat/completions", limitedBody)
	if resp.Code != http.StatusTooManyRequests || resp.Header().Get("Retry-After") != "3570" {
		t.Fatalf("third = %d, Retry-After %q; want 429, 3570", resp.Code, resp.Header().Get("Retry-After"))
	}
	if message, _ := refusal(t, resp.Body.Bytes()); message != "rate limit exceeded: 200 tokens per 1h" {
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

	if resp := f.post("/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("first = %d", resp.Code)
	}
	resp := f.post("/v1/chat/completions", limitedBody)
	if message, _ := refusal(t, resp.Body.Bytes()); resp.Code != http.StatusTooManyRequests || message != "rate limit exceeded: 1 requests per 1d" {
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
	if resp := f.post("/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("unlimited = %d, want 200 with the limit store down", resp.Code)
	}

	f.withKey(func(k *domain.KeyRecord) {
		k.Limits = []domain.Limit{{Metric: domain.LimitRequests, Window: "1m", Value: 5}}
	})
	resp := f.post("/v1/chat/completions", limitedBody)
	if message, _ := refusal(t, resp.Body.Bytes()); resp.Code != http.StatusServiceUnavailable || message != "limit store error" {
		t.Fatalf("limited = %d, %q; want 503 limit store error", resp.Code, message)
	}
}

// The keys section of a state push carries limits; a blank one clears them; one this binary cannot
// read is refused by name and nothing is stored.
func TestAKeysPushCarriesLimits(t *testing.T) {
	store := memory.New()
	handler := adminFixture(t, store)
	push := func(limits string) (int, string) {
		body := fmt.Sprintf(`{"keys": {"buckets": {"%s": {"hash": "kh", "records": [
			{"key_hash": "aa", "prefix": "K-1", "team": "T-1", "status": "active", "group": "acme", "limits": "%s"}]}}}}`, domain.BucketOf("aa"), limits)
		w := adminCall(t, handler, http.MethodPost, "/grove-admin/state", body)
		return w.Code, w.Body.String()
	}

	if code, body := push("requests:1m:200,total_tokens:1h:50000"); code != http.StatusOK {
		t.Fatalf("push = %d: %s", code, body)
	}
	if limits := store.Keys["aa"].Limits; len(limits) != 2 || limits[1] != (domain.Limit{Metric: "total_tokens", Window: "1h", Value: 50000}) {
		t.Fatalf("limits = %+v", limits)
	}
	if code, body := push("requests:1w:200"); code != http.StatusBadRequest || body != "bad body: key K-1: limit \"requests:1w:200\": unknown metric or window, or a value not above zero\n" {
		t.Fatalf("unreadable limit = %d: %q", code, body)
	}
	if len(store.Keys["aa"].Limits) != 2 {
		t.Error("a refused push moved the stored limits")
	}
	if code, _ := push(""); code != http.StatusOK || len(store.Keys["aa"].Limits) != 0 {
		t.Errorf("blank push = %d, limits = %+v; want cleared", code, store.Keys["aa"].Limits)
	}
}
