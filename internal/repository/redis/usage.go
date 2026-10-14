package redis

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/phot0n/pathway/internal/repository"
)

// usage:<key prefix>:<YYYY-MM-DD> — one hash per API key per UTC day it was served
// (repository.UsageBucket), holding both the flat metrics and their per-model and per-deployment
// twins, so a single drain carries the aggregate and the breakdown together. The day is named off
// the same clock as the field the key's spend lands under (spentField), and the control plane
// records the counters on it: its day and month are this store's own.
type usage struct{ rdb *redis.Client }

// accrueBody lands one request in one call: the counters, then the key's spend for the month and
// the balance view the drain carries. One script rather than a pipeline because key_spent needs
// the incremented total, and a drain landing between two calls would split one request across two
// pulls. The spend moves only on a key the control plane has pushed: writing to an absent hash
// would fabricate a record that grants nothing. Values travel as strings — a Lua number is a
// double, and would round a nano-USD total past 14 digits on its way back into a command.
//
// ARGV[1] cost, ARGV[2] budget, ARGV[3] the key's spent field for the month, ARGV[4..] field, value, ...
const accrueBody = `
local function accrue(usage, key)
  for i = 4, #ARGV, 2 do
    redis.call('HINCRBY', usage, ARGV[i], ARGV[i + 1])
  end
  if key == nil or redis.call('EXISTS', key) == 0 then
    return 0
  end
  redis.call('HINCRBY', key, ARGV[3], ARGV[1])
  local spent = redis.call('HGET', key, ARGV[3])
  redis.call('HSET', usage, 'key_spent', spent, 'key_balance', ARGV[2])
  if spent ~= '0' then
    redis.call('HINCRBY', usage, 'key_balance', '-' .. spent)
  end
  return 1
end
`

// KEYS[1] usage:<bucket>, KEYS[2] key:<meter id> (absent = not charged)
var accrueScript = redis.NewScript(accrueBody + `return accrue(KEYS[1], KEYS[2])`)

// replayScript is accrue behind a once-per-request marker, kept a week: a spool pass that dies
// after landing a line lands it again next pass, and the marker makes that a no-op. → -1 when
// the marker was already there.
//
// KEYS[1] accrued:<request id>, KEYS[2] usage:<bucket>, KEYS[3] key:<meter id> (absent = not charged)
var replayScript = redis.NewScript(accrueBody + `
if not redis.call('SET', KEYS[1], '1', 'NX', 'EX', 604800) then
  return -1
end
accrue(KEYS[2], KEYS[3])
return 1`)

// accrueArgs stamps the spend with the month it lands in, off the now that named the bucket. A
// spooled accrual replayed after the day turned counts in the new one, day and month alike.
func accrueArgs(a repository.Accrual, now time.Time) []any {
	args := []any{a.Cost, a.Budget, spentField(now)}
	for field, n := range a.Fields {
		args = append(args, field, strconv.FormatInt(n, 10))
	}
	return args
}

func (u usage) Accrue(ctx context.Context, a repository.Accrual) error {
	if a.Prefix == "" || len(a.Fields) == 0 {
		return nil
	}
	now := time.Now()
	keys := []string{"usage:" + repository.UsageBucket(a.Prefix, now)}
	if a.Key != "" {
		keys = append(keys, "key:"+a.Key)
	}
	return accrueScript.Run(ctx, u.rdb, keys, accrueArgs(a, now)...).Err()
}

func (u usage) Replay(ctx context.Context, a repository.Accrual) (bool, error) {
	if a.Prefix == "" || len(a.Fields) == 0 {
		return false, nil
	}
	now := time.Now()
	keys := []string{"accrued:" + a.ID, "usage:" + repository.UsageBucket(a.Prefix, now)}
	if a.Key != "" {
		keys = append(keys, "key:"+a.Key)
	}
	landed, err := replayScript.Run(ctx, u.rdb, keys, accrueArgs(a, now)...).Int()
	return landed == 1, err
}

func (u usage) Ping(ctx context.Context) error { return u.rdb.Ping(ctx).Err() }

// A counter set aside lives under drained:<id>:<bucket>, and "<id>:<bucket>" sits in drain:unacked
// until the control plane acknowledges it. A drain id carries no ':', so the first ':' splits it.
const unackedKey = "drain:unacked"

