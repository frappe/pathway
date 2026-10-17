package routing

import (
	"context"
	"time"

	"github.com/frappe/pathway/internal/domain"
)

// pickEngine is the self-hosted router: a table of engines and ingresses of ours, each with
// capacity, an in-flight count and a failure count. A session is pinned to the row it lands on so
// its prefix cache stays warm, and the row's slot is claimed until Release.
func (s *Service) pickEngine(ctx context.Context, req Request, table []domain.Route, session string, ttl time.Duration) (Decision, error) {
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
		// An engine has the one credential, its internal key.
		Key:     s.PickKey(route.Keyring()),
		Session: session,
	}
	if route.IsIngress() && session != "" {
		decision.SessionKey = domain.SHA256Hex(session)
	}
	if err := s.inFlight.Claim(ctx, route.EngineURL, decision.RequestID); err != nil {
		// Undercounting an engine biases it toward more traffic; refusing the request over a
		// counter would be worse.
		s.log.Warn("in-flight claim failed", "engine", route.EngineURL, "err", err)
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

// PickReplica is the ingress tier: the same rule with no session synthesis, no region (every
// replica is in this ingress's own VPC) and an already-opaque session key.
func (s *Service) PickReplica(ctx context.Context, model, sessionKey, requestID string) (domain.Route, error) {
	table, err := s.routes.Get(ctx, model)
	if err != nil || len(table) == 0 {
		// no-replica, not unhealthy. The gateway must not eject a whole network over one model
		// that has nowhere to go inside it.
		return domain.Route{}, domain.Deny(503, "no-replica")
	}
	table = s.oneKind(model, table)
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

// Release crosses a finished request off its engine. A vendor row claimed nothing.
func (s *Service) Release(ctx context.Context, route domain.Route, requestID string) {
	if route.IsProvider() {
		return
	}
	if err := s.inFlight.Release(ctx, route.EngineURL, requestID); err != nil {
		s.log.Warn("in-flight release failed", "engine", route.EngineURL, "rid", requestID, "err", err)
	}
}

// fillInFlight sets each engine's InFlight to what it is running now. A store failure leaves every
// count at zero, degrading to the first healthy route — refusing instead would 503 a working engine
// over an unreadable counter.
func (s *Service) fillInFlight(ctx context.Context, table []domain.Route) {
	urls := make([]string, len(table))
	for i, r := range table {
		urls[i] = r.EngineURL
	}
	counts, err := s.inFlight.Counts(ctx, urls)
	if err != nil {
		s.log.Warn("in-flight counts unavailable, falling back to the first healthy route", "err", err)
		return
	}
	for i := range table {
		table[i].InFlight = counts[i]
	}
}

// markUnhealthy flips Healthy off where an engine has failed too often in a row; PickRoute's own
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
