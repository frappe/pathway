// Package repository declares what the services need out of storage, and nothing about how it is
// stored. Every method takes and returns domain types or plain Go values — no Redis vocabulary
// crosses this line, which is what lets the whole store be replaced by a new folder beside redis/.
package repository

import (
	"context"
	"time"

	"github.com/phot0n/pathway/internal/domain"
)

// Keys holds credentials. A key's only fact of its own is whether it has been revoked.
type Keys interface {
	Get(ctx context.Context, meterID string) (domain.KeyRecord, bool, error)
	Upsert(ctx context.Context, records []KeyUpsert) error
	Delete(ctx context.Context, ids []string) (int, error)
}

// Users holds the access and budget state behind a person — one record however many keys.
type Users interface {
	Get(ctx context.Context, name string) (domain.UserRecord, bool, error)
	Upsert(ctx context.Context, records []UserUpsert) error
	Delete(ctx context.Context, ids []string) (int, error)
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

// Usage accrues token counters per API key prefix. The field names are the service's business —
// this only adds numbers to them.
type Usage interface {
	// Add applies every field in one atomic step, so a drain never sees half a request.
	Add(ctx context.Context, prefix string, fields map[string]int64) error
	// Drain atomically reads and deletes every live counter, keyed by bare prefix. Read-and-delete
	// in one step: the snapshot is the only copy once it returns, which never double-counts.
	Drain(ctx context.Context) (map[string]map[string]string, error)
}

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
	Users    Users
	Groups   Groups
	Routes   Routes
	Sessions Sessions
	InFlight InFlight
	Health   Health
	Usage    Usage
	State    State
}

// The upsert shapes the control plane pushes. Deliberately flat strings, matching the wire: the
// comma lists are parsed on read, where a stale record from an older control plane is still
// readable.

type KeyUpsert struct {
	MeterID string // sha256(secret) hex — the record id
	Prefix  string
	User    string
	Status  string
}

type UserUpsert struct {
	Name        string
	Email       string
	Groups      string // comma list of Model Group names
	Allow       string // comma list
	Deny        string // comma list
	Limited     bool
	LogPayloads bool
	Geography   string
}

type GroupUpsert struct {
	Name   string
	Models string // comma list
}

// The state-push shapes (plan_agent_state_sync.md). A nil section is untouched; a present one is
// authoritative for its namespace, so anything it does not name is deleted. users and keys arrive
// in buckets (domain.BucketOf) and each bucket prunes only its own members.

type StatePush struct {
	Groups *GroupsPush
	Users  map[string]UserBucket // bucket label → contents; empty Records = prune the bucket
	Keys   map[string]KeyBucket
	Routes *RoutesPush
}

type GroupsPush struct {
	Hash    string
	Records []GroupUpsert
}

type UserBucket struct {
	Hash    string
	Records []UserUpsert
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
	Users  int `json:"users"`
	Keys   int `json:"keys"`
	Routes int `json:"routes"`
}
