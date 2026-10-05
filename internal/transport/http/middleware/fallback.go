package middleware

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/service/metering"
	"github.com/phot0n/pathway/internal/service/routing"
	"github.com/phot0n/pathway/internal/service/transform"
)

func init() {
	Register("fallback", newFallback)
}

// fallback runs the stages below it again on the next model the caller named when the one serving
// cannot (domain.IsModelFailure), or is itself a fallback and refused the request. Below meter, so
// the request is billed once, on the model that answered; above retry, so a model's whole key ring
// is walked before the next model is tried. A fallback is the caller's choice, never the
// gateway's: only the models in the body's `fallbacks`, each granted to them like the first.
// Decided on the status line, so a stream that has begun is never moved. With no model left, the
// last answer is the client's. Every answer that is not the first model's says so in
// X-Grove-Fallback, and every held attempt leaves its line on the access log.
func newFallback(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)
			// Only a JSON body names fallbacks, so anything that gets past here can be replayed.
			for len(state.Fallbacks) > 0 {
				marks := markAttempt(w, r)
				var winner routing.Decision
				var model string
				writer := &attemptWriter{ResponseWriter: w, again: func(status int) bool {
					// The model asked for is moved off only when it could not serve. A fallback is a
					// stand-in: whatever it refuses with, the next may take.
					refused := state.Fallback != "" && status >= http.StatusBadRequest
					if r.Context().Err() != nil || !(domain.IsModelFailure(status) || refused) {
						return false
					}
					winner, model = nextFallback(deps, r, state)
					return model != ""
				}}
				serveAttempt(next, writer, r)
				if !writer.held.Load() {
					return
				}

				loser := state.Decision
				logAttempt(deps, r, state, marks.started, or(state.DeniedReason, state.Outcome.Reason), "model", model)
				// The loser's slot and its failure are settled here: meter only ever sees the
				// winner. Two models behind one ingress share its URL, and so the slot.
				ctx := withoutCancel(r.Context())
				if loser.EngineURL() != winner.EngineURL() {
					deps.Routing.Release(ctx, loser.Route, loser.RequestID)
				}
				deps.Metering.Record(ctx, metering.Report{
					Target:         loser.EngineURL(),
					UpstreamStatus: statusText(state.Outcome.Status),
					Reason:         state.Outcome.Reason,
					Cut:            state.Outcome.Cut,
				})
				marks.restore(w, r, state)
				serveFallback(w, r, state, model, winner)
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// serveFallback hands the request to a fallback model: its engine, a body that names it, and the
// header that tells the client who answered.
func serveFallback(w http.ResponseWriter, r *http.Request, state *State, model string, decision routing.Decision) {
	state.Decision, state.Fallback = decision, model
	replayBody(r, state)
	w.Header().Set("X-Grove-Fallback", model)
}

// nextFallback takes the caller's fallbacks in order until one is theirs to use, takes what the
// request carries and has an engine to go to, and claims a slot on it. A blank model means the
// list is spent.
func nextFallback(deps Deps, r *http.Request, state *State) (routing.Decision, string) {
	// Read off the client's own bytes: the decoded body is the transforms' to change.
	sent := domain.SentInputs(state.Raw)
	for len(state.Fallbacks) > 0 {
		model := state.Fallbacks[0]
		state.Fallbacks = state.Fallbacks[1:]
		err := deps.Admission.Authorize(state.Identity, model)
		if err == nil {
			var decision routing.Decision
			// No session: the caller's pin is to an engine of the model they asked for.
			request := pickRequest(r, state, model, "")
			request.Inputs = sent
			if decision, err = deps.Routing.Pick(r.Context(), request); err == nil {
				return decision, model
			}
		}
		deps.Log.Info("fallback skipped", "rid", state.RequestID, "model", model, "err", err)
	}
	return routing.Decision{}, ""
}

// fallbackModels reads the body's `fallbacks`. Refused rather than trimmed when it is not a short
// list of names: a caller who named a fallback should learn it would never be tried.
func fallbackModels(body transform.Body) ([]string, error) {
	raw, named := body["fallbacks"]
	if !named {
		return nil, nil
	}
	var models []string
	if json.Unmarshal(raw, &models) != nil || len(models) > domain.MaxFallbacks || slices.Contains(models, "") {
		return nil, domain.Deny(http.StatusBadRequest,
			"fallbacks must be a list of at most "+itoa(domain.MaxFallbacks)+" model names")
	}
	return models, nil
}

// rewriteBody makes the decoded body the upstream's: without `fallbacks`, the gateway's own
// field, and naming the fallback when one is serving. The client's bytes go on untouched when
// neither applies.
func rewriteBody(r *http.Request, state *State) {
	_, named := state.Body["fallbacks"]
	if !named && state.Fallback == "" {
		return
	}
	delete(state.Body, "fallbacks")
	if state.Fallback != "" {
		state.Body["model"], _ = json.Marshal(state.Fallback)
	}
	if encoded, err := json.Marshal(state.Body); err == nil {
		restoreBody(r, encoded)
	}
}

// replayBody puts the client's body back for another attempt, decoded afresh: the transforms are
// not idempotent.
func replayBody(r *http.Request, state *State) {
	state.Body = nil
	_ = json.Unmarshal(state.Raw, &state.Body)
	restoreBody(r, state.Raw)
	rewriteBody(r, state)
}
