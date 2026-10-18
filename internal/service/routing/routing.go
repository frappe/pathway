// Package routing turns a model into an upstream. Two routers sit behind one Pick: engine.go for
// what we run (stickiness, the capacity gate, ejection, in-flight claims, shared with the ingress
// tier) and vendor.go for a third party's fronts (the key ring). Both narrow by region and health
// with domain.PickRoute.
package routing

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/frappe/pathway/internal/domain"
	"github.com/frappe/pathway/internal/repository"
)

// StickyTTL is how long a caller that names its own session keeps its engine. Client-declared
// affinity, not a fallback, so it is a constant rather than the synthetic knob below.
const StickyTTL = 30 * time.Minute

// Request is what a pick needs. Session is the caller's own hint (header or the body's `user`
// field) and may be blank.
type Request struct {
	Model     string
	Session   string
	MeterID   string
	KeyPrefix string
	// Path is the surface being asked for, checked against what the model gives. An ASR model and
	// a chat model are indistinguishable by name alone.
	Path string
	// Dialect is the surface the request arrived on; a vendor serves only its own.
	Dialect string
	// Inputs is what the request carries besides text (domain.SentInputs), checked against what
	// the model takes. Set only for a fallback: the model asked for is the caller's to get wrong.
	Inputs []string
	// Tools is every name the request asks a tool by (domain.SentTools), checked against what a
	// vendor row denies. Set for the model asked for too: a vendor-run tool is the vendor's bill,
	// not the caller's mistake to pay for.
	Tools []string
	// RequestID was minted at the edge; the claim and the decision carry it. Blank is minted here
	// rather than claimed as "" — a blank member would silently undercount the engine.
	RequestID string
}

// Decision is the pick and everything downstream needs to act on it.
type Decision struct {
	Route     domain.Route
	RequestID string
	// Key is the credential this attempt dials with — one of the route's keyring, taken in turn;
	// the retry stage moves it when a vendor refuses the key.
	Key     domain.Credential
	Session string // the session actually pinned; "" when none was used
	// SessionKey is sha256(Session), set only for an ingress route. The gateway's session may be a
	// caller-chosen string that names the tenant; the infra plane keys on the hash instead.
	SessionKey string
}

// EngineURL and RequestID together name the in-flight slot this decision claimed.
func (d Decision) EngineURL() string { return d.Route.EngineURL }

// HealthTarget is what this hop's outcome is counted against, blank for a vendor row: with no
// sibling to steer to, ejecting it would only turn the vendor's errors into our 503.
func (d Decision) HealthTarget() string {
	if d.Route.IsProvider() {
		return ""
	}
	return d.Route.EngineURL
}

type Service struct {
	routes   repository.Routes
	sessions repository.Sessions
	inFlight repository.InFlight
	health   repository.Health
	log      *slog.Logger

	region string
	// syntheticTTL is how long a caller naming no session is pinned to one engine; 0 balances every
	// such request. A function, not a value, so turning it is an edit to the tunables file rather
	// than a deploy.
	syntheticTTL func() time.Duration
	capacityWait func() time.Duration
	// cursors is the round-robin position per vendor ring (ringID → *atomic.Uint64), this process's
	// own: two gateways each take even turns without agreeing on whose turn it is.
	cursors sync.Map
	// warned is each model already logged for a mixed table: a stale table is a line, not a flood.
	warned sync.Map
}

func serving(table []domain.Route, dialect, path string) []domain.Route {
	out := make([]domain.Route, 0, len(table))
	for _, candidate := range table {
		if domain.ServesRoute(candidate, dialect, path) {
			out = append(out, candidate)
		}
	}
	return out
}

type Options struct {
	Region string
	// SyntheticTTL is read on every pick. Nil means no synthetic session at all.
	SyntheticTTL func() time.Duration
	// CapacityWait is how long a pick waits for a slot when every replica is full. Nil or 0 is a
	// 429 at once.
	CapacityWait func() time.Duration
}

func New(store repository.Store, log *slog.Logger, opts Options) *Service {
	return &Service{
		routes: store.Routes, sessions: store.Sessions,
		inFlight: store.InFlight, health: store.Health,
		log:          log,
		region:       opts.Region,
		syntheticTTL: opts.SyntheticTTL,
		capacityWait: opts.CapacityWait,
	}
}

