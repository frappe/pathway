package memory

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
)

func accrual(prefix, user string, cost int64) repository.Accrual {
	return repository.Accrual{
		Prefix: prefix, User: user, Cost: cost, Budget: 1_000,
		Fields: map[string]int64{"request_count": 1, "completion_tokens": 10, "cost": cost},
	}
}

// One call lands the counters, the cost and the holder's spend, and the hash carries the box's
// own view of the balance for the drain to report.
func TestAccrueMovesCountersAndSpendTogether(t *testing.T) {
	store := New()
	store.Users["GU-1"] = domain.UserRecord{Prepaid: true, Budget: 1_000, Spent: 100}
	usage := store.Repositories().Usage
	ctx := context.Background()

	if err := usage.Accrue(ctx, accrual("abc", "GU-1", 250)); err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	if err := usage.Accrue(ctx, accrual("abc", "GU-1", 250)); err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	for field, want := range map[string]int64{
		"request_count": 2, "completion_tokens": 20, "cost": 500, "user_spent": 600, "user_balance": 400,
	} {
		if store.Usage["abc"][field] != want {
			t.Errorf("usage[%s] = %d, want %d", field, store.Usage["abc"][field], want)
		}
	}
	if store.Users["GU-1"].Spent != 600 {
		t.Errorf("spent = %d, want 600", store.Users["GU-1"].Spent)
	}
}

// A holder this store was never told about gets no record invented for them: the counters land,
// nobody's spend moves, and the drain carries no balance to reconcile.
func TestAccrueOnAnUnknownHolderMovesNoSpend(t *testing.T) {
	store := New()
	if err := store.Repositories().Usage.Accrue(context.Background(), accrual("abc", "GU-ghost", 5)); err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	if _, present := store.Users["GU-ghost"]; present {
		t.Error("a user record was fabricated")
	}
	if _, present := store.Usage["abc"]["user_spent"]; present || store.Usage["abc"]["cost"] != 5 {
		t.Errorf("usage = %v", store.Usage["abc"])
	}
}

// A drain racing accrues sees each request whole: a snapshot with N requests carries N costs.
func TestADrainNeverSplitsARequestFromItsCost(t *testing.T) {
	store := New()
	store.Users["GU-1"] = domain.UserRecord{}
	usage := store.Repositories().Usage
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = usage.Accrue(ctx, accrual("abc", "GU-1", 3))
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
			if requests != 1600 || cost != 4800 || store.Users["GU-1"].Spent != 4800 {
				t.Errorf("requests = %d cost = %d spent = %d", requests, cost, store.Users["GU-1"].Spent)
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
	store.Users["GU-1"] = domain.UserRecord{Spent: 700}
	bucket := domain.BucketOf("GU-1")
	_, err := store.Repositories().State.Apply(context.Background(), repository.StatePush{
		Users: map[string]repository.UserBucket{bucket: {Hash: "uh", Records: []repository.UserUpsert{
			{Name: "GU-1", Prepaid: true, Budget: 5_000},
		}}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	usr := store.Users["GU-1"]
	if !usr.Prepaid || usr.Budget != 5_000 || usr.Spent != 700 {
		t.Errorf("user = %+v, want prepaid, budget 5000, spent 700", usr)
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
func allOf(drains repository.Drains) map[string][]string {
	acks := map[string][]string{}
	for id, usages := range drains {
		for prefix := range usages {
			acks[id] = append(acks[id], prefix)
		}
	}
	return acks
}

// flat is every counter a drain answered, whatever drain it came in.
func flat(drains repository.Drains) []map[string]string {
	var out []map[string]string
	for _, usages := range drains {
		for _, fields := range usages {
			out = append(out, fields)
		}
	}
	return out
}

// A key the control plane never acknowledges is answered again under its own id on every drain,
// beside the new ones, while an acknowledged key moves on: one stuck key holds back only itself.
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
	if n, _ := usage.Ack(ctx, map[string][]string{"d1": {"good"}}, time.Hour); n != 1 {
		t.Fatalf("acked %d pairs, want 1", n)
	}
	store.Usage["bad"] = map[string]int64{"request_count": 5}
	store.Usage["good"] = map[string]int64{"request_count": 4}
	second, _ := usage.Drain(ctx, "d2", nil)
	if second["d1"]["bad"]["request_count"] != "2" || second["d1"]["good"] != nil {
		t.Errorf("d1 = %+v, want only the unacked bad key again", second["d1"])
	}
	if second["d2"]["bad"]["request_count"] != "5" || second["d2"]["good"]["request_count"] != "4" {
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

	if n, _ := usage.Ack(ctx, map[string][]string{"d0": {"abc"}, "d1": {"xyz"}}, time.Hour); n != 0 || !store.Unacked["d1:abc"] {
		t.Errorf("stray ack moved %d, d1:abc waiting = %v", n, store.Unacked["d1:abc"])
	}
	if empty, _ := New().Repositories().Usage.Drain(ctx, "d9", nil); len(empty) != 0 {
		t.Errorf("nothing live = %+v, want no drains", empty)
	}
}

// A spend adjustment applies once per id, and never invents a holder.
func TestAdjustSpentAppliesOncePerID(t *testing.T) {
	store := New()
	store.Users["GU-1"] = domain.UserRecord{Spent: 3_000}
	users := store.Repositories().Users
	ctx := context.Background()

	spent, applied, found, _ := users.AdjustSpent(ctx, "GU-1", "CD-1", -1_000)
	if spent != 2_000 || !applied || !found {
		t.Fatalf("first = %d %v %v", spent, applied, found)
	}
	spent, applied, _, _ = users.AdjustSpent(ctx, "GU-1", "CD-1", -1_000)
	if spent != 2_000 || applied {
		t.Errorf("retry = %d %v, want 2000 unapplied", spent, applied)
	}
	if _, _, found, _ := users.AdjustSpent(ctx, "GU-ghost", "CD-2", 5); found {
		t.Error("a holder was invented")
	}
}
