// Package metering records what a finished request cost and how its hop behaved. Both run on every
// admitted request, including one the client abandoned.
package metering

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
)

// Report is one finished request, as the proxy saw it.
type Report struct {
	RequestID string // the id the gateway stamped: what makes a spooled replay land once
	Prefix    string // API Key doc name — which bucket this accrues to, and whose limits are debited
	MeterID   string // the key's record id — whose spend moves
	Model     string // from the request body; buckets per-model usage
	// Deployment is which placement actually served it. On a direct route the gateway already
	// knows; on an ingress route only the ingress does, and it says so in a response header.
	Deployment string
	// Usage is the raw JSON captured from the response — the final streaming frame or the whole
	// non-streaming body. May be empty.
	Usage string
	// UsageStart is the first usage line of a response that carried more than one. May be empty.
	UsageStart string
	// What it cost: the pricing that charges it (nil = unpriced), and the cap the key's balance
	// is reported against. Only a prepaid key is charged: a free one's spend never moves, so a
	// team turned prepaid later starts at what it loads, not in debt for what was free.
	Pricing *domain.Pricing
	Prepaid bool
	Budget  int64
	// Limits is the key's rate limits: the answer's tokens are debited from the token ones.
	Limits []domain.Limit
	// How the hop went, for passive ejection: the upstream's status, and the X-Grove-Reason an
	// ingress sets when it is healthy but has no replica for this model.
	Target         string
	UpstreamStatus string
	Reason         string
	// Cut names who ended a response that did not finish; blank on one that did.
	Cut string
}

type Service struct {
	usage  repository.Usage
	limits repository.Limits
	health repository.Health
	log    *slog.Logger
	warned sync.Map // models whose cache buckets were once seen exceeding the prompt; pricings pushed without a table
	// Spool keeps what the store refused, for replay once it answers. Nil keeps nothing.
	Spool *Spool
	// Now is the clock the limit windows are read off. A field so a test can move it.
	Now func() time.Time
}

func New(usage repository.Usage, limits repository.Limits, health repository.Health, log *slog.Logger) *Service {
	return &Service{usage: usage, limits: limits, health: health, log: log, Now: time.Now}
}

// debit charges the answer's tokens to the key's token limits, in the window the answer ended
// in. Never spooled: replayed later, it would land in a window the tokens were not used in.
func (s *Service) debit(ctx context.Context, rep Report, tokens int64) {
	limits := domain.LimitsOn(rep.Limits, domain.LimitTokens)
	if tokens == 0 || len(limits) == 0 {
		return
	}
	if err := s.limits.Debit(ctx, rep.Prefix, limits, tokens, s.Now()); err != nil {
		s.log.Error("token limits not debited", "key", rep.Prefix, "tokens", tokens, "err", err)
	}
}

// Record writes the usage delta and moves the target's failure count. Errors are logged, never
// returned to the caller: the request is over, and failing it retroactively helps nobody.
func (s *Service) Record(ctx context.Context, rep Report) {
	s.recordOutcome(ctx, rep)
	if rep.Prefix == "" {
		// No bucket to accrue to. The outcome above still counted — how a target behaves is not
		// contingent on the usage record that happened to ride along with the report.
		return
	}
	fields, trusted := UsageFields(rep)
	if !trusted {
		if _, seen := s.warned.LoadOrStore(rep.Model, true); !seen {
			s.log.Warn("cache buckets exceed the prompt; counted as plain", "model", rep.Model)
		}
	}
	if rep.Pricing != nil && len(rep.Pricing.Counters) == 0 {
		// The control plane always pushes one; a pricing without it prices nothing, which the pull
		// would read as a free request. Say so, once per pricing.
		if _, seen := s.warned.LoadOrStore("pricing:"+rep.Pricing.ID, true); !seen {
			s.log.Error("pricing carries no counter table; nothing priced", "pricing", rep.Pricing.ID, "model", rep.Model)
		}
	}
	cost := PricedFields(fields, rep.Pricing, rep.Prepaid)
	accrual := repository.Accrual{ID: rep.RequestID, Prefix: rep.Prefix, Fields: fields, Cost: cost}
	if rep.Prepaid {
		accrual.Key, accrual.Budget = rep.MeterID, rep.Budget
	}
	if err := s.usage.Accrue(ctx, accrual); err != nil {
		s.log.Error("usage not recorded; spooled", "prefix", rep.Prefix, "model", rep.Model, "err", err)
		if s.Spool != nil {
			s.Spool.Add(accrual)
		}
	}
	s.debit(ctx, rep, fields["total_tokens"])
}

