package redis

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
)

// key:<sha256(secret)> and model_group:<Model Group name>. The key carries its own policy — what
// it may call, where, how fast, and the cap it spends against — because a key lives in one
// geography and so on one store, which is what lets that store gate the cap exactly. The group is
// its own record so a grant shared by many keys is one write.

type keys struct{ rdb *redis.Client }

// resolveScript follows a credential to its groups in one call: which groups to read comes from
// the key record's list, so a pipeline could not join them. It only fetches; every decision over
// what comes back is made in Go. Replies {key, {name, group, ...}}, each record as HGETALL
// answers it and empty when absent.
//
// It reads model_group: keys it is not handed, so the store must be a single Redis.
//
// KEYS[1] key:<meter id>
var resolveScript = redis.NewScript(`
local key = redis.call('HGETALL', KEYS[1])
local names = ''
for i = 1, #key, 2 do
  if key[i] == 'group' then
    names = key[i + 1]
  end
end
local groups = {}
for name in string.gmatch(names, '[^,]+') do
  name = string.match(name, '^%s*(.-)%s*$')
  if name ~= '' then
    groups[#groups + 1] = name
    groups[#groups + 1] = redis.call('HGETALL', 'model_group:' .. name)
  end
end
return {key, groups}
`)

func (k keys) Resolve(ctx context.Context, meterID string) (repository.Holder, bool, error) {
	res, err := resolveScript.Run(ctx, k.rdb, []string{"key:" + meterID}).Slice()
	if err != nil {
		return repository.Holder{}, false, err
	}
	if len(res) != 2 {
		return repository.Holder{}, false, fmt.Errorf("resolve answered %d records, want 2", len(res))
	}
	key := hashOf(res[0])
	if len(key) == 0 {
		return repository.Holder{}, false, nil
	}
	rec, err := keyRecord(key)
	if err != nil {
		return repository.Holder{}, false, err
	}
	holder := repository.Holder{Key: rec, Groups: map[string]domain.GroupRecord{}}
	groups, _ := res[1].([]any)
	for i := 0; i+1 < len(groups); i += 2 {
		name, _ := groups[i].(string)
		holder.Groups[name] = domain.GroupRecord{Models: domain.ModelSet(hashOf(groups[i+1])["models"])}
	}
	return holder, true, nil
}

// hashOf reads a script's field, value, ... reply as the map HGETALL would have answered.
func hashOf(reply any) map[string]string {
	flat, _ := reply.([]any)
	h := make(map[string]string, len(flat)/2)
	for i := 0; i+1 < len(flat); i += 2 {
		field, _ := flat[i].(string)
		h[field], _ = flat[i+1].(string)
	}
	return h
}

func keyRecord(h map[string]string) (domain.KeyRecord, error) {
	// A push is refused unless every limit reads, so one that does not was written by a newer
	// binary. Serving that key uncapped would hide it.
	limits, err := domain.ParseLimits(h["limits"])
	if err != nil {
		return domain.KeyRecord{}, err
	}
	return domain.KeyRecord{
		Status:      h["status"],
		Team:        h["team"],
		KeyPrefix:   h["prefix"],
		Groups:      domain.ModelSet(h["group"]),
		Allow:       domain.ModelSet(h["allow"]),
		Deny:        domain.ModelSet(h["deny"]),
		Limited:     strings.TrimSpace(h["limited"]) == "1",
		LogPayloads: strings.TrimSpace(h["log_payloads"]) == "1",
		Geography:   strings.TrimSpace(h["geography"]),
		Prepaid:     strings.TrimSpace(h["prepaid"]) == "1",
		Budget:      int64Field(h, "budget"),
		Spent:       int64Field(h, "spent"),
		Limits:      limits,
	}, nil
}

// keyFields is what a push writes. `spent` is not among them: it is this box's own counter, and
// an HSET here never clears a field it does not name.
func keyFields(rec repository.KeyUpsert) map[string]any {
	return map[string]any{
		"status":       rec.Status,
		"team":         rec.Team,
		"prefix":       rec.Prefix,
		"group":        rec.Groups, // comma list of group names
		"allow":        rec.Allow,
		"deny":         rec.Deny,
		"limited":      flag(rec.Limited),
		"log_payloads": flag(rec.LogPayloads),
		"geography":    rec.Geography,
		"prepaid":      flag(rec.Prepaid),
		"budget":       strconv.FormatInt(rec.Budget, 10),
		"limits":       rec.Limits, // always written: blank is what clears a removed limit
	}
}

