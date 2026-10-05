// Package routing turns a model into an engine. Stickiness, region tiering, the capacity gate and
// least-in-flight all live here; the rule itself is domain.PickRoute and is shared with the ingress
// tier unchanged.
package routing

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
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

	var stickyURL string
	if session != "" {
		stickyURL, _ = s.sessions.Engine(ctx, session)
	}
	s.fillInFlight(ctx, table)
	s.markUnhealthy(ctx, table)

	route, status := domain.PickRoute(table, stickyURL, s.region)
	if status == 429 {
		route, status = s.waitForRoom(ctx, table, stickyURL)
	}
	if status == 429 {
		return Decision{}, domain.Deny(429, "every replica of "+req.Model+" is at capacity")
	}
	if status != 200 {
		return Decision{}, domain.Deny(503, "no healthy server for model")
	}

	if session != "" {
		if err := s.sessions.Pin(ctx, session, route.EngineURL, ttl); err != nil {
			// A lost pin costs one cold prefix cache, not a wrong answer.
			s.log.Warn("session pin failed", "session", session, "err", err)
		}
	}

	decision := Decision{
		Route:     route,
		RequestID: req.RequestID,
		Key:       s.PickKey(route.Keyring()),
		Session:   session,
	}
	if route.IsIngress() && session != "" {
		decision.SessionKey = domain.SHA256Hex(session)
	}
	// A vendor row holds no slot: it has no capacity of ours and no engine to balance against.
	if !route.IsProvider() {
		if err := s.inFlight.Claim(ctx, route.EngineURL, decision.RequestID); err != nil {
			// Undercounting an engine biases it toward more traffic; refusing the request over a
			// counter would be worse.
			s.log.Warn("in-flight claim failed", "engine", route.EngineURL, "err", err)
		}
	}
	s.log.Debug("route picked",
		"model", req.Model, "engine", route.EngineURL, "deployment", route.Deployment,
		"kind", route.Kind, "in_flight", route.InFlight, "sticky", stickyURL != "",
		"candidates", len(table), "rid", decision.RequestID)
	return decision, nil
}

// capacityPoll is how often a waiting pick re-reads the in-flight counts.
const capacityPoll = 50 * time.Millisecond