// UsageFields is the whole accounting rule, pure so it is testable without a store. Each metric is
// written flat and, when known, as m:<metric>:<model> and m:<metric>:<deployment> in the SAME hash,
// so one drain carries aggregate and breakdown. Zero values are skipped. Beside the display fields
// it emits the counters the control plane prices: the root buckets the response fills — the prompt
// and the completion whole, and the parts of each that have a rate of their own (cached, written,
// audio) — each landed under its derived counter when the pricing's table holds one for this
// prompt size, whatever the pricing's rates. trusted is false when the cache buckets exceeded the
// prompt: the whole prompt is then plain, since cache credit is the one thing such a response
// cannot be trusted on.
func UsageFields(rep Report) (fields map[string]int64, trusted bool) {
	model := strings.TrimSpace(rep.Model)
	deployment := strings.TrimSpace(rep.Deployment)
	fields = map[string]int64{}

	bump := func(metric string, n int64) {
		if n == 0 {
			return
		}
		fields[metric] += n
		if model != "" {
			fields["m:"+metric+":"+model] += n
		}
		// Beside the per-model bucket, in the same hash. This is the only path by which usage
		// reaches a placement the gateway never chose — on an ingress route the replica was picked
		// a tier below.
		if deployment != "" && deployment != model {
			fields["m:"+metric+":"+deployment] += n
		}
	}

	bump("request_count", 1)
	u, ok := domain.ParseUsage([]byte(rep.Usage))
	// A first line with nothing to read (OpenAI's "usage":null chunks) is ignored.
	if start, split := domain.ParseUsage([]byte(rep.UsageStart)); split {
		u, ok = domain.MergeUsage(start, u), true
	}
	if !ok {
		return fields, true
	}
	trusted = u.Cached+u.CacheWrite <= u.Prompt
	if !trusted {
		u.Cached, u.CacheWrite, u.CacheWrite1h = 0, 0, 0
	}
	// Audio is billed out of what the cache left: a cached audio token is credited, not billed twice.
	audio := max(0, min(u.Audio, u.Prompt-u.Cached-u.CacheWrite))
	completionAudio := max(0, min(u.CompletionAudio, u.Completion))
	buckets := map[string]int64{
		"prompt_tokens": int64(u.Prompt), "cached_tokens": int64(u.Cached),
		"cache_write_tokens": int64(u.CacheWrite - u.CacheWrite1h), "cache_write_1h_tokens": int64(u.CacheWrite1h),
		"audio_tokens": int64(audio), "completion_tokens": int64(u.Completion), "completion_audio_tokens": int64(completionAudio),
	}
	for counter, n := range landed(buckets, rep.Pricing.Table(), int64(u.Prompt)) {
		bump(counter, n)
	}
	bump("total_tokens", int64(u.Total))
	bump("audio_seconds", int64(u.Seconds))
	return fields, trusted
}

// landed moves each root bucket to the counter it is counted under for a prompt this size. A part
// goes to its variant when the table holds one that applies, else stays. A container's variant
// takes what is left after the parts that stayed, which keep their place in the base container:
// hour writes and audio have one rate at any prompt size, so a long request's are counted in
// prompt_tokens and only the rest above the threshold.
func landed(buckets map[string]int64, table domain.CounterTable, prompt int64) map[string]int64 {
	out := map[string]int64{}
	for counter, n := range buckets {
		if len(table.Parts(counter)) > 0 {
			continue // a container: placed once its parts are known
		}
		if variant := table.Variant(counter, prompt); variant != "" {
			out[variant] += n
		} else {
			out[counter] += n
		}
	}
	for counter, n := range buckets {
		parts := table.Parts(counter)
		if len(parts) == 0 {
			continue
		}
		var stayed int64
		for _, part := range parts {
			if table.Variant(part, prompt) == "" {
				stayed += buckets[part]
			}
		}
		if variant := table.Variant(counter, prompt); variant != "" {
			out[variant] += n - stayed
			out[counter] += stayed
		} else {
			out[counter] += n
		}
	}
	return out
}

// PricedFields prices the request and tags what it priced with the pricing's id: each priced
// counter again as p:<id>:<counter>, and the cost as p:<id>:cost beside the flat one. The pull prices
// each p:<id> group with that same pricing's table, so a switch mid-drain never reads as drift.
// A request that is not charged is tagged f: instead, so the pull bills exactly what moved a spend.
// → the cost in nano-USD.
func PricedFields(fields map[string]int64, pricing *domain.Pricing, charged bool) int64 {
	if pricing == nil || pricing.ID == "" {
		return 0
	}
	table := pricing.Table()
	cost := table.Cost(fields, pricing.Rates)
	tag := "f:" + pricing.ID + ":"
	if charged {
		tag = "p:" + pricing.ID + ":"
	}
	for _, counter := range table {
		if n := fields[counter.Name]; n > 0 {
			fields[tag+counter.Name] = n
		}
	}
	if cost != 0 {
		fields["cost"] = cost
		fields[tag+"cost"] = cost
	}
	return cost
}

// recordOutcome moves one target's consecutive-failure count. A success clears it outright.
func (s *Service) recordOutcome(ctx context.Context, rep Report) {
	if rep.Target == "" {
		return
	}
	var err error
	switch {
	case domain.IsHopFailure(rep.UpstreamStatus, rep.Reason, rep.Cut):
		err = s.health.RecordFailure(ctx, rep.Target)
		s.log.Debug("hop failed", "target", rep.Target, "status", rep.UpstreamStatus, "reason", rep.Reason)
	case domain.IsHopSuccess(rep.UpstreamStatus):
		err = s.health.RecordSuccess(ctx, rep.Target)
	default:
		return
	}
	if err != nil {
		s.log.Warn("health counter not moved", "target", rep.Target, "err", err)
	}
}
