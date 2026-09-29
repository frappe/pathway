//go:build live

package redis

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
)

func liveAccrual(prefix, user string, cost int64) repository.Accrual {
	return repository.Accrual{
		Prefix: prefix, User: user, Cost: cost, Budget: 1_000,
		Fields: map[string]int64{"request_count": 1, "completion_tokens": 10, "cost": cost},
	}
}

// The script lands counters, cost and spend in one call, and a push afterwards leaves spent alone.
func TestAccrueAgainstRealRedis(t *testing.T) {
	client, state := liveStore(t)
	ctx := context.Background()
	bucket := domain.BucketOf("GU-1")
	push := repository.StatePush{Users: map[string]repository.UserBucket{bucket: {
		Hash: "uh", Records: []repository.UserUpsert{{Name: "GU-1", Prepaid: true, Budget: 1_000}},
	}}}
	if _, err := state.Apply(ctx, push); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	usage := client.Store().Usage
	for i := 0; i < 2; i++ {
		if err := usage.Accrue(ctx, liveAccrual("abc", "GU-1", 250)); err != nil {
			t.Fatalf("Accrue: %v", err)
		}
	}
	// No holder pushed: counters land, no record is invented.
	if err := usage.Accrue(ctx, liveAccrual("xyz", "GU-ghost", 5)); err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	if n, _ := client.rdb.Exists(ctx, "user:GU-ghost").Result(); n != 0 {
		t.Error("a user record was fabricated")
	}

	h, _ := client.rdb.HGetAll(ctx, "usage:abc").Result()
	for field, want := range map[string]string{
		"request_count": "2", "completion_tokens": "20", "cost": "500", "user_spent": "500", "user_balance": "500",
	} {
		if h[field] != want {
			t.Errorf("usage:abc %s = %q, want %q", field, h[field], want)
		}
	}
	usr, _, err := client.Store().Users.Get(ctx, "GU-1")
	if err != nil || !usr.Prepaid || usr.Budget != 1_000 || usr.Spent != 500 {
		t.Errorf("Get = %+v, %v; want prepaid, budget 1000, spent 500", usr, err)
	}

	if _, err := state.Apply(ctx, push); err != nil {
		t.Fatalf("Apply again: %v", err)
	}
	if usr, _, _ = client.Store().Users.Get(ctx, "GU-1"); usr.Spent != 500 {
		t.Errorf("a push moved spent to %d", usr.Spent)
	}
}

// A nano-USD total past 14 digits must survive the trip through Lua unrounded.
func TestAccrueKeepsBigTotalsExact(t *testing.T) {
	client, _ := liveStore(t)
	ctx := context.Background()
	client.rdb.HSet(ctx, "user:GU-1", "spent", "123456789012345678")
	a := liveAccrual("abc", "GU-1", 1)
	a.Budget = 223456789012345678
	if err := client.Store().Usage.Accrue(ctx, a); err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	h, _ := client.rdb.HGetAll(ctx, "usage:abc").Result()
	if h["user_spent"] != "123456789012345679" || h["user_balance"] != "99999999999999999" {
		t.Errorf("user_spent = %q user_balance = %q", h["user_spent"], h["user_balance"])
	}
}

// A drain racing accrues sees each request whole: a snapshot with N requests carries N costs.
func TestADrainNeverSplitsARequestFromItsCostOnRealRedis(t *testing.T) {
	client, _ := liveStore(t)
	ctx := context.Background()
	client.rdb.HSet(ctx, "user:GU-1", "email", "x")
	usage := client.Store().Usage

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = usage.Accrue(ctx, liveAccrual("abc", "GU-1", 3))
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	var requests, cost int64
	drains := 0
	check := func() {
		drains++
		drained, err := usage.Drain(ctx, "d"+strconv.Itoa(drains), nil)
		if err != nil {
			t.Fatalf("Drain: %v", err)
		}
		acks := map[string][]string{}
		for id, usages := range drained {
			for prefix := range usages {
				acks[id] = append(acks[id], prefix)
			}
		}
		if _, err := usage.Ack(ctx, acks, time.Hour); err != nil {
			t.Fatalf("Ack: %v", err)
		}
		for _, fields := range drained["d"+strconv.Itoa(drains)] {
			n, _ := strconv.ParseInt(fields["request_count"], 10, 64)
			c, _ := strconv.ParseInt(fields["cost"], 10, 64)
			if c != n*3 {
				t.Errorf("drained %d requests with cost %d", n, c)
			}
			requests, cost = requests+n, cost+c
		}
	}
	for {
		select {
		case <-done:
			check()
			if requests != 400 || cost != 1200 {
				t.Errorf("requests = %d cost = %d", requests, cost)
			}
			return
		default:
			check()
		}
	}
}

