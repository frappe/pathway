package memory

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/frappe/pathway/internal/domain"
	"github.com/frappe/pathway/internal/repository"
)

func accrual(prefix, key string, cost int64) repository.Accrual {
	return repository.Accrual{
		Prefix: prefix, Key: key, Cost: cost, Budget: 1_000,
		Fields: map[string]int64{"request_count": 1, "completion_tokens": 10, "cost": cost},
	}
}

// One call lands the counters, the cost and the key's spend, and the hash carries the box's
// own view of the balance for the drain to report.
func TestAccrueMovesCountersAndSpendTogether(t *testing.T) {
	store := New()
	store.Keys["k1"] = domain.KeyRecord{Budget: 1_000, Spent: 100}
	usage := store.Repositories().Usage
	ctx := context.Background()

	if err := usage.Accrue(ctx, accrual("abc", "k1", 250)); err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	if err := usage.Accrue(ctx, accrual("abc", "k1", 250)); err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	for field, want := range map[string]int64{
		"request_count": 2, "completion_tokens": 20, "cost": 500, "key_spent": 600, "key_balance": 400,
	} {
		if store.UsageOf("abc")[field] != want {
			t.Errorf("usage[%s] = %d, want %d", field, store.UsageOf("abc")[field], want)
		}
	}
	if store.Keys["k1"].Spent != 600 {
		t.Errorf("spent = %d, want 600", store.Keys["k1"].Spent)
	}
}

// Spent counts one month: a key last touched in an earlier month starts this one at 0, whether a
// read or an accrue touches it first, and an adjustment lands on this month.
func TestSpentStartsEachMonthAtZero(t *testing.T) {
	store := New()
	store.Keys["k1"] = domain.KeyRecord{Budget: 1_000, Spent: 900}
	store.Keys["k2"] = domain.KeyRecord{Budget: 1_000, Spent: 900}
	store.Field["k1"], store.Field["k2"] = "spent:2000-01", "spent:2000-01"
	repos := store.Repositories()
	ctx := context.Background()

	if holder, _, _ := repos.Keys.Resolve(ctx, "k1"); holder.Key.Spent != 0 {
		t.Errorf("read after the flip = %d, want 0", holder.Key.Spent)
	}
	if err := repos.Usage.Accrue(ctx, accrual("abc", "k2", 250)); err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	if store.Keys["k2"].Spent != 250 || store.UsageOf("abc")["key_balance"] != 750 {
		t.Errorf("accrue after the flip: spent = %d balance = %d, want 250 and 750",
			store.Keys["k2"].Spent, store.UsageOf("abc")["key_balance"])
	}
	if spent, _, _, _ := repos.Keys.AdjustSpent(ctx, "k2", "CD-1", -50); spent != 200 {
		t.Errorf("adjusted = %d, want 200", spent)
	}
}

// A key this store was never told about gets no record invented for it: the counters land,
// nobody's spend moves, and the drain carries no balance to reconcile.
func TestAccrueOnAnUnknownHolderMovesNoSpend(t *testing.T) {
	store := New()
	if err := store.Repositories().Usage.Accrue(context.Background(), accrual("abc", "k-ghost", 5)); err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	if _, present := store.Keys["k-ghost"]; present {
		t.Error("a key record was fabricated")
	}
	if _, present := store.UsageOf("abc")["key_spent"]; present || store.UsageOf("abc")["cost"] != 5 {
		t.Errorf("usage = %v", store.UsageOf("abc"))
	}
}

// A request lands under its key's bucket for the UTC day it was served; a drain narrowed to the
// key finds the bucket by its prefix, and a counter from before buckets carried a day drains as is.
func TestUsageIsBucketedByTheDayItWasServed(t *testing.T) {
	store := New()
	usage := store.Repositories().Usage
	ctx := context.Background()
	if err := usage.Accrue(ctx, accrual("abc", "", 3)); err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	store.Usage["old"] = map[string]int64{"request_count": 1}
	day := domain.DayOf(time.Now())
	if store.Usage[repository.UsageBucket("abc", time.Now())]["request_count"] != 1 {
		t.Fatalf("usage = %v, want one request under abc:%s", store.Usage, day)
	}

	mine, _ := usage.Drain(ctx, "d1", []string{"abc"})
	if len(mine["d1"]) != 1 || mine["d1"]["abc"][day]["request_count"] != "1" || store.Usage["old"] == nil {
		t.Errorf("abc alone = %+v, want only its day, old left live", mine)
	}
	rest, _ := usage.Drain(ctx, "d2", nil)
	if rest["d2"]["old"][""]["request_count"] != "1" || rest["d1"]["abc"][day] == nil {
		t.Errorf("all = %+v, want old under d2 with no day, beside the unacked abc", rest)
	}
}

