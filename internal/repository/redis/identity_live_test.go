//go:build live

package redis

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
)

// trips counts round trips: one per command sent alone, one per pipeline.
type trips struct{ n int }

func (c *trips) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c *trips) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.n++
		return next(ctx, cmd)
	}
}

func (c *trips) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		c.n++
		return next(ctx, cmds)
	}
}

// The script follows a key to its groups, answers each record as it is stored, and costs one
// round trip.
func TestResolveAgainstRealRedis(t *testing.T) {
	client, _ := liveStore(t)
	ctx := context.Background()
	keys := client.Store().Keys

	client.rdb.HSet(ctx, "model_group:a", "models", "m1")
	client.rdb.HSet(ctx, "model_group:b", "models", "m2,m3")
	client.rdb.HSet(ctx, "key:k-two", "status", "active", "prefix", "P-k-two", "team", "T-1", "group", " a , ,b ", "geography", "eu")
	client.rdb.HSet(ctx, "key:k-none", "status", "active", "allow", "m9")
	client.rdb.HSet(ctx, "key:k-gone", "status", "active", "group", "never-pushed")
	client.rdb.HSet(ctx, "key:k-newer", "status", "active", "limits", "concurrent:1m:5")

	if _, found, err := keys.Resolve(ctx, "nope"); found || err != nil {
		t.Errorf("a key never pushed = found %v, %v; want not found", found, err)
	}

	two, found, err := keys.Resolve(ctx, "k-two")
	if err != nil || !found {
		t.Fatalf("k-two = %+v, found %v, %v", two, found, err)
	}
	if two.Key.KeyPrefix != "P-k-two" || two.Key.Team != "T-1" || two.Key.Geography != "eu" {
		t.Errorf("k-two = %+v; the key read wrong", two)
	}
	if len(two.Groups) != 2 || !two.Groups["a"].Models["m1"] || !two.Groups["b"].Models["m3"] {
		t.Errorf("groups = %+v; want a and b out of %q", two.Groups, " a , ,b ")
	}
	for name := range two.Key.Groups {
		if _, read := two.Groups[name]; !read {
			t.Errorf("group %q is on the key record and was not read beside it", name)
		}
	}

	none, _, err := keys.Resolve(ctx, "k-none")
	if err != nil || len(none.Groups) != 0 || !none.Key.Allow["m9"] {
		t.Errorf("an ungrouped key = %+v, %v; want its allow and no group", none, err)
	}

	gone, _, err := keys.Resolve(ctx, "k-gone")
	if grp, read := gone.Groups["never-pushed"]; err != nil || !read || len(grp.Models) != 0 {
		t.Errorf("a group never pushed = %+v (read %v), %v; want it read as granting nothing", grp, read, err)
	}

	// A limit a newer binary wrote is an error, not a key served uncapped.
	if _, _, err := keys.Resolve(ctx, "k-newer"); err == nil {
		t.Error("an unreadable limit read as no limit")
	}

	count := &trips{}
	client.rdb.AddHook(count)
	if _, _, err := keys.Resolve(ctx, "k-two"); err != nil || count.n != 1 {
		t.Errorf("round trips = %d, %v; want 1", count.n, err)
	}
}