// A drain sets counters aside rather than deleting them: an unacked key is answered again under
// its own id beside the next drain, an acked one is kept with a TTL, and keys narrows both.
func TestAnUnackedKeyIsResentOnRealRedis(t *testing.T) {
	client, _ := liveStore(t)
	ctx := context.Background()
	usage := client.Store().Usage
	_ = usage.Accrue(ctx, liveAccrual("bad", "", 3))
	_ = usage.Accrue(ctx, liveAccrual("good", "", 3))

	first, err := usage.Drain(ctx, "d1", nil)
	if err != nil || len(first["d1"]) != 2 {
		t.Fatalf("first = %+v, %v", first, err)
	}
	if n, err := usage.Ack(ctx, map[string][]string{"d1": {"good", "stray"}, "d0": {"bad"}}, time.Hour); err != nil || n != 1 {
		t.Fatalf("Ack = %d, %v; want only d1:good", n, err)
	}
	if ttl := client.rdb.TTL(ctx, "drained:d1:good").Val(); ttl <= 0 || ttl > time.Hour {
		t.Errorf("acked counter ttl = %v, want kept up to an hour", ttl)
	}
	if ttl := client.rdb.TTL(ctx, "drained:d1:bad").Val(); ttl != -1 {
		t.Errorf("unacked counter ttl = %v, want none", ttl)
	}
	_ = usage.Accrue(ctx, liveAccrual("bad", "", 3))
	_ = usage.Accrue(ctx, liveAccrual("good", "", 3))
	second, _ := usage.Drain(ctx, "d2", []string{"good"})
	if len(second) != 1 || second["d2"]["good"]["request_count"] != "1" {
		t.Errorf("good alone = %+v, want only d2:good", second)
	}
	third, _ := usage.Drain(ctx, "d3", nil)
	if third["d1"]["bad"]["request_count"] != "1" || third["d3"]["bad"]["request_count"] != "1" || third["d2"]["good"] == nil {
		t.Errorf("third = %+v, want d1:bad resent, d2:good still waiting, d3:bad new", third)
	}
	if empty, _ := client.Store().Usage.Drain(ctx, "d4", []string{"nobody"}); len(empty) != 0 {
		t.Errorf("nobody = %+v", empty)
	}
}

// A spend adjustment applies once per id and never invents a holder.
func TestAdjustSpentOnRealRedis(t *testing.T) {
	client, _ := liveStore(t)
	ctx := context.Background()
	client.rdb.HSet(ctx, "user:GU-1", "spent", "3000")
	users := client.Store().Users

	if spent, applied, found, err := users.AdjustSpent(ctx, "GU-1", "CD-1", -1000); spent != 2000 || !applied || !found || err != nil {
		t.Fatalf("first = %d %v %v %v", spent, applied, found, err)
	}
	if spent, applied, _, _ := users.AdjustSpent(ctx, "GU-1", "CD-1", -1000); spent != 2000 || applied {
		t.Errorf("retry = %d %v", spent, applied)
	}
	if _, _, found, _ := users.AdjustSpent(ctx, "GU-ghost", "CD-2", 5); found {
		t.Error("a holder was invented")
	}
	if client.rdb.Exists(ctx, "user:GU-ghost").Val() != 0 {
		t.Error("user:GU-ghost was created")
	}
}
