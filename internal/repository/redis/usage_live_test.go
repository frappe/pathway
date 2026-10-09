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

func liveAccrual(prefix, key string, cost int64) repository.Accrual {
	return repository.Accrual{
		Prefix: prefix, Key: key, Cost: cost, Budget: 1_000,
		Fields: map[string]int64{"request_count": 1, "completion_tokens": 10, "cost": cost},
	}
}

// The script lands counters, cost and spend in one call, and a push afterwards leaves spent alone.
func TestAccrueAgainstRealRedis(t *testing.T) {
	client, state := liveStore(t)
	ctx := context.Background()
	bucket := domain.BucketOf("aa")
	push := repository.StatePush{Keys: map[string]repository.KeyBucket{bucket: {
		Hash: "kh", Records: []repository.KeyUpsert{{MeterID: "aa", Prefix: "abc", Status: "active", Prepaid: true, Budget: 1_000}},
	}}}
	if _, err := state.Apply(ctx, push); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	usage := client.Store().Usage
	for i := 0; i < 2; i++ {
		if err := usage.Accrue(ctx, liveAccrual("abc", "aa", 250)); err != nil {
			t.Fatalf("Accrue: %v", err)
		}
	}
	// No key pushed: counters land, no record is invented.
	if err := usage.Accrue(ctx, liveAccrual("xyz", "ghost", 5)); err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	if n, _ := client.rdb.Exists(ctx, "key:ghost").Result(); n != 0 {
		t.Error("a key record was fabricated")
	}

	h, _ := client.rdb.HGetAll(ctx, "usage:abc").Result()
	for field, want := range map[string]string{
		"request_count": "2", "completion_tokens": "20", "cost": "500", "key_spent": "500", "key_balance": "500",
	} {
		if h[field] != want {
			t.Errorf("usage:abc %s = %q, want %q", field, h[field], want)
		}
	}
	holder, _, err := client.Store().Keys.Resolve(ctx, "aa")
	if rec := holder.Key; err != nil || !rec.Prepaid || rec.Budget != 1_000 || rec.Spent != 500 {
		t.Errorf("Resolve = %+v, %v; want prepaid, budget 1000, spent 500", rec, err)
	}

	if _, err := state.Apply(ctx, push); err != nil {
		t.Fatalf("Apply again: %v", err)
	}
	if holder, _, _ = client.Store().Keys.Resolve(ctx, "aa"); holder.Key.Spent != 500 {
		t.Errorf("a push moved spent to %d", holder.Key.Spent)
	}
}

// A nano-USD total past 14 digits must survive the trip through Lua unrounded.
func TestAccrueKeepsBigTotalsExact(t *testing.T) {
	client, _ := liveStore(t)
	ctx := context.Background()
	client.rdb.HSet(ctx, "key:aa", "spent", "123456789012345678")
	a := liveAccrual("abc", "aa", 1)
	a.Budget = 223456789012345678
	if err := client.Store().Usage.Accrue(ctx, a); err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	h, _ := client.rdb.HGetAll(ctx, "usage:abc").Result()
	if h["key_spent"] != "123456789012345679" || h["key_balance"] != "99999999999999999" {
		t.Errorf("key_spent = %q key_balance = %q", h["key_spent"], h["key_balance"])
	}
}

// A drain racing accrues sees each request whole: a snapshot with N requests carries N costs.
func TestADrainNeverSplitsARequestFromItsCostOnRealRedis(t *testing.T) {
	client, _ := liveStore(t)
	ctx := context.Background()
	client.rdb.HSet(ctx, "key:aa", "status", "active")
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

// A spend adjustment applies once per id and never invents a key.
func TestAdjustSpentOnRealRedis(t *testing.T) {
	client, _ := liveStore(t)
	ctx := context.Background()
	client.rdb.HSet(ctx, "key:aa", "spent", "3000")
	keys := client.Store().Keys

	if spent, applied, found, err := keys.AdjustSpent(ctx, "aa", "CD-1", -1000); spent != 2000 || !applied || !found || err != nil {
		t.Fatalf("first = %d %v %v %v", spent, applied, found, err)
	}
	if spent, applied, _, _ := keys.AdjustSpent(ctx, "aa", "CD-1", -1000); spent != 2000 || applied {
		t.Errorf("retry = %d %v", spent, applied)
	}
	if _, _, found, _ := keys.AdjustSpent(ctx, "ghost", "CD-2", 5); found {
		t.Error("a key was invented")
	}
	if client.rdb.Exists(ctx, "key:ghost").Val() != 0 {
		t.Error("key:ghost was created")
	}
}

// A replay lands like an accrue, once per request id: the second replay of the same id moves nothing.
func TestReplayLandsOncePerRequestAgainstRealRedis(t *testing.T) {
	client, state := liveStore(t)
	ctx := context.Background()
	bucket := domain.BucketOf("aa")
	push := repository.StatePush{Keys: map[string]repository.KeyBucket{bucket: {
		Hash: "kh", Records: []repository.KeyUpsert{{MeterID: "aa", Prefix: "abc", Status: "active", Prepaid: true, Budget: 1_000}},
	}}}
	if _, err := state.Apply(ctx, push); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	usage := client.Store().Usage
	if err := usage.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	accrual := liveAccrual("replayed", "aa", 250)
	accrual.ID = "rid-live-1"
	for i, want := range []bool{true, false} {
		landed, err := usage.Replay(ctx, accrual)
		if err != nil || landed != want {
			t.Fatalf("replay %d = %v, %v; want %v", i, landed, err, want)
		}
	}
	h, _ := client.rdb.HGetAll(ctx, "usage:replayed").Result()
	if h["request_count"] != "1" || h["key_spent"] != "250" {
		t.Errorf("usage:replayed = %v, want one request and 250 spent", h)
	}
	if ttl, _ := client.rdb.TTL(ctx, "accrued:rid-live-1").Result(); ttl <= 0 {
		t.Errorf("marker ttl = %v, want a week", ttl)
	}
}
