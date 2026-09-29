// Package metering records what a finished request cost and how its hop behaved. Both run on every
// admitted request, including one the client abandoned.
package metering

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
)

// Report is one finished request, as the proxy saw it.
type Report struct {
	RequestID string // the id the gateway stamped: what makes a spooled replay land once
	Prefix    string // API Key doc name — which bucket this accrues to
	Model     string // from the request body; buckets per-model usage
	// Deployment is which placement actually served it. On a direct route the gateway already
	// knows; on an ingress route only the ingress does, and it says so in a response header.
	Deployment string
	// Usage is the raw JSON captured from the response — the final streaming frame or the whole
	// non-streaming body. May be empty.
	Usage string
	// What it cost and whom it cost: the pricing that charges it (nil = unpriced), and the holder
	// whose lifetime spend moves, with the amount they loaded that their balance is reported against.
	// Only a prepaid holder is charged: a free one's spend never moves, so turning them prepaid
	// later starts them at what they load, not in debt for what was free.
	Pricing *domain.Pricing
	User    string
	Prepaid bool
	Budget  int64
	// How the hop went, for passive ejection: the upstream's status, and the X-Grove-Reason an
	// ingress sets when it is healthy but has no replica for this model.
	Target         string
	UpstreamStatus string
	Reason         string
}

type Service struct {
	usage  repository.Usage
	health repository.Health
	log    *slog.Logger
	warned sync.Map // models whose cache buckets were once seen exceeding the prompt
	// Spool keeps what the store refused, for replay once it answers. Nil keeps nothing.
	Spool *Spool
}

func New(usage repository.Usage, health repository.Health, log *slog.Logger) *Service {
	return &Service{usage: usage, health: health, log: log}
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
	cost := PricedFields(fields, rep.Pricing)
	accrual := repository.Accrual{ID: rep.RequestID, Prefix: rep.Prefix, Fields: fields, Cost: cost}
	if rep.Prepaid {
		accrual.User, accrual.Budget = rep.User, rep.Budget
	}
	if err := s.usage.Accrue(ctx, accrual); err != nil {
		s.log.Error("usage not recorded; spooled", "prefix", rep.Prefix, "model", rep.Model, "err", err)
		if s.Spool != nil {
			s.Spool.Add(accrual)
		}
	}
}

// UsageFields is the whole accounting rule, pure so it is testable without a store. Each metric is
// written flat and, when known, as m:<metric>:<model> and m:<metric>:<deployment> in the SAME hash,
// so one drain carries aggregate and breakdown. Zero values are skipped. Beside the display fields
// it emits the seven counters the control plane prices, the prompt split into plain, cached,
// written and audio. trusted is false when the cache buckets exceeded the prompt: the whole prompt is then
// plain, since cache credit is the one thing such a response cannot be trusted on.
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
	if !ok {
		return fields, true
	}
	trusted = u.Cached+u.CacheWrite <= u.Prompt
	if !trusted {
		u.Cached, u.CacheWrite, u.CacheWrite1h = 0, 0, 0
	}
	// Audio is billed out of what the cache left: a cached audio token is credited, not billed twice.
	audio := max(0, min(u.Audio, u.Prompt-u.Cached-u.CacheWrite))
	bump("prompt_tokens", int64(u.Prompt))
	bump("completion_tokens", int64(u.Completion))
	bump("total_tokens", int64(u.Total))
	bump("input_tokens", int64(u.Prompt-u.Cached-u.CacheWrite-audio))
	bump("cached_tokens", int64(u.Cached))
	bump("cache_write_tokens", int64(u.CacheWrite-u.CacheWrite1h))
	bump("cache_write_1h_tokens", int64(u.CacheWrite1h))
	bump("audio_tokens", int64(audio))
	bump("audio_seconds", int64(u.Seconds))
	return fields, trusted
}

// PricedFields prices the request and tags what it charged with the pricing's id: each priced
// counter again as p:<id>:<counter>, and the cost as p:<id>:cost beside the flat one. The pull prices
// each p:<id> group with that same pricing's table, so a switch mid-drain never reads as drift.
// → the cost in nano-USD.
func PricedFields(fields map[string]int64, pricing *domain.Pricing) int64 {
	if pricing == nil || pricing.ID == "" {
		return 0
	}
	cost := domain.Cost(fields, pricing.Rates)
	tag := "p:" + pricing.ID + ":"
	for _, counter := range domain.PricedCounters {
		if n := fields[counter]; n > 0 {
			fields[tag+counter] = n
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
	case domain.IsHopFailure(rep.UpstreamStatus, rep.Reason):
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