// A drain racing accrues sees each request whole: a snapshot with N requests carries N costs.
func TestADrainNeverSplitsARequestFromItsCost(t *testing.T) {
	store := New()
	store.Keys["k1"] = domain.KeyRecord{}
	usage := store.Repositories().Usage
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = usage.Accrue(ctx, accrual("abc", "k1", 3))
			}
		}()
	}
	var requests, cost int64
	drains := 0
	check := func(drained repository.Drains, _ error) {
		drains++
		_, _ = usage.Ack(ctx, allOf(drained), time.Hour)
		for _, fields := range flat(drained) {
			n, c := atoi(t, fields["request_count"]), atoi(t, fields["cost"])
			if c != n*3 {
				t.Errorf("drained %d requests with cost %d", n, c)
			}
			requests, cost = requests+n, cost+c
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for {
		select {
		case <-done:
			check(usage.Drain(ctx, "final", nil))
			if requests != 1600 || cost != 4800 || store.Keys["k1"].Spent != 4800 {
				t.Errorf("requests = %d cost = %d spent = %d", requests, cost, store.Keys["k1"].Spent)
			}
			return
		default:
			check(usage.Drain(ctx, "d"+strconv.Itoa(drains), nil))
		}
	}
}

// A push carries the ceiling and the flag and leaves this store's own counter where it was.
func TestAPushLeavesSpentAlone(t *testing.T) {
	store := New()
	store.Keys["k1"] = domain.KeyRecord{Spent: 700}
	bucket := domain.BucketOf("k1")
	_, err := store.Repositories().State.Apply(context.Background(), repository.StatePush{
		Keys: map[string]repository.KeyBucket{bucket: {Hash: "kh", Records: []repository.KeyUpsert{
			{MeterID: "k1", Budget: 5_000},
		}}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rec := store.Keys["k1"]
	if rec.Budget != 5_000 || rec.Spent != 700 {
		t.Errorf("key = %+v, want budget 5000, spent 700", rec)
	}
}

func atoi(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	for _, c := range s {
		n = n*10 + int64(c-'0')
	}
	return n
}

// allOf acknowledges every pair a drain answered.
func allOf(drains repository.Drains) repository.Acks {
	acks := repository.Acks{}
	for id, keys := range drains {
		acks[id] = map[string][]string{}
		for prefix, days := range keys {
			for day := range days {
				acks[id][prefix] = append(acks[id][prefix], day)
			}
		}
	}
	return acks
}

// flat is every counter a drain answered, whatever drain, key and day it came under.
func flat(drains repository.Drains) []map[string]string {
	var out []map[string]string
	for _, keys := range drains {
		for _, days := range keys {
			for _, fields := range days {
				out = append(out, fields)
			}
		}
	}
	return out
}

func TestAnUnackedKeyIsResentWhileTheRestMoveOn(t *testing.T) {
	store := New()
	usage := store.Repositories().Usage
	ctx := context.Background()
	store.Usage["bad"] = map[string]int64{"request_count": 2}
	store.Usage["good"] = map[string]int64{"request_count": 1}

	first, _ := usage.Drain(ctx, "d1", nil)
	if len(first["d1"]) != 2 {
		t.Fatalf("first = %+v, want both keys under d1", first)
	}
	if n, _ := usage.Ack(ctx, repository.Acks{"d1": {"good": {""}}}, time.Hour); n != 1 {
		t.Fatalf("acked %d pairs, want 1", n)
	}
	store.Usage["bad"] = map[string]int64{"request_count": 5}
	store.Usage["good"] = map[string]int64{"request_count": 4}
	second, _ := usage.Drain(ctx, "d2", nil)
	if second["d1"]["bad"][""]["request_count"] != "2" || second["d1"]["good"] != nil {
		t.Errorf("d1 = %+v, want only the unacked bad key again", second["d1"])
	}
	if second["d2"]["bad"][""]["request_count"] != "5" || second["d2"]["good"][""]["request_count"] != "4" {
		t.Errorf("d2 = %+v, want both keys' later usage", second["d2"])
	}
	if store.Retained["d1:good"] != time.Hour || store.Drained["d1"]["good"] == nil {
		t.Errorf("an acked key must be kept for retention, not deleted")
	}
}

// A drain named by keys sets aside only those, and answers only their unacknowledged pairs.
func TestADrainForSomeKeysLeavesTheOthersLive(t *testing.T) {
	store := New()
	usage := store.Repositories().Usage
	ctx := context.Background()
	store.Usage["mine"] = map[string]int64{"request_count": 1}
	store.Usage["theirs"] = map[string]int64{"request_count": 1}
	_, _ = usage.Drain(ctx, "d0", []string{"theirs"})

	got, _ := usage.Drain(ctx, "d1", []string{"mine"})
	if len(got) != 1 || len(got["d1"]) != 1 || got["d1"]["mine"] == nil {
		t.Errorf("got %+v, want only mine under d1", got)
	}
	if store.Usage["mine"] != nil || !store.Unacked["d0:theirs"] {
		t.Errorf("mine should be set aside and theirs still waiting")
	}
}

// An ack of a pair not waiting moves nothing, so a stale or retried ack is harmless; nothing live
// and nothing waiting answers no drains.
func TestAStrayAckIsANoOp(t *testing.T) {
	store := New()
	usage := store.Repositories().Usage
	ctx := context.Background()
	store.Usage["abc"] = map[string]int64{"request_count": 1}
	_, _ = usage.Drain(ctx, "d1", nil)

	if n, _ := usage.Ack(ctx, repository.Acks{"d0": {"abc": {""}}, "d1": {"xyz": {""}}}, time.Hour); n != 0 || !store.Unacked["d1:abc"] {
		t.Errorf("stray ack moved %d, d1:abc waiting = %v", n, store.Unacked["d1:abc"])
	}
	if empty, _ := New().Repositories().Usage.Drain(ctx, "d9", nil); len(empty) != 0 {
		t.Errorf("nothing live = %+v, want no drains", empty)
	}
}

// A spend adjustment applies once per id, and never invents a key.
func TestAdjustSpentAppliesOncePerID(t *testing.T) {
	store := New()
	store.Keys["k1"] = domain.KeyRecord{Spent: 3_000}
	keys := store.Repositories().Keys
	ctx := context.Background()

	spent, applied, found, _ := keys.AdjustSpent(ctx, "k1", "CD-1", -1_000)
	if spent != 2_000 || !applied || !found {
		t.Fatalf("first = %d %v %v", spent, applied, found)
	}
	spent, applied, _, _ = keys.AdjustSpent(ctx, "k1", "CD-1", -1_000)
	if spent != 2_000 || applied {
		t.Errorf("retry = %d %v, want 2000 unapplied", spent, applied)
	}
	if _, _, found, _ := keys.AdjustSpent(ctx, "k-ghost", "CD-2", 5); found {
		t.Error("a key was invented")
	}
}

// A teams section lands like keys: a bucket prunes only its own members, its hash lands under
// teams:<label>, and Resolve hands the team back beside a key that names it.
func TestATeamPushLandsPrunesItsBucketAndResolvesWithItsKey(t *testing.T) {
	store := New()
	store.Teams["T-stale"] = domain.TeamRecord{Limited: true}
	store.Teams["T-elsewhere"] = domain.TeamRecord{}
	store.Keys["k1"] = domain.KeyRecord{Status: "active", Team: "T-1"}
	store.Keys["k2"] = domain.KeyRecord{Status: "active", Team: "T-none"}
	bucket := domain.BucketOf("T-1")
	if bucket != domain.BucketOf("T-stale") {
		// Move the stale one into the pushed bucket by naming it so; the ids are fixtures.
		store.Teams[bucketMate(t, "T-1")] = store.Teams["T-stale"]
		delete(store.Teams, "T-stale")
	}
	counts, err := store.Repositories().State.Apply(context.Background(), repository.StatePush{
		Teams: map[string]repository.TeamBucket{bucket: {Hash: "th", Records: []repository.TeamUpsert{
			{Team: "T-1", Limited: true, LogPayloads: true, Limits: "requests:1m:5"},
		}}},
	})
	if err != nil || counts.Teams != 1 {
		t.Fatalf("Apply: %v, counts %+v", err, counts)
	}
	if len(store.Teams) != 2 || store.Teams["T-elsewhere"].Limited {
		t.Errorf("teams = %+v, want T-1 and the one in an unpushed bucket only", store.Teams)
	}
	if store.Hashes["teams:"+bucket] != "th" {
		t.Errorf("hashes = %+v", store.Hashes)
	}
	holder, found, err := store.Repositories().Keys.Resolve(context.Background(), "k1")
	if err != nil || !found || holder.Team == nil || !holder.Team.Limited || holder.Team.Limits[0].Value != 5 {
		t.Errorf("resolve k1 = %+v, %v, %v; want its team record", holder, found, err)
	}
	if holder, _, _ := store.Repositories().Keys.Resolve(context.Background(), "k2"); holder.Team != nil {
		t.Errorf("a key naming a team never pushed carries %+v, want no team record", holder.Team)
	}
}

// bucketMate is an id in the same bucket as `id`, by brute force over a counter.
func bucketMate(t *testing.T, id string) string {
	t.Helper()
	for i := 0; i < 100_000; i++ {
		candidate := "T-mate-" + strconv.Itoa(i)
		if candidate != id && domain.BucketOf(candidate) == domain.BucketOf(id) {
			return candidate
		}
	}
	t.Fatal("no bucket mate found")
	return ""
}

// A key capped for life counts under one field with no month in it, so the month turn that resets
// the others passes it by, and once spent it stays spent.
func TestATotalKeyNeverRollsTheMonth(t *testing.T) {
	store := New()
	store.Keys["k1"] = domain.KeyRecord{Budget: 1_000, Spent: 900, BudgetWindow: domain.WindowTotal}
	store.Field["k1"] = "spent:total"
	repos := store.Repositories()
	ctx := context.Background()

	if holder, _, _ := repos.Keys.Resolve(ctx, "k1"); holder.Key.Spent != 900 {
		t.Errorf("read after the flip = %d, want 900 kept", holder.Key.Spent)
	}
	if err := repos.Usage.Accrue(ctx, accrual("abc", "k1", 250)); err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	if rec := store.Keys["k1"]; rec.Spent != 1150 || !rec.Exhausted() {
		t.Errorf("spent = %d exhausted = %v, want 1150 and spent for good", rec.Spent, rec.Exhausted())
	}
}
