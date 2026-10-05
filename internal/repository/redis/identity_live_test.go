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

// The script follows a key to its user and that user's groups, answers each record as it is stored,
// and costs one round trip.
func TestResolveAgainstRealRedis(t *testing.T) {
	client, _ := liveStore(t)
	ctx := context.Background()
	keys := client.Store().Keys

	client.rdb.HSet(ctx, "model_group:a", "models", "m1")
	client.rdb.HSet(ctx, "model_group:b", "models", "m2,m3")
	client.rdb.HSet(ctx, "user:two", "group", " a , ,b ", "email", "r@example.com")
	client.rdb.HSet(ctx, "user:none", "allow", "m9")
	client.rdb.HSet(ctx, "user:gone", "group", "never-pushed")
	client.rdb.HSet(ctx, "user:newer", "limits", "concurrent:1m:5")
	for id, user := range map[string]string{"k-two": "two", "k-none": "none", "k-gone": "gone", "k-newer": "newer"} {
		client.rdb.HSet(ctx, "key:"+id, "status", "active", "user", user, "prefix", "P-"+id)
	}
	// A pre-split key: its user was never pushed, and its group sits on the key itself.
	client.rdb.HSet(ctx, "key:k-legacy", "status", "active", "user", "unpushed", "group", " b ")

	if _, found, err := keys.Resolve(ctx, "nope"); found || err != nil {
		t.Errorf("a key never pushed = found %v, %v; want not found", found, err)
	}

	two, found, err := keys.Resolve(ctx, "k-two")
	if err != nil || !found || !two.HasUser {
		t.Fatalf("k-two = %+v, found %v, %v", two, found, err)
	}
	if two.Key.KeyPrefix != "P-k-two" || two.Key.User != "two" || two.User.Email != "r@example.com" {
		t.Errorf("k-two = %+v; the key or its user read wrong", two)
	}
	if len(two.Groups) != 2 || !two.Groups["a"].Models["m1"] || !two.Groups["b"].Models["m3"] {
		t.Errorf("groups = %+v; want a and b out of %q", two.Groups, " a , ,b ")
	}
	for name := range two.User.Groups {
		if _, read := two.Groups[name]; !read {
			t.Errorf("group %q is on the user record and was not read beside the key", name)
		}
	}

	none, _, err := keys.Resolve(ctx, "k-none")
	if err != nil || !none.HasUser || len(none.Groups) != 0 || !none.User.Allow["m9"] {
		t.Errorf("an ungrouped user = %+v, %v; want the user and no group", none, err)
	}

	gone, _, err := keys.Resolve(ctx, "k-gone")
	if grp, read := gone.Groups["never-pushed"]; err != nil || !read || len(grp.Models) != 0 {
		t.Errorf("a group never pushed = %+v (read %v), %v; want it read as granting nothing", grp, read, err)
	}

	legacy, found, err := keys.Resolve(ctx, "k-legacy")
	if err != nil || !found || legacy.HasUser || legacy.Key.Legacy.Group != "b" || !legacy.Groups["b"].Models["m2"] {
		t.Errorf("a pre-split key = %+v, found %v, %v; want no user and its own group read", legacy, found, err)
	}

	// A limit a newer binary wrote is an error, not a holder served uncapped.
	if _, _, err := keys.Resolve(ctx, "k-newer"); err == nil {
		t.Error("an unreadable limit read as no limit")
	}

	count := &trips{}
	client.rdb.AddHook(count)
	if _, _, err := keys.Resolve(ctx, "k-two"); err != nil || count.n != 1 {
		t.Errorf("round trips = %d, %v; want 1", count.n, err)
	}
}