// Pick chooses an engine and claims a slot on it. The claim is the caller's obligation to release:
// every path that gets a Decision back must Release it, including on client disconnect.
func (s *Service) Pick(ctx context.Context, req Request) (Decision, error) {
	// A caller that names a session keeps its engine, so its prefix cache stays warm. One that
	// names none is balanced — unless the operator asked for the synthetic session back, which
	// pins a whole key to one engine and is what a single-placement fleet always did.
	session, ttl := req.Session, StickyTTL
	synthetic := time.Duration(0)
	if s.syntheticTTL != nil {
		synthetic = s.syntheticTTL()
	}
	if session == "" && synthetic > 0 {
		session = domain.SHA256Hex(req.MeterID + "|" + req.Model)[:24]
		ttl = synthetic
	}

	if req.RequestID == "" {
		req.RequestID = domain.NewRequestID()
	}

	table, err := s.routes.Get(ctx, req.Model)
	if err != nil || len(table) == 0 {
		if err != nil {
			s.log.Error("route store unreadable", "model", req.Model, "err", err)
		}
		return Decision{}, domain.Deny(503, "model unavailable")
	}

	// Surface admission is per ROW: a dual-front vendor's rows differ in dialect, so the path
	// filters the table and the pick runs on what survives. Refused only when nothing does,
	// rather than forwarded — the upstream would 404 it, and this sits above meter, so a
	// wrong-surface call bills nothing.
	table = serving(table, req.Dialect, req.Path)
	if len(table) == 0 {
		return Decision{}, domain.Deny(404, req.Model+" does not serve "+req.Path)
	}
	// A row that declares its inputs and lacks one the request carries would refuse it after a
	// dial. Rows of one model are stamped alike, so this empties the table or leaves it whole.
	table = slices.DeleteFunc(table, func(candidate domain.Route) bool {
		return !domain.Takes(candidate.InputModalities, req.Inputs)
	})
	if len(table) == 0 {
		return Decision{}, domain.Deny(404, req.Model+" does not take "+strings.Join(req.Inputs, ", ")+" input")
	}
	// A vendor row that denies a tool the request names would run it at our cost. 400 and not the
	// 404 above: the model serves this surface, the body asks for what is not offered.
	var refused string
	table = slices.DeleteFunc(table, func(candidate domain.Route) bool {
		if name := domain.Refuses(candidate.DeniedTools, req.Tools); name != "" {
			refused = name
			return true
		}
		return false
	})
	if len(table) == 0 {
		return Decision{}, domain.Deny(400, req.Model+" does not run "+refused)
	}

	// A model is one kind, so its first row names its router. A push that mixes them is refused,
	// but a table an older binary stored is not re-read until it changes: ours win there — the old
	// pick could land on its vendor row, this one never does — until the control plane's next push.
	table = s.oneKind(req.Model, table)
	if table[0].IsProvider() {
		return s.pickVendor(req, table)
	}
	return s.pickEngine(ctx, req, table, session, ttl)
}

// What CanServe reports instead of a bare open socket. Fixed strings: they reach a public health
// check, so they name no model, engine or count.
var (
	ErrStoreUnreachable = errors.New("store unreachable")
	ErrNoEngine         = errors.New("no reachable engine")
)

// CanServe reports whether a pick would succeed for anything at all — what a health check in front
// of this box actually needs to know. An unreadable store authenticates nobody and routes nothing;
// a table whose every target has been ejected serves nobody.
//
// A fleet with no models pushed is nil, not ErrNoEngine: nothing is deployed, which is not a broken
// box, and taking every gateway out of DNS for it would be worse than the 503 a caller gets anyway.
// In-flight counts are deliberately left at zero — a gateway turning traffic away at capacity is
// working, so capacity must not read as unhealthy.
func (s *Service) CanServe(ctx context.Context) error {
	models, err := s.routes.Models(ctx)
	if err != nil {
		return ErrStoreUnreachable
	}
	for _, model := range models {
		table, err := s.routes.Get(ctx, model)
		if err != nil || len(table) == 0 {
			continue
		}
		table = s.oneKind(model, table)
		if !table[0].IsProvider() {
			s.markUnhealthy(ctx, table)
		}
		if _, status := domain.PickRoute(table, "", s.region); status == 200 {
			return nil
		}
	}
	if len(models) == 0 {
		return nil
	}
	return ErrNoEngine
}

// oneKind drops a mixed table's vendor rows (domain.MixedKinds). Loud once per model, since the
// table is wrong and the engine router would otherwise count, eject and pin against a vendor.
func (s *Service) oneKind(model string, table []domain.Route) []domain.Route {
	if !domain.MixedKinds(table) {
		return table
	}
	if _, seen := s.warned.LoadOrStore(model, true); !seen {
		s.log.Warn("mixed route table, vendor rows ignored until the next push", "model", model)
	}
	return slices.DeleteFunc(table, domain.Route.IsProvider)
}
