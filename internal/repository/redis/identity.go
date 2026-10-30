package redis

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/frappe/pathway/internal/domain"
	"github.com/frappe/pathway/internal/repository"
)

// key:<sha256(secret)>, team:<Grove Team> and model_group:<Model Group name>. The key carries
// its own policy — what it may call, where, how fast, and the cap it spends against — because a
// key lives in one geography and so on one store, which is what lets that store gate the cap
// exactly. The team is its own record so a verdict on the team is one write, not one per key; the
// group is its own record so a grant shared by many keys is one write.

type keys struct{ rdb *redis.Client }

// resolveScript follows a credential to its team and its groups in one call: which to read comes
// off the key record, so a pipeline could not join them. It only fetches; every decision over what
// comes back is made in Go. Replies {key, team, {name, group, ...}}, each record as HGETALL answers
// it and empty when absent.
//
// It reads team: and model_group: keys it is not handed, so the store must be a single Redis.
//
// KEYS[1] key:<meter id>
var resolveScript = redis.NewScript(`
local key = redis.call('HGETALL', KEYS[1])
local names, team = '', ''
for i = 1, #key, 2 do
  if key[i] == 'group' then
    names = key[i + 1]
  elseif key[i] == 'team' then
    team = key[i + 1]
  end
end
local teamrec = {}
if team ~= '' then
  teamrec = redis.call('HGETALL', 'team:' .. team)
end
local groups = {}
for name in string.gmatch(names, '[^,]+') do
  name = string.match(name, '^%s*(.-)%s*$')
  if name ~= '' then
    groups[#groups + 1] = name
    groups[#groups + 1] = redis.call('HGETALL', 'model_group:' .. name)
  end
end
return {key, teamrec, groups}
`)

