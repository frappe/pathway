package middleware

import (
	"net/http"

	"github.com/frappe/pathway/internal/domain"
	"github.com/frappe/pathway/internal/service/vendor"
)

// vendorAuth is upstreamauth for a third party's front: the row's own headers, then the credential
// in the headers that vendor reads it from (vendor.Headers, by the front's dialect), so the
// credential wins. Our request id stays inside our network: a vendor ignores it and mints its own.
// The secret is the decision's, not the row's: a row carries several and the retry stage moves
// between them.
func vendorAuth(r *http.Request, state *State) {
	route := state.Decision.Route
	r.Header.Del("X-Request-Id")
	for name, value := range route.Headers {
		r.Header.Set(name, value)
	}
	for name, value := range vendor.Of(route).Headers(route.Dialect, state.Decision.Key.Secret) {
		r.Header.Set(name, value)
	}
}

// countAttempt lands one attempt on the key it dialled with. A blank id is an engine's one
// credential, counted nowhere.
func countAttempt(deps Deps, r *http.Request, key domain.Credential, status int) {
	if key.ID == "" || deps.ProviderKeys == nil {
		return
	}
	if err := deps.ProviderKeys.Count(withoutCancel(r.Context()), key.ID, status); err != nil {
		deps.Log.Warn("provider key count failed", "rid", From(r).RequestID, "key", key.ID, "err", err)
	}
}