// staleKeyFields were written by control planes before the policy moved onto the key: the user
// pointer and the balance-read flag, and the flattened model set before that. Dropped with every
// write so a record never shows new fields beside stale ones.
var staleKeyFields = []string{"user", "can_read_balance", "models", "priority"}

// writeKey lands one record whole: the write and the strip of stale fields go in one
// transaction, so a reader never sees it half-updated.
func writeKey(ctx context.Context, p redis.Pipeliner, rec repository.KeyUpsert) {
	redisKey := "key:" + rec.MeterID
	p.HSet(ctx, redisKey, keyFields(rec))
	p.HDel(ctx, redisKey, staleKeyFields...)
}

func (k keys) Upsert(ctx context.Context, records []repository.KeyUpsert) error {
	for _, rec := range records {
		if rec.MeterID == "" {
			continue
		}
		_, err := k.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
			writeKey(ctx, p, rec)
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (k keys) Delete(ctx context.Context, ids []string) (int, error) {
	return deletePrefixed(ctx, k.rdb, "key:", ids)
}

// int64Field reads a decimal field, 0 when absent — a record from before the field existed.
func int64Field(h map[string]string, field string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(h[field]), 10, 64)
	return n
}

// adjustScript moves one key's spend once per adjustment id. Replies {spent, code}: code 1
// applied, 0 already applied, -1 no such key. The id is kept a week: a retry comes within
// minutes, and the control plane names each adjustment after its own row, so ids never recur.
//
// KEYS[1] adjust:<id>, KEYS[2] key:<meter id>; ARGV[1] delta, ARGV[2] seconds to keep the id
var adjustScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[2]) == 0 then
  return {'0', -1}
end
if redis.call('EXISTS', KEYS[1]) == 1 then
  return {redis.call('HGET', KEYS[2], 'spent') or '0', 0}
end
local spent = redis.call('HINCRBY', KEYS[2], 'spent', ARGV[1])
redis.call('SET', KEYS[1], '1', 'EX', ARGV[2])
return {tostring(spent), 1}
`)

const adjustKeep = 7 * 24 * 60 * 60

func (k keys) AdjustSpent(ctx context.Context, meterID, id string, delta int64) (int64, bool, bool, error) {
	res, err := adjustScript.Run(ctx, k.rdb, []string{"adjust:" + id, "key:" + meterID},
		strconv.FormatInt(delta, 10), adjustKeep).Slice()
	if err != nil || len(res) != 2 {
		return 0, false, false, err
	}
	text, _ := res[0].(string)
	code, _ := res[1].(int64)
	spent, _ := strconv.ParseInt(text, 10, 64)
	return spent, code == 1, code != -1, nil
}

func flag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

type groups struct{ rdb *redis.Client }

// Get answers the zero value for a group that was never pushed or has been deleted, rather than an
// error, so a key pointing at one falls back to its own Allow list instead of 503ing.
func (g groups) Get(ctx context.Context, name string) (domain.GroupRecord, error) {
	if name == "" {
		return domain.GroupRecord{}, nil
	}
	h, err := g.rdb.HGetAll(ctx, "model_group:"+name).Result()
	if err != nil {
		return domain.GroupRecord{}, err
	}
	return domain.GroupRecord{Models: domain.ModelSet(h["models"])}, nil
}

func (g groups) Upsert(ctx context.Context, records []repository.GroupUpsert) error {
	for _, rec := range records {
		if rec.Name == "" {
			continue
		}
		if err := g.rdb.HSet(ctx, "model_group:"+rec.Name, map[string]any{
			"models": rec.Models,
		}).Err(); err != nil {
			return err
		}
	}
	return nil
}

// deletePrefixed is the only pruning path the admin API has — every other push upserts — so a
// revoked key keeps working on this box until one of these lands. A blank id would name the prefix
// itself, and DEL on "key:" is a different record.
func deletePrefixed(ctx context.Context, rdb *redis.Client, prefix string, ids []string) (int, error) {
	redisKeys := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			redisKeys = append(redisKeys, prefix+id)
		}
	}
	if len(redisKeys) == 0 {
		return 0, nil
	}
	return len(redisKeys), rdb.Del(ctx, redisKeys...).Err()
}
