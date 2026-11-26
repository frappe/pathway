//go:build live

package http

import (
	"context"
	"encoding/json"
	goflag "flag"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/frappe/pathway/internal/domain"
	redisstore "github.com/frappe/pathway/internal/repository/redis"
)

// The limits through the whole chain on a real Redis: what the in-memory store, serialized by a Go
// lock, cannot show. Run with: go test -tags live -p 1 ./internal/transport/http/ -redis=localhost:6390
// (-p 1: every live package flushes the one store, so they run one at a time).
var liveRedis = goflag.String("redis", "localhost:6390", "address of a THROWAWAY redis — the tests flush it")

var liveCtx = context.Background()

// liveStore is the store at -redis, flushed: the handle a test reads it by behind the gateway.
func liveStore(t *testing.T) *redisstore.Client {
	t.Helper()
	store := redisstore.New(*liveRedis, "")
	if err := store.Ping(liveCtx); err != nil {
		t.Skipf("no redis at %s: %v", *liveRedis, err)
	}
	if err := store.Flush(liveCtx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// liveStack is a gateway over the store at -redis, on a connection of its own.
func liveStack(t *testing.T, clock *time.Time) *stack {
	t.Helper()
	client := redisstore.New(*liveRedis, "")
	t.Cleanup(func() { client.Close() })
	return newStack(t, client.Store(), clock)
}

func liveClock() *time.Time {
	clock := time.Date(2026, 10, 4, 12, 0, 10, 0, time.UTC)
	return &clock
}

// burst sends n requests released together and answers each one's status.
func burst(n int, send func(i int) int) []int {
	codes := make([]int, n)
	release := make(chan struct{})
	var wait sync.WaitGroup
	for i := range n {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-release
			codes[i] = send(i)
		}()
	}
	close(release)
	wait.Wait()
	return codes
}

func tally(codes []int) map[int]int {
	counts := map[int]int{}
	for _, code := range codes {
		counts[code]++
	}
	return counts
}

// liveCounters reads every counter matching pattern, by key.
func liveCounters(t *testing.T, store *redisstore.Client, pattern string) map[string]int64 {
	t.Helper()
	counts, err := store.Counters(liveCtx, pattern)
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

// liveCounter is the one counter matching pattern, 0 when there is none.
func liveCounter(t *testing.T, store *redisstore.Client, pattern string) int64 {
	t.Helper()
	found := liveCounters(t, store, pattern)
	if len(found) > 1 {
		t.Fatalf("%s matches %v, want one counter", pattern, found)
	}
	for _, count := range found {
		return count
	}
	return 0
}

func answering(models ...string) map[string]http.HandlerFunc {
	handlers := map[string]http.HandlerFunc{}
	for _, model := range models {
		handlers[model] = jsonEngine(`{` + usageObject + `}`)
	}
	return handlers
}

func limitedTeam(name, limits string) map[string]any {
	return pushedTeam(name, map[string]any{"limits": limits})
}

func messageOf(t *testing.T, body []byte) string {
	t.Helper()
	message, _ := refusal(t, body)
	return message
}

// Fifty requests land together on a limit of ten: exactly ten are admitted, round after round. The
// check and the count are one script, so no two requests both take the last slot.
func TestLiveExactlyTheCeilingIsAdmittedUnderCollision(t *testing.T) {
	store, clock := liveStore(t), liveClock()
	s := liveStack(t, clock)
	s.seed(answering("m1"), nil,
		[]map[string]any{limitedTeam("T-1", "requests:1m:10")},
		[]map[string]any{pushedKey("s1", "K-1", "T-1", nil), pushedKey("s2", "K-2", "T-1", nil)})

	for round := range 20 {
		*clock = clock.Add(time.Minute)
		got := tally(burst(50, func(i int) int { return s.ask([]string{"s1", "s2"}[i%2], "m1").Code }))
		if got[http.StatusOK] != 10 || got[http.StatusTooManyRequests] != 40 {
			t.Fatalf("round %d: %v, want 10 admitted and 40 refused", round, got)
		}
		bucket := strconv.FormatInt(clock.Unix()/60, 10)
		if count := liveCounter(t, store, "lim:team:T-1:m1:requests:1m:"+bucket); count != 10 {
			t.Fatalf("round %d: the counter reads %d, want 10", round, count)
		}
	}
}

// Two gateways on one store count into the same window: the ceiling is the team's, not each box's.
func TestLiveTwoGatewaysShareOneCount(t *testing.T) {
	liveStore(t)
	clock := liveClock()
	first, second := liveStack(t, clock), liveStack(t, clock)
	first.seed(answering("m1"), nil,
		[]map[string]any{limitedTeam("T-1", "requests:1m:10")},
		[]map[string]any{pushedKey("s1", "K-1", "T-1", nil)})

	for round := range 5 {
		*clock = clock.Add(time.Minute)
		got := tally(burst(50, func(i int) int { return []*stack{first, second}[i%2].ask("s1", "m1").Code }))
		if got[http.StatusOK] != 10 {
			t.Fatalf("round %d: %v, want 10 admitted across the two", round, got)
		}
	}
}

// A refusal moves no counter, however many arrive.
func TestLiveARefusalCountsNothing(t *testing.T) {
	store, clock := liveStore(t), liveClock()
	s := liveStack(t, clock)
	s.seed(answering("m1"), nil,
		[]map[string]any{limitedTeam("T-1", "requests:1m:2,total_tokens:1h:100000")},
		[]map[string]any{pushedKey("s1", "K-1", "T-1", map[string]any{"key_limits": "requests:1d:50"})})
	for range 2 {
		s.ask("s1", "m1")
	}
	before := liveCounters(t, store, "lim:*")
	for i := range 100 {
		if resp := s.ask("s1", "m1"); resp.Code != http.StatusTooManyRequests {
			t.Fatalf("request %d past the ceiling = %d", i, resp.Code)
		}
	}
	after := liveCounters(t, store, "lim:*")
	if len(before) != 3 || fmt.Sprint(before) != fmt.Sprint(after) {
		t.Errorf("counters moved under refusals: %v, then %v", before, after)
	}
}

// The key's own row refuses the key alone, and what it refuses is not counted on the team: the
// rest of the team's window is still there for its other keys.
func TestLiveTheKeysGateTakesNothingFromTheTeam(t *testing.T) {
	store, clock := liveStore(t), liveClock()
	s := liveStack(t, clock)
	s.seed(answering("m1"), nil,
		[]map[string]any{limitedTeam("T-1", "requests:1m:5")},
		[]map[string]any{
			pushedKey("s1", "K-1", "T-1", map[string]any{"key_limits": "requests:1m:2"}),
			pushedKey("s2", "K-2", "T-1", nil),
		})

	var own []int
	var last string
	for range 5 {
		resp := s.ask("s1", "m1")
		own = append(own, resp.Code)
		last = messageOf(t, resp.Body.Bytes())
	}
	if fmt.Sprint(own) != "[200 200 429 429 429]" || last != "rate limit exceeded: 2 requests per 1m on m1 (key)" {
		t.Fatalf("the limited key = %v, %q", own, last)
	}
	if count := liveCounter(t, store, "lim:team:T-1:m1:requests:1m:*"); count != 2 {
		t.Fatalf("the team's counter reads %d after the key's refusals, want 2", count)
	}
	var other []int
	for range 4 {
		resp := s.ask("s2", "m1")
		other = append(other, resp.Code)
		last = messageOf(t, resp.Body.Bytes())
	}
	if fmt.Sprint(other) != "[200 200 200 429]" || last != "rate limit exceeded: 5 requests per 1m on m1 (team)" {
		t.Errorf("the other key = %v, %q; want the team's three left, then the team's refusal", other, last)
	}
}

// Every counter is kept two of its windows and no longer.
func TestLiveACounterOutlivesItsWindowOnce(t *testing.T) {
	store, clock := liveStore(t), liveClock()
	s := liveStack(t, clock)
	s.seed(answering("m1"), nil,
		[]map[string]any{limitedTeam("T-1", "requests:1m:9,requests:1h:9,requests:1d:9,total_tokens:1h:9000")},
		[]map[string]any{pushedKey("s1", "K-1", "T-1", nil)})
	if resp := s.ask("s1", "m1"); resp.Code != http.StatusOK {
		t.Fatalf("request = %d: %s", resp.Code, resp.Body)
	}
	for pattern, window := range map[string]time.Duration{
		"lim:team:T-1:m1:requests:1m:*":     time.Minute,
		"lim:team:T-1:m1:requests:1h:*":     time.Hour,
		"lim:team:T-1:m1:requests:1d:*":     24 * time.Hour,
		"lim:team:T-1:m1:total_tokens:1h:*": time.Hour,
	} {
		found := liveCounters(t, store, pattern)
		if len(found) != 1 {
			t.Fatalf("%s = %v, want one counter", pattern, found)
		}
		for key := range found {
			if ttl := store.TTL(liveCtx, key); ttl <= window || ttl > 2*window {
				t.Errorf("%s ttl = %s, want within two windows of %s", key, ttl, window)
			}
		}
	}
}

// What is accepted, pinned: tokens are counted once the answer says how many, so requests in
// flight together all pass a token limit, and only the request limit bounds how far. Rewrite
// this the day an estimate is reserved at admit.
func TestLiveATokenLimitIsPassedByWhatIsInFlight(t *testing.T) {
	slow := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		jsonEngine(`{`+usageObject+`}`)(w, r)
	}
	store, clock := liveStore(t), liveClock()
	s := liveStack(t, clock)
	s.seed(map[string]http.HandlerFunc{"m1": slow}, nil,
		[]map[string]any{
			limitedTeam("T-1", "total_tokens:1m:300"),
			limitedTeam("T-2", "total_tokens:1m:300,requests:1m:4"),
		},
		[]map[string]any{pushedKey("s1", "K-1", "T-1", nil), pushedKey("s2", "K-2", "T-2", nil)})

	got := tally(burst(10, func(int) int { return s.ask("s1", "m1").Code }))
	if got[http.StatusOK] != 10 {
		t.Fatalf("ten at once = %v, want all admitted: nothing was counted when they were let in", got)
	}
	if count := liveCounter(t, store, "lim:team:T-1:m1:total_tokens:1m:*"); count != 1200 {
		t.Errorf("token counter = %d, want 1200: ten answers of 120 against a limit of 300", count)
	}
	resp := s.ask("s1", "m1")
	if message := messageOf(t, resp.Body.Bytes()); resp.Code != http.StatusTooManyRequests || message != "rate limit exceeded: 300 tokens per 1m on m1 (team)" {
		t.Errorf("the eleventh = %d %q, want the token limit's refusal", resp.Code, message)
	}

	got = tally(burst(10, func(int) int { return s.ask("s2", "m1").Code }))
	if got[http.StatusOK] != 4 || got[http.StatusTooManyRequests] != 6 {
		t.Fatalf("ten at once under a request limit of four = %v", got)
	}
	if count := liveCounter(t, store, "lim:team:T-2:m1:total_tokens:1m:*"); count != 480 {
		t.Errorf("token counter = %d, want 480: the request limit is what bounded it", count)
	}
}

// A fallback is counted on its own model on the real store too, and is passed over at its ceiling.
func TestLiveAFallbackCountsOnItsOwnModel(t *testing.T) {
	store, clock := liveStore(t), liveClock()
	s := liveStack(t, clock)
	handlers := answering("m2")
	handlers["m1"] = failing(http.StatusBadGateway)
	dials := s.seed(handlers, nil,
		[]map[string]any{limitedTeam("T-1", "requests:1m:1:m2,total_tokens:1m:1000")},
		[]map[string]any{pushedKey("s1", "K-1", "T-1", nil)})

	resp := s.ask("s1", "m1", "m2")
	if resp.Code != http.StatusOK || resp.Header().Get("X-Grove-Fallback") != "m2" {
		t.Fatalf("first = %d, fallback %q: %s", resp.Code, resp.Header().Get("X-Grove-Fallback"), resp.Body)
	}
	if m2, m1 := liveCounter(t, store, "lim:team:T-1:m2:total_tokens:1m:*"), liveCounter(t, store, "lim:team:T-1:m1:total_tokens:1m:*"); m2 != 120 || m1 != 0 {
		t.Errorf("tokens counted: m2 %d, m1 %d; want the answer's 120 on the model that gave it", m2, m1)
	}
	if resp := s.ask("s1", "m1", "m2"); resp.Code != http.StatusBadGateway || dials["m2"].Load() != 1 {
		t.Errorf("second = %d, m2 dialled %d times; want the first model's 502 and no dial past m2's limit", resp.Code, dials["m2"].Load())
	}
}

// A suspension is the team's one record: it refuses every key of the team, and lifting it is that
// one record pushed again, no key touched.
func TestLiveASuspensionIsOneRecord(t *testing.T) {
	liveStore(t)
	s := liveStack(t, liveClock())
	keys := []map[string]any{pushedKey("s1", "K-1", "T-1", nil), pushedKey("s2", "K-2", "T-1", nil)}
	s.seed(answering("m1"), nil, []map[string]any{pushedTeam("T-1", map[string]any{"limited": true})}, keys)
	for _, secret := range []string{"s1", "s2"} {
		if resp := s.ask(secret, "m1"); resp.Code != http.StatusPaymentRequired {
			t.Fatalf("%s under a suspended team = %d", secret, resp.Code)
		}
	}
	s.push(map[string]any{"teams": s.buckets("team", pushedTeam("T-1", nil))})
	for _, secret := range []string{"s1", "s2"} {
		if resp := s.ask(secret, "m1"); resp.Code != http.StatusOK {
			t.Fatalf("%s once the team record is lifted = %d: %s", secret, resp.Code, resp.Body)
		}
	}
}

// The 402 of a spent cap says the team's text, off the team's record; a suspended team's keeps
// the gateway's own.
func TestLiveASpentCapSaysTheTeamsMessage(t *testing.T) {
	store := liveStore(t)
	s := liveStack(t, liveClock())
	key := pushedKey("s1", "K-1", "T-1", map[string]any{"budget": 5})
	s.seed(answering("m1"), nil, []map[string]any{pushedTeam("T-1", map[string]any{"cap_message": "trial over"})}, []map[string]any{key})
	if err := store.SetField(liveCtx, "key:"+domain.SHA256Hex("s1"), "spent:"+domain.MonthOf(time.Now()), 5); err != nil {
		t.Fatal(err)
	}

	resp := s.ask("s1", "m1")
	if message := messageOf(t, resp.Body.Bytes()); resp.Code != http.StatusPaymentRequired || message != "trial over" {
		t.Fatalf("spent cap = %d %q, want the team's message", resp.Code, message)
	}
	s.push(map[string]any{"teams": s.buckets("team", pushedTeam("T-1", map[string]any{"cap_message": "trial over", "limited": true}))})
	resp = s.ask("s1", "m1")
	if message := messageOf(t, resp.Body.Bytes()); resp.Code != http.StatusPaymentRequired || message != "key credits exhausted" {
		t.Errorf("suspended = %d %q, want the gateway's own text", resp.Code, message)
	}
}

func liveHashes(t *testing.T, s *stack) map[string]string {
	t.Helper()
	var reply struct {
		Hashes map[string]string `json:"hashes"`
	}
	w := adminCall(t, s.admin, http.MethodGet, "/grove-admin/state-hash", "")
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatalf("state-hash = %d %s", w.Code, w.Body)
	}
	return reply.Hashes
}

