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
	if value, sent := resp.Header()["X-Should-Retry"]; sent {
		t.Errorf("X-Should-Retry = %q on a wait of 30s, want a client left to retry", value)
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
	// 240 is short of twice the ceiling, so the next hour is owed nothing.
	for key, count := range f.store.Limits {
		if count != 240 {
			t.Errorf("%s = %d, want 240", key, count)
		}
	}
	if len(f.store.Limits) != 1 {
		t.Errorf("counters = %v, want this hour's alone", f.store.Limits)
	}
}

// A token limit taken to twice its ceiling spends the next window as well: that whole window is
// refused, the wait named runs to its end, and the window after starts clean. Short of twice, the
// next window is whole.
func TestATokenLimitAtTwiceItsCeilingSpendsTheNextWindow(t *testing.T) {
	clock := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	under := limitedFixture(t, "total_tokens:1m:100", &clock)
	if resp := under.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("first under a ceiling of 100 = %d: %s", resp.Code, resp.Body)
	}
	if resp := under.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusTooManyRequests || resp.Header().Get("Retry-After") != "30" {
		t.Fatalf("after 120 tokens on a ceiling of 100 = %d, Retry-After %q; want 429 to the end of this minute", resp.Code, resp.Header().Get("Retry-After"))
	}
	clock = clock.Add(time.Minute)
	if resp := under.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("the minute after 120 on a ceiling of 100 = %d, want a whole window: %s", resp.Code, resp.Body)
	}

	clock = time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	f := limitedFixture(t, "total_tokens:1m:60", &clock)

	if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("first = %d: %s", resp.Code, resp.Body)
	}
	resp := f.post("/openai/v1/chat/completions", limitedBody)
	if resp.Code != http.StatusTooManyRequests || resp.Header().Get("Retry-After") != "90" || resp.Header().Get("X-Should-Retry") != "" {
		t.Fatalf("after 120 tokens on a ceiling of 60 = %d, Retry-After %q, X-Should-Retry %q; want 429 to the end of the next minute, left to retry",
			resp.Code, resp.Header().Get("Retry-After"), resp.Header().Get("X-Should-Retry"))
	}
	clock = clock.Add(time.Minute)
	resp = f.post("/openai/v1/chat/completions", limitedBody)
	if resp.Code != http.StatusTooManyRequests || resp.Header().Get("Retry-After") != "30" {
		t.Fatalf("the minute after = %d, Retry-After %q; want 429, 30", resp.Code, resp.Header().Get("Retry-After"))
	}
	clock = clock.Add(time.Minute)
	if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("two minutes on = %d, want a clean window: %s", resp.Code, resp.Body)
	}
}

// Over two limits at once, the answer names the one that resets last, and with that further off
// than a client should sit waiting, tells it not to retry by itself.
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
	if resp.Header().Get("Retry-After") != "43170" || resp.Header().Get("X-Should-Retry") != "false" {
		t.Errorf("Retry-After = %q, X-Should-Retry = %q; want the seconds to midnight UTC and no retry",
			resp.Header().Get("Retry-After"), resp.Header().Get("X-Should-Retry"))
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
	if code, body := push("requests:1w:200", ""); code != http.StatusBadRequest || body != "bad body: key K-1: limit \"requests:1w:200\": unknown metric or window, or a value below zero\n" {
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

	own("requests:1m:1")
	if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("first under the key's own row = %d: %s", resp.Code, resp.Body)
	}
	resp := f.post("/openai/v1/chat/completions", limitedBody)
	if message, _ := refusal(t, resp.Body.Bytes()); resp.Code != http.StatusTooManyRequests || message != "rate limit exceeded: 1 requests per 1m (key)" {
		t.Fatalf("second = %d, %q; want 429 from the key's own row", resp.Code, message)
	}
	counts := map[string]int64{}
	for counter, count := range f.store.Limits {
		counts[counter[:strings.Index(counter, ":")]] = count
	}
	if counts["team"] != 1 || counts["key"] != 1 {
		t.Errorf("counters = %v, want team 1 and key 1", f.store.Limits)
	}
}

