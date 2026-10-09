// Package repository declares what the services need out of storage, and nothing about how it is
// stored. Every method takes and returns domain types or plain Go values — no Redis vocabulary
// crosses this line, which is what lets the whole store be replaced by a new folder beside redis/.
package repository

import (
	"context"
	"time"

	"github.com/phot0n/pathway/internal/domain"
)

// Keys holds credentials and the policy behind each: what it may call, where, how fast, and the
// cap it spends against.
type Keys interface {
	// Resolve reads a credential and what stands behind it in one step: the key and each group it
	// is in. found=false for a key that was never pushed.
	Resolve(ctx context.Context, meterID string) (holder Holder, found bool, err error)
	Upsert(ctx context.Context, records []KeyUpsert) error
	Delete(ctx context.Context, ids []string) (int, error)
	// AdjustSpent moves a key's lifetime spend by delta nano-USD, once per id: a repeated id
	// answers the current spend with applied=false. found=false when the key is not here.
	AdjustSpent(ctx context.Context, meterID, id string, delta int64) (spent int64, applied, found bool, err error)
}

// Holder is a credential and what stands behind it, as the store read them.
type Holder struct {
	Key domain.KeyRecord
	// Groups is each group the key lists, by name. One never pushed is here as the zero value.
	Groups map[string]domain.GroupRecord
}

// Groups holds what a Model Group grants everyone in it.
type Groups interface {
	// Get answers the zero value for a group that was never pushed, so a key pointing at one falls
	// back to its own Allow list instead of erroring.
	Get(ctx context.Context, name string) (domain.GroupRecord, error)
	Upsert(ctx context.Context, records []GroupUpsert) error
}

// Routes is the model → placements table the control plane pushes.
type Routes interface {
	Get(ctx context.Context, model string) ([]domain.Route, error)
	// Models lists every model with at least one placement, deduped and sorted.
	Models(ctx context.Context) ([]string, error)
	Replace(ctx context.Context, model string, routes []domain.Route) error
	DeleteModels(ctx context.Context, models []string) error
}

// Sessions is the affinity pin: a caller keeps its engine so its prefix cache stays warm.
type Sessions interface {
	// Engine answers "" when the session has no pin, which is not an error.
	Engine(ctx context.Context, session string) (string, error)
	Pin(ctx context.Context, session, engineURL string, ttl time.Duration) error
}

// InFlight counts what is running on each engine right now. Gateway-local: the control plane has
// no view of it.
type InFlight interface {
	// Counts answers one number per engine, in the order given. A store failure returns an error
	// and the caller degrades to taking the first healthy route rather than refusing the request.
	Counts(ctx context.Context, engineURLs []string) ([]int, error)
	Claim(ctx context.Context, engineURL, requestID string) error
	Release(ctx context.Context, engineURL, requestID string) error
}

// Health is the consecutive-failure count behind passive ejection.
type Health interface {
	// Failures answers one count per target, in the order given.
	Failures(ctx context.Context, targets []string) ([]int, error)
	RecordFailure(ctx context.Context, target string) error
	RecordSuccess(ctx context.Context, target string) error
}

// ProviderKeys counts what each vendor credential answered — lifetime, by the id the control
// plane pushed it under. Read live by the control plane; never drained.
type ProviderKeys interface {
	// Count lands one attempt on one key: the upstream's status, or 0 when the hop produced none.
	Count(ctx context.Context, id string, status int) error
	// Stats answers one record per id, zero for a key that never answered.
	Stats(ctx context.Context, ids []string) (map[string]domain.KeyStats, error)
}

// Limits counts what a key has used inside each limit's current window. now picks the window:
// the caller's clock decides it, so a test can move it.
type Limits interface {
	// Admit checks every limit and, only when all have room, counts the request on the request
	// limits — one atomic step, so two requests cannot both take the last slot. → the limits
	// already spent; none when admitted.
	Admit(ctx context.Context, key string, limits []domain.Limit, now time.Time) ([]domain.Limit, error)
	// Debit adds tokens to the current window of each of these limits.
	Debit(ctx context.Context, key string, limits []domain.Limit, tokens int64, now time.Time) error
}

