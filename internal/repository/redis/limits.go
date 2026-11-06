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
// KEYS[i] a counter; ARGV[3i-2] its ceiling, ARGV[3i-1] '1' when a request counts on it,
// ARGV[3i] its TTL. → the indexes of the counters already at their ceiling.
var admitScript = redis.NewScript(`
local over = {}
for i, key in ipairs(KEYS) do
  if tonumber(redis.call('GET', key) or '0') >= tonumber(ARGV[3 * i - 2]) then
    over[#over + 1] = i
  end
end
if #over > 0 then
  return over
end
for i, key in ipairs(KEYS) do
  if ARGV[3 * i - 1] == '1' then
    redis.call('INCR', key)
    redis.call('EXPIRE', key, ARGV[3 * i])
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
			keys = append(keys, limitKey(holder, limit, label))
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
		exceeded = append(exceeded, counted[index-1])
	}
	return exceeded, nil
}

func (l limits) Debit(ctx context.Context, holders []domain.Holder, tokens int64, now time.Time) error {
	_, err := l.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, holder := range holders {
			for _, limit := range domain.LimitsOn(holder.Limits, domain.LimitTokens) {
				label, _, ttl := limit.Bucket(now)
				counter := limitKey(holder, limit, label)
				p.IncrBy(ctx, counter, tokens)
				p.Expire(ctx, counter, time.Duration(ttl)*time.Second)
			}
		}
		return nil
	})
	return err
}
