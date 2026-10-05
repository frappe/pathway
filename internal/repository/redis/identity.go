package redis

import (
	"context"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
)

// key:<sha256(secret)>, user:<Grove User name>, model_group:<Model Group name>. Three records
// rather than one projection on the credential, so one leaked key dies without touching the rest
// and a budget flip is one write however many keys the holder has.

type keys struct{ rdb *redis.Client }

func (k keys) Get(ctx context.Context, meterID string) (domain.KeyRecord, bool, error) {
	h, err := k.rdb.HGetAll(ctx, "key:"+meterID).Result()
	if err != nil {
		return domain.KeyRecord{}, false, err
	}
	if len(h) == 0 {
		return domain.KeyRecord{}, false, nil
	}
	group, hasGroup := h["group"] // present-but-blank = ungrouped; absent = a pre-group record
	rec := domain.KeyRecord{
		Status:    h["status"],
		User:      h["user"],
		KeyPrefix: h["prefix"],
		Legacy: domain.LegacyKey{
			HasGroup: hasGroup,
			Group:    strings.TrimSpace(group),
			Allow:    domain.ModelSet(h["allow"]),
			Deny:     domain.ModelSet(h["deny"]),
			Models:   domain.ModelSet(h["models"]),
		},
	}
	// Status used to carry the holder's budget flag as a third value. Lift it off here, so Status
	// means only "is this credential live" — which is all a current record puts there.
	if rec.Status == "rate_limited" {
		rec.Status, rec.Legacy.Limited = "active", true
	}
	return rec, true, nil
}

func (k keys) Upsert(ctx context.Context, records []repository.KeyUpsert) error {
	for _, rec := range records {
		if rec.MeterID == "" {
			continue
		}
		redisKey := "key:" + rec.MeterID
		// One transaction, so a reader never sees this record half-updated: the write and the strip
		// below land together or not at all. Two commands left a window showing new fields beside
		// stale legacy ones — inert today, a torn read regardless, and free to fix.
		_, err := k.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.HSet(ctx, redisKey, map[string]any{
				"status": rec.Status,
				"user":   rec.User,
				"prefix": rec.Prefix,
			})
			// A pre-group control plane flattened access onto the key; that set is stale the moment a
			// group is pushed. `group`/`allow`/`deny` stay: this cannot tell a current push from an
			// older plane still writing them, and dropping them there would leave no access at all.
			p.HDel(ctx, redisKey, "models", "priority")
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

type users struct{ rdb *redis.Client }

func (u users) Get(ctx context.Context, name string) (domain.UserRecord, bool, error) {
	h, err := u.rdb.HGetAll(ctx, "user:"+name).Result()
	if err != nil {
		return domain.UserRecord{}, false, err
	}
	if len(h) == 0 {
		return domain.UserRecord{}, false, nil
	}
	// A push is refused unless every limit reads, so one that does not was written by a newer
	// binary. Serving that holder uncapped would hide it.
	limits, err := domain.ParseLimits(h["limits"])
	if err != nil {
		return domain.UserRecord{}, false, err
	}
	return domain.UserRecord{
		Limits:      limits,
		Email:       h["email"],
		Groups:      domain.ModelSet(h["group"]),
		Allow:       domain.ModelSet(h["allow"]),
		Deny:        domain.ModelSet(h["deny"]),
		Limited:     strings.TrimSpace(h["limited"]) == "1",
		LogPayloads: strings.TrimSpace(h["log_payloads"]) == "1",
		Geography:   strings.TrimSpace(h["geography"]),
		Prepaid:     strings.TrimSpace(h["prepaid"]) == "1",
		Budget:      int64Field(h, "budget"),
		Spent:       int64Field(h, "spent"),
	}, true, nil
}

func (u users) Upsert(ctx context.Context, records []repository.UserUpsert) error {
	for _, rec := range records {
		if rec.Name == "" {
			continue
		}
		if err := u.rdb.HSet(ctx, "user:"+rec.Name, userFields(rec)).Err(); err != nil {
			return err
		}
	}
	return nil
}

// userFields is what a push writes. `spent` is not among them: it is this box's own counter, and
// an HSET here never clears a field it does not name.
func userFields(rec repository.UserUpsert) map[string]any {
	return map[string]any{
		"email":        rec.Email,
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

// int64Field reads a decimal field, 0 when absent — a record from before the field existed.
func int64Field(h map[string]string, field string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(h[field]), 10, 64)
	return n
}

func (u users) Delete(ctx context.Context, ids []string) (int, error) {
	return deletePrefixed(ctx, u.rdb, "user:", ids)
}

// adjustScript moves one holder's spend once per adjustment id. Replies {spent, code}: code 1
// applied, 0 already applied, -1 no such holder. The id is kept a week: a retry comes within
// minutes, and the control plane names each adjustment after its own row, so ids never recur.
//
// KEYS[1] adjust:<id>, KEYS[2] user:<name>; ARGV[1] delta, ARGV[2] seconds to keep the id
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

func (u users) AdjustSpent(ctx context.Context, name, id string, delta int64) (int64, bool, bool, error) {
	res, err := adjustScript.Run(ctx, u.rdb, []string{"adjust:" + id, "user:" + name},
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