// waitForRoom re-picks until a replica has room, the wait runs out, or the client leaves. Nothing is
// claimed while waiting, and this sits above meter, so a request that gives up bills nothing.
// ponytail: polls the in-flight store every capacityPoll per waiting request; a release
// notification is the upgrade if many requests wait at once.
func (s *Service) waitForRoom(ctx context.Context, table []domain.Route, stickyURL string) (domain.Route, int) {
	var wait time.Duration
	if s.capacityWait != nil {
		wait = s.capacityWait()
	}
	deadline := time.Now().Add(wait)
	for time.Until(deadline) > 0 {
		timer := time.NewTimer(min(capacityPoll, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return domain.Route{}, 429
		case <-timer.C:
		}
		s.fillInFlight(ctx, table)
		if route, status := domain.PickRoute(table, stickyURL, s.region); status != 429 {
			return route, status
		}
	}
	return domain.Route{}, 429
}

// PickKey is the route's one selection strategy, round robin: the credential a request dials with,
// the next in the ring's order after the last request's. The cursor is per ring, and a vendor's ring
// is the same on every model it serves, so its keys take even turns across models — the limits they
// share are the vendor's. A session still changes key between requests, so a vendor's prompt cache
// stays per credential; that is the strategy's known cost. Blank on an empty ring.
func (s *Service) PickKey(keyring []domain.Credential) domain.Credential {
	if len(keyring) == 0 {
		return domain.Credential{}
	}
	cursor, _ := s.cursors.LoadOrStore(ringID(keyring), new(atomic.Uint64))
	turn := cursor.(*atomic.Uint64).Add(1) - 1
	return keyring[turn%uint64(len(keyring))]
}

// ringID names a ring by its members: the ids the control plane counts the keys under, in the
// order it pushed them. An engine's one blank-id credential names the one ring every engine shares,
// which has a single position anyway.
func ringID(keyring []domain.Credential) string {
	ids := make([]string, len(keyring))
	for i, key := range keyring {
		ids[i] = key.ID
	}
	return strings.Join(ids, ",")
}

// NextKey is the retry stage's walk: the first key after `loser` in ring order that is not in
// `spent`, round the ring once. Blank when every key is spent.
func NextKey(keyring []domain.Credential, loser domain.Credential, spent map[string]bool) domain.Credential {
	start := slices.IndexFunc(keyring, func(key domain.Credential) bool { return key.ID == loser.ID })
	for step := 1; step <= len(keyring); step++ {
		if key := keyring[(start+step)%len(keyring)]; !spent[key.ID] {
			return key
		}
	}
	return domain.Credential{}
}

// PickReplica is the ingress tier: the same rule with no session synthesis, no region (every
// replica is in this ingress's own VPC) and an already-opaque session key.
func (s *Service) PickReplica(ctx context.Context, model, sessionKey, requestID string) (domain.Route, error) {
	table, err := s.routes.Get(ctx, model)
	if err != nil || len(table) == 0 {
		// no-replica, not unhealthy. The gateway must not eject a whole network over one model
		// that has nowhere to go inside it.
		return domain.Route{}, domain.Deny(503, "no-replica")
	}
	s.fillInFlight(ctx, table)

	var stickyURL string
	if sessionKey != "" {
		stickyURL, _ = s.sessions.Engine(ctx, sessionKey)
	}
	route, status := domain.PickRoute(table, stickyURL, "")
	if status == 429 {
		return domain.Route{}, domain.Deny(429, "at-capacity")
	}
	if status != 200 {
		return domain.Route{}, domain.Deny(503, "no-replica")
	}
	if sessionKey != "" {
		if err := s.sessions.Pin(ctx, sessionKey, route.EngineURL, StickyTTL); err != nil {
			s.log.Warn("replica pin failed", "err", err)
		}
	}
	if err := s.inFlight.Claim(ctx, route.EngineURL, requestID); err != nil {
		s.log.Warn("in-flight claim failed", "engine", route.EngineURL, "err", err)
	}
	return route, nil
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
		s.markUnhealthy(ctx, table)
		if _, status := domain.PickRoute(table, "", s.region); status == 200 {
			return nil
		}
	}
	if len(models) == 0 {
		return nil
	}
	return ErrNoEngine
}

// Release crosses a finished request off its engine. A vendor row claimed nothing.
func (s *Service) Release(ctx context.Context, route domain.Route, requestID string) {
	if route.IsProvider() {
		return
	}
	if err := s.inFlight.Release(ctx, route.EngineURL, requestID); err != nil {
		s.log.Warn("in-flight release failed", "engine", route.EngineURL, "rid", requestID, "err", err)
	}
}

// fillInFlight sets each route's InFlight to what its engine is running now; a vendor row is never
// counted. A store failure leaves every count at zero, degrading to the first healthy route —
// refusing instead would 503 a working engine over an unreadable counter.
func (s *Service) fillInFlight(ctx context.Context, table []domain.Route) {
	rows := make([]int, 0, len(table))
	urls := make([]string, 0, len(table))
	for i, r := range table {
		if !r.IsProvider() {
			rows = append(rows, i)
			urls = append(urls, r.EngineURL)
		}
	}
	if len(urls) == 0 {
		return
	}
	counts, err := s.inFlight.Counts(ctx, urls)
	if err != nil {
		s.log.Warn("in-flight counts unavailable, falling back to the first healthy route", "err", err)
		return
	}
	for i, row := range rows {
		table[row].InFlight = counts[i]
	}
}

// markUnhealthy flips Healthy off where a target has failed too often in a row; PickRoute's own
// check does the rest. A store failure leaves the table as the control plane pushed it — ejection
// is an optimisation on something already correct, so an unreadable counter must not cause an outage.
func (s *Service) markUnhealthy(ctx context.Context, table []domain.Route) {
	targets := make([]string, len(table))
	for i, r := range table {
		targets[i] = r.EngineURL
	}
	failures, err := s.health.Failures(ctx, targets)
	if err != nil {
		return
	}
	for i := range table {
		if failures[i] >= domain.EjectAfter {
			table[i].Healthy = false
			s.log.Debug("target ejected", "engine", table[i].EngineURL, "failures", failures[i])
		}
	}
}