// A key's limit counts every model together, so a request that falls back counts on it once: the
// fallback is not refused by the count its own request made.
func TestAKeysLimitCountsARequestOnceAcrossAFallback(t *testing.T) {
	f, backup := fallbackFixture(t, failing(http.StatusBadGateway))
	own, err := domain.ParseKeyLimits("requests:1m:1")
	if err != nil {
		t.Fatal(err)
	}
	f.withKey(func(k *domain.KeyRecord) { k.KeyLimits = own })

	if resp := f.post("/openai/v1/chat/completions", fallbackBody); resp.Code != http.StatusOK || len(backup.bodies) != 1 {
		t.Fatalf("first = %d, backup dials = %d; want the fallback's answer", resp.Code, len(backup.bodies))
	}
	resp := f.post("/openai/v1/chat/completions", fallbackBody)
	if message, _ := refusal(t, resp.Body.Bytes()); resp.Code != http.StatusTooManyRequests || message != "rate limit exceeded: 1 requests per 1m (key)" {
		t.Fatalf("second = %d, %q; want 429 from the key's limit", resp.Code, message)
	}
	for counter, count := range f.store.Limits {
		if !strings.HasPrefix(counter, "key:") || strings.Contains(counter, "backup") || count != 1 {
			t.Errorf("counter %q = %d, want the key's one counter at 1", counter, count)
		}
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

// A key record carrying no limits runs under its team record's, a row naming a model holding on
// that model alone; one an older control plane pushed them on keeps those.
func TestTeamLimitsStandInWhereTheKeyNamesNone(t *testing.T) {
	clock := time.Date(2026, 10, 13, 12, 0, 30, 0, time.UTC)
	teamLimit, _ := domain.ParseLimits("requests:1m:1:qwen3-4b,requests:1m:9")

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

// A fallback is held to its own model's limits, not the first model's: the tokens it answers with
// land on its counter, and once its window is spent it is passed over like a model not granted.
func TestAFallbackCountsOnItsOwnModel(t *testing.T) {
	f, backup := fallbackFixture(t, failing(http.StatusBadGateway))
	limits, err := domain.ParseLimits("requests:1m:1:backup,total_tokens:1m:1000")
	if err != nil {
		t.Fatal(err)
	}
	f.withKey(func(k *domain.KeyRecord) { k.Limits = limits })

	if resp := f.post("/openai/v1/chat/completions", fallbackBody); resp.Code != http.StatusOK {
		t.Fatalf("first = %d: %s", resp.Code, resp.Body)
	}
	tokens := map[string]int64{}
	for key, count := range f.store.Limits {
		if strings.Contains(key, ":total_tokens:") {
			tokens[strings.Split(key, ":")[2]] += count
		}
	}
	if tokens["backup"] != 120 || tokens["qwen3-4b"] != 0 {
		t.Errorf("token counters = %v, want the answer's 120 on backup alone", f.store.Limits)
	}

	resp := f.post("/openai/v1/chat/completions", fallbackBody)
	if resp.Code != http.StatusBadGateway || len(backup.bodies) != 1 {
		t.Errorf("second = %d, backup dials = %d; want the primary's 502 and no dial past backup's limit", resp.Code, len(backup.bodies))
	}
}

// limitCounts is every limit counter's count under "<kind>:<model>", a key's under "key:".
func limitCounts(f *fixture) map[string]int64 {
	counts := map[string]int64{}
	for counter, count := range f.store.Limits {
		parts := strings.Split(counter, ":")
		if parts[0] == domain.HolderKey {
			parts[2] = ""
		}
		counts[parts[0]+":"+parts[2]] += count
	}
	return counts
}

// overLimitFixture is a healthy primary and backup under these rows, on a clock that stands still.
func overLimitFixture(t *testing.T, team, own string) (*fixture, *backupEngine) {
	t.Helper()
	f, backup := fallbackFixture(t, jsonEngine(`{`+usageObject+`}`))
	teamRows, err := domain.ParseLimits(team)
	if err != nil {
		t.Fatal(err)
	}
	ownRows, err := domain.ParseKeyLimits(own)
	if err != nil {
		t.Fatal(err)
	}
	f.withKey(func(k *domain.KeyRecord) { k.Limits, k.KeyLimits = teamRows, ownRows })
	now := func() time.Time { return time.Date(2026, 11, 28, 12, 0, 30, 0, time.UTC) }
	f.handler = buildHandler(t, f.store, config.Config{}, 0, func(s *Services) {
		s.Admission.Now, s.Metering.Now = now, now
	})
	return f, backup
}

// A model over its team's limit hands the request to the fallback the caller named, counted on
// the fallback's row and not the first model's. With the fallback over too, or none named, the
// refusal is the first model's.
func TestAModelOverItsLimitMovesToAFallback(t *testing.T) {
	f, backup := overLimitFixture(t, "requests:1m:1:qwen3-4b,requests:1m:1:backup", "")
	const refused = "rate limit exceeded: 1 requests per 1m on qwen3-4b (team)"

	if resp := f.post("/openai/v1/chat/completions", fallbackBody); resp.Code != http.StatusOK || len(backup.bodies) != 0 {
		t.Fatalf("first = %d, backup dials = %d; want the model asked for", resp.Code, len(backup.bodies))
	}
	resp := f.post("/openai/v1/chat/completions", limitedBody)
	if message, _ := refusal(t, resp.Body.Bytes()); resp.Code != http.StatusTooManyRequests || message != refused {
		t.Fatalf("over the limit with no fallback named = %d, %q; want the 429", resp.Code, message)
	}
	resp = f.post("/openai/v1/chat/completions", fallbackBody)
	if resp.Code != http.StatusOK || resp.Header().Get("X-Grove-Fallback") != "backup" || len(backup.bodies) != 1 {
		t.Fatalf("over the limit = %d, X-Grove-Fallback %q, backup dials = %d; want the fallback's answer",
			resp.Code, resp.Header().Get("X-Grove-Fallback"), len(backup.bodies))
	}
	if counts := limitCounts(f); counts["team:qwen3-4b"] != 1 || counts["team:backup"] != 1 {
		t.Errorf("counters = %v, want the moved request on backup alone", f.store.Limits)
	}

	resp = f.post("/openai/v1/chat/completions", fallbackBody)
	message, _ := refusal(t, resp.Body.Bytes())
	if resp.Code != http.StatusTooManyRequests || message != refused || resp.Header().Get("Retry-After") != "30" || len(backup.bodies) != 1 {
		t.Errorf("both over = %d, %q, Retry-After %q, backup dials = %d; want the first model's 429 and no dial",
			resp.Code, message, resp.Header().Get("Retry-After"), len(backup.bodies))
	}
}

// A key's own limit counts every model together: a request a fallback took off a model over its
// limit counts on it once, and a key at it is not let through to the fallback.
func TestAKeysOwnLimitHoldsAcrossAMoveToAFallback(t *testing.T) {
	f, backup := overLimitFixture(t, "requests:1m:1:qwen3-4b", "requests:1m:2")

	for i, dials := range []int{0, 1} {
		if resp := f.post("/openai/v1/chat/completions", fallbackBody); resp.Code != http.StatusOK || len(backup.bodies) != dials {
			t.Fatalf("request %d = %d, backup dials = %d; want %d", i+1, resp.Code, len(backup.bodies), dials)
		}
	}
	if counts := limitCounts(f); counts["key:"] != 2 {
		t.Fatalf("counters = %v, want each request once on the key's row", f.store.Limits)
	}
	resp := f.post("/openai/v1/chat/completions", fallbackBody)
	if counts := limitCounts(f); resp.Code != http.StatusTooManyRequests || len(backup.bodies) != 1 || counts["key:"] != 2 {
		t.Errorf("third = %d, backup dials = %d, counters = %v; want 429, no dial and nothing counted",
			resp.Code, len(backup.bodies), f.store.Limits)
	}
}

// A key's own token row counts every model together, so one taken to twice its ceiling spends
// that key's next window on every model, and no other key's.
func TestAKeysTokenLimitAtTwiceItsCeilingSpendsItsNextWindow(t *testing.T) {
	clock := time.Date(2026, 10, 4, 12, 0, 30, 0, time.UTC)
	f := limitedFixture(t, "", &clock)
	addModel(t, f, "other", jsonEngine(`{`+usageObject+`}`))
	own, err := domain.ParseKeyLimits("total_tokens:1m:60")
	if err != nil {
		t.Fatal(err)
	}
	f.withKey(func(k *domain.KeyRecord) { k.KeyLimits = own })
	twin := f.store.Keys[domain.SHA256Hex(secret)]
	twin.KeyPrefix = "def456"
	f.store.Keys[domain.SHA256Hex("twin-secret")] = twin
	const otherBody, refused = `{"model":"other","messages":[]}`, "rate limit exceeded: 60 tokens per 1m (key)"

	if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("first = %d: %s", resp.Code, resp.Body)
	}
	resp := f.post("/openai/v1/chat/completions", otherBody)
	if message, _ := refusal(t, resp.Body.Bytes()); resp.Code != http.StatusTooManyRequests || message != refused || resp.Header().Get("Retry-After") != "90" {
		t.Fatalf("another model after 120 tokens on the key's ceiling of 60 = %d, %q, Retry-After %q; want the key's 429 to the end of the next minute",
			resp.Code, message, resp.Header().Get("Retry-After"))
	}

	clock = clock.Add(time.Minute)
	resp = f.post("/openai/v1/chat/completions", otherBody)
	if message, _ := refusal(t, resp.Body.Bytes()); resp.Code != http.StatusTooManyRequests || message != refused || resp.Header().Get("Retry-After") != "30" {
		t.Fatalf("the minute after, on another model = %d, %q, Retry-After %q; want the key's 429, 30", resp.Code, message, resp.Header().Get("Retry-After"))
	}
	if resp := f.post("/openai/v1/chat/completions", limitedBody, "Authorization", "Bearer twin-secret"); resp.Code != http.StatusOK {
		t.Fatalf("another key of the team that minute = %d, want its own window: %s", resp.Code, resp.Body)
	}

	clock = clock.Add(time.Minute)
	if resp := f.post("/openai/v1/chat/completions", limitedBody); resp.Code != http.StatusOK {
		t.Fatalf("two minutes on = %d, want a clean window: %s", resp.Code, resp.Body)
	}
}
