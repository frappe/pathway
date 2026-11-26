//go:build live

package redis

import (
	"context"
	"time"
)

// What a live test of another package needs of a throwaway store, so that only this package speaks
// Redis: wipe it, and read or disturb a record behind the gateway's back. Built under the live tag
// alone; nothing in the binary has these.

// Flush wipes the store.
func (c *Client) Flush(ctx context.Context) error { return c.rdb.FlushDB(ctx).Err() }

// Counters is every integer key matching pattern, by key.
func (c *Client) Counters(ctx context.Context, pattern string) (map[string]int64, error) {
	keys, err := c.rdb.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, err
	}
	counts := map[string]int64{}
	for _, key := range keys {
		counts[key], _ = c.rdb.Get(ctx, key).Int64()
	}
	return counts, nil
}

// Record is a hash as it is stored; empty when there is none.
func (c *Client) Record(ctx context.Context, key string) map[string]string {
	return c.rdb.HGetAll(ctx, key).Val()
}

// SetField writes one field of a hash.
func (c *Client) SetField(ctx context.Context, key, field string, value any) error {
	return c.rdb.HSet(ctx, key, field, value).Err()
}

// Delete removes a key.
func (c *Client) Delete(ctx context.Context, key string) error { return c.rdb.Del(ctx, key).Err() }

// TTL is how long a key has left.
func (c *Client) TTL(ctx context.Context, key string) time.Duration { return c.rdb.TTL(ctx, key).Val() }
