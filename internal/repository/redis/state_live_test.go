//go:build live

// Run against a throwaway server: `redis-server --port 6390 &` then
// `go test -tags live ./internal/repository/redis/ -run TestState -redis localhost:6390`.
// Tagged out of the default build so CI needs no Redis.
package redis

import (
	"context"
	goflag "flag"
	"fmt"
	"testing"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
)

// sibling finds a distinct id in the same bucket as `id` — a bucket push only ever carries and
// prunes its own members, so the fixtures must respect the labeling the control plane uses.
func sibling(t *testing.T, id string) string {
	t.Helper()
	for i := 0; i < 4096; i++ {
		candidate := fmt.Sprintf("%s-sib%d", id, i)
		if domain.BucketOf(candidate) == domain.BucketOf(id) {
			return candidate
		}
	}
	t.Fatal("no sibling found")
	return ""
}

var redisAddr = goflag.String("redis", "localhost:6390", "address of a THROWAWAY redis — the test flushes it")

func liveStore(t *testing.T) (*Client, repository.State) {
	t.Helper()
	client := New(*redisAddr, "")
	ctx := context.Background()
	if err := client.Ping(ctx); err != nil {
		t.Skipf("no redis at %s: %v", *redisAddr, err)
	}
	if err := client.rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client, client.Store().State
}

func TestStateApplyAgainstRealRedis(t *testing.T) {
	client, state := liveStore(t)
	ctx := context.Background()

	// Seed records the push will and will not name, plus one in an untouched bucket.
	client.rdb.HSet(ctx, "model_group:stale", "models", "m")
	client.rdb.HSet(ctx, "key:aa", "status", "active")
	client.rdb.HSet(ctx, "key:bb", "status", "active")
	client.rdb.Set(ctx, "deploy:stale", "[]", 0)
	client.rdb.Set(ctx, "usage:prefix", "untouchable", 0)

	pushed := domain.BucketOf("aa")
	if pushed == domain.BucketOf("bb") {
		t.Fatal("fixture ids landed in one bucket")
	}
	kept := sibling(t, "aa") // named in aa's bucket, so the push keeps it while pruning aa
	counts, err := state.Apply(ctx, repository.StatePush{
		Groups: &repository.GroupsPush{
			Hash:    "gh",
			Records: []repository.GroupUpsert{{Name: "acme", Models: "m1"}},
		},
		Keys: map[string]repository.KeyBucket{
			pushed: {Hash: "kh", Records: []repository.KeyUpsert{
				{MeterID: kept, Prefix: "K-kept", User: "GU-1", Status: "active", CanReadBalance: true},
			}},
		},
		Routes: &repository.RoutesPush{
			Hash: "rh",
			Table: map[string][]domain.Route{
				"m1": {{EngineURL: "https://box/e/md1", Healthy: true}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if counts.Groups != 1 || counts.Keys != 1 || counts.Routes != 1 {
		t.Errorf("counts = %+v", counts)
	}
	if rec, _, err := client.Store().Keys.Get(ctx, kept); err != nil || !rec.CanReadBalance {
		t.Errorf("pushed key = %+v, %v; want it allowed to read the balance", rec, err)
	}

	for key, want := range map[string]bool{
		"model_group:acme":  true,
		"model_group:stale": false, // unnamed → pruned
		"key:" + kept:       true,
		"key:aa":            false, // in the pushed bucket, unnamed → pruned
		"key:bb":            true,  // its bucket was not pushed → survives
		"deploy:m1":         true,
		"deploy:stale":      false,
		"usage:prefix":      true, // never a prune target
	} {
		n, err := client.rdb.Exists(ctx, key).Result()
		if err != nil {
			t.Fatalf("EXISTS %s: %v", key, err)
		}
		if (n == 1) != want {
			t.Errorf("%s exists = %v, want %v", key, n == 1, want)
		}
	}

	hashes, err := state.Hashes(ctx)
	if err != nil {
		t.Fatalf("Hashes: %v", err)
	}
	want := map[string]string{"groups": "gh", "routes": "rh", "keys:" + pushed: "kh"}
	for field, hash := range want {
		if hashes[field] != hash {
			t.Errorf("hashes[%s] = %q, want %q", field, hashes[field], hash)
		}
	}

	// An empty bucket prunes its members and drops its hash — the delete path of the gate.
	if _, err := state.Apply(ctx, repository.StatePush{
		Keys: map[string]repository.KeyBucket{pushed: {}},
	}); err != nil {
		t.Fatalf("Apply empty bucket: %v", err)
	}
	if n, _ := client.rdb.Exists(ctx, "key:"+kept).Result(); n != 0 {
		t.Error("emptied bucket kept its member")
	}
	if hashes, _ = state.Hashes(ctx); hashes["keys:"+pushed] != "" {
		t.Errorf("emptied bucket kept its hash %q", hashes["keys:"+pushed])
	}
}

func TestAUserRecordRoundTripsItsGeography(t *testing.T) {
	client, state := liveStore(t)
	ctx := context.Background()
	bucket := domain.BucketOf("GU-1")
	if _, err := state.Apply(ctx, repository.StatePush{Users: map[string]repository.UserBucket{
		bucket: {Hash: "uh", Records: []repository.UserUpsert{{Name: "GU-1", Geography: "eu"}}},
	}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	usr, found, err := client.Store().Users.Get(ctx, "GU-1")
	if err != nil || !found || usr.Geography != "eu" {
		t.Errorf("Get = %+v, %v, %v; want geography eu", usr, found, err)
	}
}

// The push is hash-gated: a pushed record that expired would stay missing until its section
// changed. So nothing the control plane pushes may carry a TTL, including after the two gateway
// writes that touch a pushed record — an accrual and a spend adjustment on the holder.
func TestPushedStateNeverExpires(t *testing.T) {
	client, state := liveStore(t)
	ctx := context.Background()
	push := repository.StatePush{
		Groups: &repository.GroupsPush{Hash: "gh", Records: []repository.GroupUpsert{{Name: "acme", Models: "m"}}},
		Users: map[string]repository.UserBucket{domain.BucketOf("GU-1"): {
			Hash: "uh", Records: []repository.UserUpsert{{Name: "GU-1", Prepaid: true, Budget: 1_000, Limits: "requests:1m:5"}},
		}},
		Keys: map[string]repository.KeyBucket{domain.BucketOf("aa"): {
			Hash: "kh", Records: []repository.KeyUpsert{{MeterID: "aa", Prefix: "abc", User: "GU-1", Status: "active"}},
		}},
		Routes: &repository.RoutesPush{Hash: "rh", Table: map[string][]domain.Route{"m": {{EngineURL: "http://e", Healthy: true}}}},
	}
	if _, err := state.Apply(ctx, push); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := client.Store().Usage.Accrue(ctx, liveAccrual("abc", "GU-1", 250)); err != nil {
		t.Fatalf("Accrue: %v", err)
	}
	if _, _, _, err := client.Store().Users.AdjustSpent(ctx, "GU-1", "adj-1", -50); err != nil {
		t.Fatalf("AdjustSpent: %v", err)
	}
	for _, key := range []string{"model_group:acme", "user:GU-1", "key:aa", "deploy:m", stateHashKey} {
		// -1 is "exists, no expiry"; -2 would be a record the push never wrote.
		if ttl := client.rdb.TTL(ctx, key).Val(); ttl != -1 {
			t.Errorf("%s: ttl = %d, want -1 (no expiry)", key, ttl)
		}
	}
}
