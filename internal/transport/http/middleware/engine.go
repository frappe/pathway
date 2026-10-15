package middleware

import "net/http"

// engineAuth is upstreamauth for an engine or ingress of ours: its one internal key as a Bearer,
// and what an ingress needs to pick a replica.
func engineAuth(r *http.Request, state *State) {
	if secret := state.Decision.Key.Secret; secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	if state.Decision.Route.IsIngress() {
		// An ingress reads no body, so the model it would otherwise have parsed goes as a
		// header — and the session key with it, hashed, because it is the one tier that
		// must not learn whose request this is.
		r.Header.Set("X-Grove-Model", state.ServingModel())
		r.Header.Set("X-Grove-Session-Key", state.Decision.SessionKey)
	}
}