func (k keys) Resolve(ctx context.Context, meterID string) (repository.Holder, bool, error) {
	res, err := resolveScript.Run(ctx, k.rdb, []string{"key:" + meterID}).Slice()
	if err != nil {
		return repository.Holder{}, false, err
	}
	if len(res) != 3 {
		return repository.Holder{}, false, fmt.Errorf("resolve answered %d records, want 3", len(res))
	}
	key := hashOf(res[0])
	if len(key) == 0 {
		return repository.Holder{}, false, nil
	}
	rec, err := keyRecord(key, time.Now())
	if err != nil {
		return repository.Holder{}, false, err
	}
	holder := repository.Holder{Key: rec, Groups: map[string]domain.GroupRecord{}}
	if team := hashOf(res[1]); len(team) > 0 {
		teamRec, err := teamRecord(team)
		if err != nil {
			return repository.Holder{}, false, err
		}
		holder.Team = &teamRec
	}
	groups, _ := res[2].([]any)
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

// spentFieldFor names the hash field a key's spend lands under. For a key capped by the month it
// is the month of now: a month not yet written reads 0, so a new month needs no reset, and earlier
// months' fields stay behind, ~20 bytes a key a month, with nothing reading them. For a key capped
// for life (domain.WindowTotal) it is the one field spent:total, which never turns over.
// ponytail: month fields accumulate one per key per month; HDEL the previous month on first write if it ever matters.
func spentFieldFor(window string, now time.Time) string {
	if window == domain.WindowTotal {
		return "spent:total"
	}
	return "spent:" + domain.MonthOf(now)
}

// keyRecord reads a record as HGETALL answered it, its Spent off the field its own window names.
func keyRecord(h map[string]string, now time.Time) (domain.KeyRecord, error) {
	// A push is refused unless every limit reads, so one that does not was written by a newer
	// binary. Serving that key uncapped would hide it.
	limits, err := domain.ParseLimits(h["limits"])
	if err != nil {
		return domain.KeyRecord{}, err
	}
	window := strings.TrimSpace(h["budget_window"])
	return domain.KeyRecord{
		Status:       h["status"],
		Team:         h["team"],
		KeyPrefix:    h["prefix"],
		Groups:       domain.ModelSet(h["group"]),
		Allow:        domain.ModelSet(h["allow"]),
		Deny:         domain.ModelSet(h["deny"]),
		Limited:      strings.TrimSpace(h["limited"]) == "1",
		LogPayloads:  strings.TrimSpace(h["log_payloads"]) == "1",
		Geography:    strings.TrimSpace(h["geography"]),
		Budget:       int64Field(h, "budget"),
		Spent:        int64Field(h, spentFieldFor(window, now)),
		BudgetWindow: window,
		Limits:       limits,
		CapMessage:   h["cap_message"],
	}, nil
}

// teamRecord reads a team as HGETALL answered it. A limit that does not read was written by a
// newer binary, and is refused the way a key's is.
func teamRecord(h map[string]string) (domain.TeamRecord, error) {
	limits, err := domain.ParseLimits(h["limits"])
	if err != nil {
		return domain.TeamRecord{}, err
	}
	return domain.TeamRecord{
		Limited:     strings.TrimSpace(h["limited"]) == "1",
		LogPayloads: strings.TrimSpace(h["log_payloads"]) == "1",
		Limits:      limits,
	}, nil
}

// writeTeam lands one team record whole.
func writeTeam(ctx context.Context, p redis.Pipeliner, rec repository.TeamUpsert) {
	p.HSet(ctx, "team:"+rec.Team, map[string]any{
		"limited":      flag(rec.Limited),
		"log_payloads": flag(rec.LogPayloads),
		"limits":       rec.Limits, // always written: blank is what clears a removed limit
	})
}

// keyFields is what a push writes. No `spent:<month>` is among them: those are this box's own
// counters, and an HSET here never clears a field it does not name.
func keyFields(rec repository.KeyUpsert) map[string]any {
	return map[string]any{
		"status":        rec.Status,
		"team":          rec.Team,
		"prefix":        rec.Prefix,
		"group":         rec.Groups, // comma list of group names
		"allow":         rec.Allow,
		"deny":          rec.Deny,
		"limited":       flag(rec.Limited),
		"log_payloads":  flag(rec.LogPayloads),
		"geography":     rec.Geography,
		"budget":        strconv.FormatInt(rec.Budget, 10),
		"budget_window": rec.BudgetWindow, // always written: blank (the month) is what clears a dropped total
		"limits":        rec.Limits,       // always written: blank is what clears a removed limit
		"cap_message":   rec.CapMessage,   // always written: blank is what clears a dropped message
	}
}

// staleKeyFields were written by control planes before the policy moved onto the key: the user
// pointer and the balance-read flag, and the flattened model set before that; and `spent`, the
// lifetime counter this gateway kept before spend was counted by month. Dropped with every write
// so a record never shows new fields beside stale ones.
var staleKeyFields = []string{"user", "can_read_balance", "models", "priority", "spent"}

// writeKey lands one record whole: the write and the strip of stale fields go in one
// transaction, so a reader never sees it half-updated.
func writeKey(ctx context.Context, p redis.Pipeliner, rec repository.KeyUpsert) {
	redisKey := "key:" + rec.MeterID
	p.HSet(ctx, redisKey, keyFields(rec))
	p.HDel(ctx, redisKey, staleKeyFields...)
}

// Upsert lands each record whole. A key that changed hands — held here under a team the push
// does not name — has its live buckets set aside in the same transaction, stamped with the team
// that was charged for them: a request lands wholly in the old team's bucket or wholly in a
// fresh one under the new record, so the control plane's books split exactly where the key moved.
func (k keys) Upsert(ctx context.Context, records []repository.KeyUpsert) error {
	held, err := k.teamsOf(ctx, records)
	if err != nil {
		return err
	}
	for i, rec := range records {
		if rec.MeterID == "" {
			continue
		}
		var closing []string
		if held[i] != "" && held[i] != rec.Team {
			if closing, err = liveBuckets(ctx, k.rdb, rec.Prefix+":*"); err != nil {
				return err
			}
		}
		id := repository.NewDrainID(time.Now())
		_, err = k.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
			writeKey(ctx, p, rec)
			for _, bucket := range closing {
				closeBook(ctx, p, id, bucket, held[i])
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// teamsOf is the team each record's key is held under on this store, "" for one not held: one
// round trip however many records.
func (k keys) teamsOf(ctx context.Context, records []repository.KeyUpsert) ([]string, error) {
	cmds, err := k.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, rec := range records {
			p.HGet(ctx, "key:"+rec.MeterID, "team")
		}
		return nil
	})
	if err != nil && err != redis.Nil {
		return nil, err
	}
	teams := make([]string, len(cmds))
	for i, cmd := range cmds {
		teams[i], _ = cmd.(*redis.StringCmd).Result()
	}
	return teams, nil
}

func (k keys) Delete(ctx context.Context, ids []string) (int, error) {
	return deletePrefixed(ctx, k.rdb, "key:", ids)
}

// int64Field reads a decimal field, 0 when absent — a record from before the field existed.
func int64Field(h map[string]string, field string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(h[field]), 10, 64)
	return n
}

// adjustScript moves one key's spend once per adjustment id, under the field the key's own window
// names. Replies {spent, code}: code 1 applied, 0 already applied, -1 no such key. The id is kept
// a week: a retry comes within minutes, and the control plane names each adjustment after its own
// row, so ids never recur.
//
// KEYS[1] adjust:<id>, KEYS[2] key:<meter id>; ARGV[1] delta, ARGV[2] seconds to keep the id,
// ARGV[3] the month's spent field (a total key's spend lands under spent:total instead)
var adjustScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[2]) == 0 then
  return {'0', -1}