// A box holding what a control plane from before team records pushed — limits, verdict and message
// on each key — is cut over in one push: the team's record takes them, the key's copies are
// cleared, and the window the team was in carries on at the count it had.
func TestLiveABoxIsCutOverWithoutAFreshWindow(t *testing.T) {
	store, clock := liveStore(t), liveClock()
	s := liveStack(t, clock)
	old := map[string]any{"limits": "requests:1m:3", "limited": false, "log_payloads": false, "cap_message": "old"}
	s.seed(answering("m1"), nil, nil, []map[string]any{pushedKey("s1", "K-1", "T-1", old), pushedKey("s2", "K-2", "T-1", old)})
	for _, secret := range []string{"s1", "s2"} {
		if resp := s.ask(secret, "m1"); resp.Code != http.StatusOK {
			t.Fatalf("%s before the cutover = %d: %s", secret, resp.Code, resp.Body)
		}
	}

	s.push(map[string]any{
		"teams": s.buckets("team", limitedTeam("T-1", "requests:1m:3")),
		"keys":  s.buckets("key_hash", pushedKey("s1", "K-1", "T-1", nil), pushedKey("s2", "K-2", "T-1", nil)),
	})
	record := store.Record(liveCtx, "key:"+domain.SHA256Hex("s1"))
	if record["limits"] != "" || record["cap_message"] != "" {
		t.Errorf("the key still carries limits %q, cap_message %q", record["limits"], record["cap_message"])
	}
	if limits := store.Record(liveCtx, "team:T-1")["limits"]; limits != "requests:1m:3" {
		t.Errorf("team:T-1 limits = %q", limits)
	}
	if _, held := liveHashes(t, s)["teams:"+domain.BucketOf("T-1")]; !held {
		t.Errorf("no hash for the team's bucket: %v", liveHashes(t, s))
	}
	third, fourth := s.ask("s1", "m1"), s.ask("s2", "m1")
	if message := messageOf(t, fourth.Body.Bytes()); third.Code != http.StatusOK || fourth.Code != http.StatusTooManyRequests || message != "rate limit exceeded: 3 requests per 1m on m1 (team)" {
		t.Errorf("after the cutover: third = %d, fourth = %d %q; want the count carried on from two", third.Code, fourth.Code, message)
	}
}