// prefixes is the keys a drain or a read is narrowed to, matched on each bucket's prefix. None
// means every key.
type prefixes map[string]bool

func prefixesOf(keys []string) prefixes {
	p := prefixes{}
	for _, key := range keys {
		p[key] = true
	}
	return p
}

func (p prefixes) has(bucket string) bool {
	prefix, _ := repository.SplitBucket(bucket)
	return len(p) == 0 || p[prefix]
}

// setAsideScript moves one live counter into a drain. Redis is single-threaded, so a request
// metered mid-drain lands wholly before the RENAME or wholly on a fresh usage:<bucket>. It refuses
// when the bucket is already set aside under this id — SCAN can return a key twice, and the second
// time it may be a fresh counter that belongs to the next drain.
//
// KEYS[1] usage:<bucket>, KEYS[2] drained:<id>:<bucket>, KEYS[3] drain:unacked; ARGV[1] "<id>:<bucket>"
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
// KEYS[1] drain:unacked; ARGV[1] seconds, ARGV[2..] "<id>:<bucket>"
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

// Drain sets live counters aside under newID — all of them, or only keys' — and answers every pair
// still unacknowledged. One SCAN either way: which days a key has live, only the store knows.
// Nothing is deleted here: a bucket stays until its Ack, so one the control plane fails to record
// comes back under its own id while the others move on.
func (u usage) Drain(ctx context.Context, newID string, keys []string) (repository.Drains, error) {
	live, err := u.scanLive(ctx)
	if err != nil {
		return nil, err
	}
	want := prefixesOf(keys)
	for _, bucket := range live {
		if !want.has(bucket) {
			continue
		}
		member := newID + ":" + bucket
		scriptKeys := []string{"usage:" + bucket, "drained:" + member, unackedKey}
		if err := setAsideScript.Run(ctx, u.rdb, scriptKeys, member).Err(); err != nil {
			return nil, err
		}
	}
	return u.read(ctx, want)
}

// Peek reads what Drain would answer and moves nothing: the unacknowledged pairs, then every live
// bucket under the id "". The control plane holds its own pull lock while it peeks, so no drain of
// its renames a bucket between the two reads.
func (u usage) Peek(ctx context.Context, keys []string) (repository.Drains, error) {
	want := prefixesOf(keys)
	out, err := u.read(ctx, want)
	if err != nil {
		return nil, err
	}
	live, err := u.scanLive(ctx)
	if err != nil {
		return nil, err
	}
	for _, bucket := range live {
		if !want.has(bucket) {
			continue
		}
		h, err := u.rdb.HGetAll(ctx, "usage:"+bucket).Result()
		if err != nil {
			return nil, err
		}
		if len(h) == 0 {
			continue
		}
		prefix, day := repository.SplitBucket(bucket)
		out.Put("", prefix, day, h)
	}
	return out, nil
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

// read answers every unacknowledged pair, only the wanted keys' when any are named.
func (u usage) read(ctx context.Context, want prefixes) (repository.Drains, error) {
	members, err := u.rdb.SMembers(ctx, unackedKey).Result()
	if err != nil {
		return nil, err
	}
	out := repository.Drains{}
	for _, member := range members {
		id, bucket, ok := strings.Cut(member, ":")
		if !ok || !want.has(bucket) {
			continue
		}
		h, err := u.rdb.HGetAll(ctx, "drained:"+member).Result()
		if err != nil {
			return nil, err
		}
		if len(h) == 0 {
			continue
		}
		prefix, day := repository.SplitBucket(bucket)
		out.Put(id, prefix, day, h)
	}
	return out, nil
}

func (u usage) Ack(ctx context.Context, acks repository.Acks, retention time.Duration) (int, error) {
	seconds := int64(retention / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	args := []any{seconds}
	for id, keys := range acks {
		for prefix, days := range keys {
			for _, day := range days {
				args = append(args, id+":"+repository.Bucket(prefix, day))
			}
		}
	}
	if len(args) == 1 {
		return 0, nil
	}
	return ackScript.Run(ctx, u.rdb, []string{unackedKey}, args...).Int()
}
