package redis

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
)

// grove:state_hash — one hash of section/bucket fields ("groups", "routes", "users:3f",
// "keys:a0"), written in the same transaction as the records they describe. Losing the store
// loses these too, which is what makes the next control-plane tick re-push everything.
const stateHashKey = "grove:state_hash"

type state struct{ rdb *redis.Client }

func (s state) Hashes(ctx context.Context) (map[string]string, error) {
	return s.rdb.HGetAll(ctx, stateHashKey).Result()
}

// Apply projects the push in one MULTI: upserts, prunes-by-absence, and the hash writes land
// together or not at all. The reads (what each namespace currently holds) happen before the
// transaction — pushes are serialized control-plane side, so nothing races them.
func (s state) Apply(ctx context.Context, push repository.StatePush) (repository.StateCounts, error) {
	var counts repository.StateCounts

	held := map[string][]string{}
	for prefix, present := range map[string]bool{
		"group:":  push.Groups != nil,
		"user:":   push.Users != nil,
		"key:":    push.Keys != nil,
		"deploy:": push.Routes != nil,
	} {
		if !present {
			continue
		}
		ids, err := s.scanIDs(ctx, prefix)
		if err != nil {
			return counts, err
		}
		held[prefix] = ids
	}

	_, err := s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		if push.Groups != nil {
			counts.Groups = applyGroups(ctx, p, *push.Groups, held["group:"])
		}
		if push.Users != nil {
			counts.Users = applyUsers(ctx, p, push.Users, held["user:"])
		}
		if push.Keys != nil {
			counts.Keys = applyKeys(ctx, p, push.Keys, held["key:"])
		}
		if push.Routes != nil {
			if err := applyRoutes(ctx, p, *push.Routes, held["deploy:"], &counts.Routes); err != nil {
				return err
			}
		}
		return nil
	})
	return counts, err
}

func applyGroups(ctx context.Context, p redis.Pipeliner, push repository.GroupsPush, held []string) int {
	named := map[string]bool{}
	for _, rec := range push.Records {
		if rec.Name == "" {
			continue
		}
		named[rec.Name] = true
		p.HSet(ctx, "group:"+rec.Name, map[string]any{"models": rec.Models})
	}
	deleteUnnamed(ctx, p, "group:", held, func(id string) bool { return named[id] })
	if push.Catalog == "" {
		p.Del(ctx, "catalog:public")
	} else {
		p.Set(ctx, "catalog:public", push.Catalog, 0)
	}
	p.HSet(ctx, stateHashKey, "groups", push.Hash)
	return len(named)
}

func applyUsers(ctx context.Context, p redis.Pipeliner, buckets map[string]repository.UserBucket, held []string) int {
	named, count := map[string]bool{}, 0
	for label, bucket := range buckets {
		for _, rec := range bucket.Records {
			if rec.Name == "" {
				continue
			}
			named[rec.Name] = true
			count++
			limited := "0"
			if rec.Limited {
				limited = "1"
			}
			p.HSet(ctx, "user:"+rec.Name, map[string]any{
				"email": rec.Email, "group": rec.Group,
				"allow": rec.Allow, "deny": rec.Deny, "limited": limited,
			})
		}
		setBucketHash(ctx, p, "users:"+label, bucket.Hash, len(bucket.Records))
	}
	deleteUnnamed(ctx, p, "user:", held, func(id string) bool {
		_, pushed := buckets[domain.BucketOf(id)]
		return !pushed || named[id]
	})
	return count
}

func applyKeys(ctx context.Context, p redis.Pipeliner, buckets map[string]repository.KeyBucket, held []string) int {
	named, count := map[string]bool{}, 0
	for label, bucket := range buckets {
		for _, rec := range bucket.Records {
			if rec.MeterID == "" {
				continue
			}
			named[rec.MeterID] = true
			count++
			p.HSet(ctx, "key:"+rec.MeterID, map[string]any{
				"status": rec.Status, "user": rec.User, "prefix": rec.Prefix,
			})
			// A pre-group control plane flattened access onto the key; stale the moment this lands.
			p.HDel(ctx, "key:"+rec.MeterID, "models", "priority")
		}
		setBucketHash(ctx, p, "keys:"+label, bucket.Hash, len(bucket.Records))
	}
	deleteUnnamed(ctx, p, "key:", held, func(id string) bool {
		_, pushed := buckets[domain.BucketOf(id)]
		return !pushed || named[id]
	})
	return count
}

func applyRoutes(ctx context.Context, p redis.Pipeliner, push repository.RoutesPush, held []string, count *int) error {
	named := map[string]bool{}
	for model, table := range push.Table {
		if len(table) == 0 {
			continue // equivalent to absent — the prune below retires it
		}
		encoded, err := json.Marshal(table)
		if err != nil {
			return err
		}
		named[model] = true
		*count++
		p.Set(ctx, "deploy:"+model, encoded, 0)
	}
	deleteUnnamed(ctx, p, "deploy:", held, func(id string) bool { return named[id] })
	p.HSet(ctx, stateHashKey, "routes", push.Hash)
	return nil
}

// setBucketHash stores a pushed bucket's hash, or drops the field for a bucket pushed empty —
// its last record was deleted, so the control plane's snapshot no longer has a hash for it.
func setBucketHash(ctx context.Context, p redis.Pipeliner, field, hash string, records int) {
	if records == 0 {
		p.HDel(ctx, stateHashKey, field)
		return
	}
	p.HSet(ctx, stateHashKey, field, hash)
}

// deleteUnnamed prunes every held id the push did not keep. This is the only way a record
// leaves this box — there is no tombstone path.
func deleteUnnamed(ctx context.Context, p redis.Pipeliner, prefix string, held []string, keep func(string) bool) {
	for _, id := range held {
		if !keep(id) {
			p.Del(ctx, prefix+id)
		}
	}
}

func (s state) scanIDs(ctx context.Context, prefix string) ([]string, error) {
	var ids []string
	var cursor uint64
	for {
		found, next, err := s.rdb.Scan(ctx, cursor, prefix+"*", 200).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range found {
			ids = append(ids, strings.TrimPrefix(k, prefix))
		}
		cursor = next
		if cursor == 0 {
			return ids, nil
		}
	}
}
