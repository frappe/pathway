package redis

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/phot0n/pathway/internal/repository"
)

// usage:<key prefix> — one hash per API key, holding both the flat metrics and their per-model and
// per-deployment twins, so a single drain carries the aggregate and the breakdown together. The
// month is NOT tracked here: the control plane stamps it from its own clock when it pulls.
type usage struct{ rdb *redis.Client }

// accrueBody lands one request in one call: the counters, then the holder's lifetime spend and
// the balance view the drain carries. One script rather than a pipeline because user_spent needs
// the incremented total, and a drain landing between two calls would split one request across
// two pulls. The spend moves only on a holder the control plane has pushed: a legacy key names a
// user this box may never have been told about, and writing to that hash would fabricate a record
// that grants nothing. Values travel as strings — a Lua number is a double, and would round a
// nano-USD total past 14 digits on its way back into a command.
//
// ARGV[1] cost, ARGV[2] budget, ARGV[3..] field, value, ...
const accrueBody = `
local function accrue(usage, user)
  for i = 3, #ARGV, 2 do
    redis.call('HINCRBY', usage, ARGV[i], ARGV[i + 1])
  end
  if user == nil or redis.call('EXISTS', user) == 0 then
    return 0
  end
  redis.call('HINCRBY', user, 'spent', ARGV[1])
  local spent = redis.call('HGET', user, 'spent')
  redis.call('HSET', usage, 'user_spent', spent, 'user_balance', ARGV[2])
  if spent ~= '0' then
    redis.call('HINCRBY', usage, 'user_balance', '-' .. spent)
  end
  return 1
end
`

// KEYS[1] usage:<prefix>, KEYS[2] user:<name> (absent = no holder)
var accrueScript = redis.NewScript(accrueBody + `return accrue(KEYS[1], KEYS[2])`)

func accrueArgs(a repository.Accrual) []any {
	args := []any{a.Cost, a.Budget}
	for field, n := range a.Fields {
		args = append(args, field, strconv.FormatInt(n, 10))
	}
	return args
}

func (u usage) Accrue(ctx context.Context, a repository.Accrual) error {
	if a.Prefix == "" || len(a.Fields) == 0 {
		return nil
	}
	keys := []string{"usage:" + a.Prefix}
	if a.User != "" {
		keys = append(keys, "user:"+a.User)
	}
	return accrueScript.Run(ctx, u.rdb, keys, accrueArgs(a)...).Err()
}

// A counter set aside lives under drained:<id>:<prefix>, and "<id>:<prefix>" sits in drain:unacked
// until the control plane acknowledges it. A drain id carries no ':', so the first ':' splits it.
const unackedKey = "drain:unacked"

// setAsideScript moves one live counter into a drain. Redis is single-threaded, so a request
// metered mid-drain lands wholly before the RENAME or wholly on a fresh usage:<prefix>. It refuses
// when the prefix is already set aside under this id — SCAN can return a key twice, and the second
// time it may be a fresh counter that belongs to the next drain.
//
// KEYS[1] usage:<prefix>, KEYS[2] drained:<id>:<prefix>, KEYS[3] drain:unacked; ARGV[1] "<id>:<prefix>"
var setAsideScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 or redis.call('EXISTS', KEYS[2]) == 1 then
  return 0
end
redis.call('RENAME', KEYS[1], KEYS[2])
redis.call('SADD', KEYS[3], ARGV[1])
return 1
`)

// ackScript keeps each acknowledged pair for ARGV[1] seconds, then lets it expire. A pair not in
// drain:unacked moves nothing.
//
// KEYS[1] drain:unacked; ARGV[1] seconds, ARGV[2..] "<id>:<prefix>"
var ackScript = redis.NewScript(`
local count = 0
for i = 2, #ARGV do
  if redis.call('SREM', KEYS[1], ARGV[i]) == 1 then
    redis.call('EXPIRE', 'drained:' .. ARGV[i], ARGV[1])
    count = count + 1
  end
end
return count
`)

// Drain sets live counters aside under newID — all of them, or only keys — and answers every pair
// still unacknowledged. Nothing is deleted here: a key stays until its Ack, so one the control
// plane fails to record comes back under its own id while the others move on.
func (u usage) Drain(ctx context.Context, newID string, keys []string) (repository.Drains, error) {
	live := keys
	if len(keys) == 0 {
		var err error
		if live, err = u.scanLive(ctx); err != nil {
			return nil, err
		}
	}
	for _, prefix := range live {
		member := newID + ":" + prefix
		scriptKeys := []string{"usage:" + prefix, "drained:" + member, unackedKey}
		if err := setAsideScript.Run(ctx, u.rdb, scriptKeys, member).Err(); err != nil {
			return nil, err
		}
	}
	return u.read(ctx, keys)
}

func (u usage) scanLive(ctx context.Context) ([]string, error) {
	var live []string
	var cursor uint64
	for {
		found, next, err := u.rdb.Scan(ctx, cursor, "usage:*", 200).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range found {
			live = append(live, strings.TrimPrefix(key, "usage:"))
		}
		if cursor = next; cursor == 0 {
			return live, nil
		}
	}
}

// read answers every unacknowledged pair, only those of keys when it is non-empty.
func (u usage) read(ctx context.Context, keys []string) (repository.Drains, error) {
	members, err := u.rdb.SMembers(ctx, unackedKey).Result()
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, key := range keys {
		wanted[key] = true
	}
	out := repository.Drains{}
	for _, member := range members {
		id, prefix, ok := strings.Cut(member, ":")
		if !ok || (len(wanted) > 0 && !wanted[prefix]) {
			continue
		}
		h, err := u.rdb.HGetAll(ctx, "drained:"+member).Result()
		if err != nil {
			return nil, err
		}
		if len(h) == 0 {
			continue
		}
		if out[id] == nil {
			out[id] = map[string]map[string]string{}
		}
		out[id][prefix] = h
	}
	return out, nil
}

func (u usage) Ack(ctx context.Context, acks map[string][]string, retention time.Duration) (int, error) {
	seconds := int64(retention / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	args := []any{seconds}
	for id, prefixes := range acks {
		for _, prefix := range prefixes {
			args = append(args, id+":"+prefix)
		}
	}
	if len(args) == 1 {
		return 0, nil
	}
	return ackScript.Run(ctx, u.rdb, []string{unackedKey}, args...).Int()
}