end
local field = ARGV[3]
if redis.call('HGET', KEYS[2], 'budget_window') == 'total' then
  field = 'spent:total'
end
if redis.call('EXISTS', KEYS[1]) == 1 then
  return {redis.call('HGET', KEYS[2], field) or '0', 0}
end
local spent = redis.call('HINCRBY', KEYS[2], field, ARGV[1])
redis.call('SET', KEYS[1], '1', 'EX', ARGV[2])
return {tostring(spent), 1}
`)

const adjustKeep = 7 * 24 * 60 * 60

func (k keys) AdjustSpent(ctx context.Context, meterID, id string, delta int64) (int64, bool, bool, error) {
	res, err := adjustScript.Run(ctx, k.rdb, []string{"adjust:" + id, "key:" + meterID},
		strconv.FormatInt(delta, 10), adjustKeep, spentFieldFor(domain.WindowMonth, time.Now())).Slice()
	if err != nil || len(res) != 2 {
		return 0, false, false, err
	}
	text, _ := res[0].(string)
	code, _ := res[1].(int64)
	spent, _ := strconv.ParseInt(text, 10, 64)
	return spent, code == 1, code != -1, nil
}

// Spent reads every id's counter in one round trip, each off the field its window names: status
// tells a key this store holds from one it does not, and a field never written reads 0.
func (k keys) Spent(ctx context.Context, ids []string) (map[string]int64, error) {
	now := time.Now()
	month, total := spentFieldFor(domain.WindowMonth, now), spentFieldFor(domain.WindowTotal, now)
	cmds, err := pipelined(ctx, k.rdb, len(ids), func(p redis.Pipeliner, i int) {
		p.HMGet(ctx, "key:"+ids[i], "status", "budget_window", month, total)
	})
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for i, cmd := range cmds {
		vals := cmd.(*redis.SliceCmd).Val()
		if len(vals) != 4 || vals[0] == nil {
			continue
		}
		window, _ := vals[1].(string)
		text, _ := vals[2].(string)
		if strings.TrimSpace(window) == domain.WindowTotal {
			text, _ = vals[3].(string)
		}
		out[ids[i]], _ = strconv.ParseInt(text, 10, 64)
	}
	return out, nil
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