// What a rollback meets, pinned: a push with no teams section prunes no team record, so the
// record a newer control plane left keeps deciding over what the older one pushes on the keys.
// Pushing the record's bucket empty is what removes it, and the keys' own fields are read again.
func TestLiveAStaleTeamRecordIsDroppedByPushingItsBucketEmpty(t *testing.T) {
	store := liveStore(t)
	s := liveStack(t, liveClock())
	s.seed(answering("m1"), nil, []map[string]any{pushedTeam("T-1", nil)}, []map[string]any{pushedKey("s1", "K-1", "T-1", nil)})

	s.push(map[string]any{"keys": s.buckets("key_hash", pushedKey("s1", "K-1", "T-1", map[string]any{"limited": true}))})
	if resp := s.ask("s1", "m1"); resp.Code != http.StatusOK {
		t.Fatalf("with the stale team record = %d, want it to still decide (200)", resp.Code)
	}

	bucket := domain.BucketOf("T-1")
	s.push(map[string]any{"teams": map[string]any{"buckets": map[string]any{bucket: map[string]any{"records": []any{}}}}})
	if len(store.Record(liveCtx, "team:T-1")) != 0 {
		t.Fatal("team:T-1 outlived its bucket pushed empty")
	}
	if _, held := liveHashes(t, s)["teams:"+bucket]; held {
		t.Error("the emptied bucket kept its hash")
	}
	if resp := s.ask("s1", "m1"); resp.Code != http.StatusPaymentRequired {
		t.Errorf("once the team record is gone = %d, want the key's own verdict (402)", resp.Code)
	}
}

