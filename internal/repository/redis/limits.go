package redis

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/frappe/pathway/internal/domain"
)

// lim:<kind>:<id>:<model>:<metric>:<window>:<bucket> — one counter per holder, model and limit
// window. Every gateway on this store counts into the same ones, so a limit is exact across them.
type limits struct{ rdb *redis.Client }

func limitKey(holder domain.Holder, limit domain.Limit, label string) string {
	return "lim:" + holder.Counter() + ":" + limit.Metric + ":" + limit.Window + ":" + label
}

// admitScript checks every counter and, only when all have room, counts the request on those that
// count requests. One script, so two requests cannot both take the last slot, and a refusal counts
// nothing.
//
// KEYS[2i-1] a counter, KEYS[2i] the next window's; ARGV[3i-2] its ceiling, ARGV[3i-1] '1' when a
// request counts on it, ARGV[3i] its TTL. → the indexes of the counters already at their ceiling,
// negative where the next window's is too.
var admitScript = redis.NewScript(`
local over = {}
for i = 1, #KEYS / 2 do
  local ceiling = tonumber(ARGV[3 * i - 2])
  if tonumber(redis.call('GET', KEYS[2 * i - 1]) or '0') >= ceiling then
    if tonumber(redis.call('GET', KEYS[2 * i]) or '0') >= ceiling then
      over[#over + 1] = -i
    else
      over[#over + 1] = i
    end
  end
end
if #over > 0 then
  return over
end
for i = 1, #KEYS / 2 do
  if ARGV[3 * i - 1] == '1' then
    redis.call('INCR', KEYS[2 * i - 1])
    redis.call('EXPIRE', KEYS[2 * i - 1], ARGV[3 * i])
  end
end
return over`)

func (l limits) Admit(ctx context.Context, holders []domain.Holder, now time.Time) ([]domain.Over, error) {
	var keys []string
	var args []any
	var counted []domain.Over
	for _, holder := range holders {
		for _, limit := range holder.Limits {
			label, _, ttl := limit.Bucket(now)
			next, _, _ := limit.Bucket(limit.Next(now))
			keys = append(keys, limitKey(holder, limit, label), limitKey(holder, limit, next))
			args = append(args, limit.Value, flag(limit.Metric == domain.LimitRequests), ttl)
			counted = append(counted, domain.Over{Holder: holder, Limit: limit})
		}
	}
	over, err := admitScript.Run(ctx, l.rdb, keys, args...).Int64Slice()
	if err != nil {
		return nil, err
	}
	exceeded := make([]domain.Over, 0, len(over))
	for _, index := range over {
		spent := counted[max(index, -index)-1]
		spent.Carried = index < 0
		exceeded = append(exceeded, spent)
	}
	return exceeded, nil
}

// debitScript adds the tokens to each counter, and to the next window's the part of them that is
// past the ceiling. One script: the part is read off the count this debit made.
//
// KEYS[2i-1] a counter, KEYS[2i] the next window's; ARGV[1] the tokens, ARGV[2i] the ceiling,
// ARGV[2i+1] the TTL of both.
var debitScript = redis.NewScript(`
local tokens = tonumber(ARGV[1])
for i = 1, #KEYS / 2 do
  local used = redis.call('INCRBY', KEYS[2 * i - 1], tokens)
  redis.call('EXPIRE', KEYS[2 * i - 1], ARGV[2 * i + 1])
  local over = math.min(tokens, used - tonumber(ARGV[2 * i]))
  if over > 0 then
    redis.call('INCRBY', KEYS[2 * i], over)
    redis.call('EXPIRE', KEYS[2 * i], ARGV[2 * i + 1])
  end
end
return 0`)

func (l limits) Debit(ctx context.Context, holders []domain.Holder, tokens int64, now time.Time) error {
	var keys []string
	args := []any{tokens}
	for _, holder := range holders {
		for _, limit := range domain.LimitsOn(holder.Limits, domain.LimitTokens) {
			label, _, ttl := limit.Bucket(now)
			next, _, _ := limit.Bucket(limit.Next(now))
			keys = append(keys, limitKey(holder, limit, label), limitKey(holder, limit, next))
			args = append(args, limit.Value, ttl)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return debitScript.Run(ctx, l.rdb, keys, args...).Err()
}