// Usage accrues token counters per API key prefix. The field names are the service's business —
// this only adds numbers to them.
type Usage interface {
	// Accrue lands one request in one atomic step — its counters, its cost, and the key's
	// spend — so a drain never sees half a request, and never a counter without its cost.
	Accrue(ctx context.Context, accrual Accrual) error
	// Drain sets live counters aside under newID — every one, or only these prefixes when keys is
	// non-empty — and answers every counter not yet acknowledged, old drains included, so one the
	// control plane failed to record comes back under its own id. Nothing is deleted here.
	Drain(ctx context.Context, newID string, keys []string) (Drains, error)
	// Ack marks (drain id → prefixes) recorded: each is kept for retention, then expires. A pair
	// that is not unacknowledged is a no-op. → how many pairs moved.
	Ack(ctx context.Context, acks map[string][]string, retention time.Duration) (int, error)
	// Replay lands a spooled accrual exactly as Accrue would, once per ID: a second replay of the
	// same ID (a crash mid-pass) moves nothing. → whether it landed now.
	Replay(ctx context.Context, accrual Accrual) (bool, error)
	// Ping reports whether the store answers — the spool replays only while it does.
	Ping(ctx context.Context) error
}

// Drains is drain id → bare prefix → counters, as the control plane receives them.
type Drains map[string]map[string]map[string]string

// State is the desired-state push: apply what the payload names, delete what it does not, and
// store the hashes it carried — all in one transaction, so the hashes never claim state that
// did not land. Hashes is what the control plane diffs against before deciding to push at all.
type State interface {
	Hashes(ctx context.Context) (map[string]string, error)
	Apply(ctx context.Context, push StatePush) (StateCounts, error)
}

// Store is every repository at once, for wiring. Services take only the interfaces they use.
type Store struct {
	Keys     Keys
	Groups   Groups
	Routes   Routes
	Sessions Sessions
	InFlight InFlight
	Health   Health
	Usage    Usage
	Limits   Limits
	State    State
	// ProviderKeys is unused on an ingress, which dials no vendor.
	ProviderKeys ProviderKeys
}

// The upsert shapes the control plane pushes. Deliberately flat strings, matching the wire: the
// comma lists are parsed on read, where a stale record from an older control plane is still
// readable.

type KeyUpsert struct {
	MeterID     string // sha256(secret) hex — the record id
	Prefix      string
	Team        string
	Status      string
	Groups      string // comma list of Model Group names
	Allow       string // comma list
	Deny        string // comma list
	Limited     bool
	LogPayloads bool
	Geography   string
	Prepaid     bool
	Budget      int64  // the key's cap, nano-USD
	Limits      string // comma list of metric:window:value
}

type GroupUpsert struct {
	Name   string
	Models string // comma list
}

// Accrual is one metered request. Fields already carry the cost beside the counters; Cost is
// repeated so the store can move the key's lifetime spend without reading the map back.
type Accrual struct {
	// ID is the request id: what makes a spooled accrual's replay land once.
	ID     string           `json:"id"`
	Prefix string           `json:"prefix"`
	Fields map[string]int64 `json:"fields"`
	Cost   int64            `json:"cost"`
	// Key is the record (sha256 hex) whose spend moves; blank moves nobody's. Budget is the cap
	// the key's balance is reported against, so the drain carries this store's own view of it.
	Key    string `json:"key"`
	Budget int64  `json:"budget"`
}

// The state-push shapes (plan_agent_state_sync.md). A nil section is untouched; a present one is
// authoritative for its namespace, so anything it does not name is deleted. keys arrive in
// buckets (domain.BucketOf) and each bucket prunes only its own members.

type StatePush struct {
	Groups *GroupsPush
	Keys   map[string]KeyBucket // bucket label → contents; empty Records = prune the bucket
	Routes *RoutesPush
}

type GroupsPush struct {
	Hash    string
	Records []GroupUpsert
}

type KeyBucket struct {
	Hash    string
	Records []KeyUpsert
}

type RoutesPush struct {
	Hash  string
	Table map[string][]domain.Route
}

// StateCounts is records written per section, informational for the control plane's run log.
type StateCounts struct {
	Groups int `json:"groups"`
	Keys   int `json:"keys"`
	Routes int `json:"routes"`
}