// What moving the team's verdicts off the keys costs, pinned: a key whose team record is missing
// runs under no team limit and no suspension.
func TestLiveAKeyWithoutItsTeamRecordRunsUngated(t *testing.T) {
	store := liveStore(t)
	s := liveStack(t, liveClock())
	team := pushedTeam("T-1", map[string]any{"limits": "requests:1m:1", "limited": true})
	s.seed(answering("m1"), nil, []map[string]any{team}, []map[string]any{pushedKey("s1", "K-1", "T-1", nil)})
	if resp := s.ask("s1", "m1"); resp.Code != http.StatusPaymentRequired {
		t.Fatalf("under its team record = %d, want 402", resp.Code)
	}
	if err := store.Delete(liveCtx, "team:T-1"); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if resp := s.ask("s1", "m1"); resp.Code != http.StatusOK {
			t.Fatalf("request %d without the team record = %d: %s", i, resp.Code, resp.Body)
		}
	}
}

// The cap's window stays the key's: a key capped for life counts under spent:total and is refused
// there, whatever its team's record holds.
func TestLiveALifetimeCapCountsUnderItsOwnField(t *testing.T) {
	store := liveStore(t)
	s := liveStack(t, liveClock())
	pricing := &domain.Pricing{ID: "mp-1", Rates: map[string]int64{"prompt_tokens": 3e9}, Counters: counters(t)}
	key := pushedKey("s1", "K-1", "T-1", map[string]any{"budget": 1, "budget_window": "total"})
	s.seed(answering("m1"), pricing, []map[string]any{pushedTeam("T-1", map[string]any{"cap_message": "trial over"})}, []map[string]any{key})

	if resp := s.ask("s1", "m1"); resp.Code != http.StatusOK {
		t.Fatalf("first = %d: %s", resp.Code, resp.Body)
	}
	record := store.Record(liveCtx, "key:"+domain.SHA256Hex("s1"))
	spent, _ := strconv.ParseInt(record["spent:total"], 10, 64)
	if _, monthly := record["spent:"+domain.MonthOf(time.Now())]; spent <= 0 || monthly {
		t.Fatalf("record = %v, want the spend under spent:total alone", record)
	}
	resp := s.ask("s1", "m1")
	if message := messageOf(t, resp.Body.Bytes()); resp.Code != http.StatusPaymentRequired || message != "trial over" {
		t.Errorf("second = %d %q, want the lifetime cap's refusal in the team's words", resp.Code, message)
	}
}

// The random sequences of TestLimitsAgreeWithTheModel, on the real store's scripts.
func TestLiveLimitsAgreeWithTheModel(t *testing.T) {
	for seed := int64(1); seed <= 5; seed++ {
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) {
			liveStore(t)
			client := redisstore.New(*liveRedis, "")
			t.Cleanup(func() { client.Close() })
			runLimitSequence(t, client.Store(), seed, 300)
		})
	}
}
