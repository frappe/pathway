package middleware

import (
	"net/http"

	"github.com/frappe/pathway/internal/domain"
	"github.com/frappe/pathway/internal/service/vendor"
)

// vendorAuth is upstreamauth for a third party's front: the credential in the headers that vendor
// reads it from (vendor.Headers, by the front's dialect). Our request id stays inside our network:
// a vendor ignores it and mints its own. The secret is the decision's, not the row's: a row carries
// several and the retry stage moves between them.
func vendorAuth(r *http.Request, state *State) {
	route := state.Decision.Route
	r.Header.Del("X-Request-Id")
	for name, value := range vendor.For(route.Vendor).Headers(route.Dialect, state.Decision.Key.Secret, route.APIVersion) {
		r.Header.Set(name, value)
	}
}

// vendorPeeks says which of a vendor's statuses retry reads to the end before classing: an
// exhausted account hides under a 400 or a 429 (domain.ClassifyUpstream). An engine's are its own.
func vendorPeeks(status int) bool { return status == 400 || status == 429 }

// countAttempt lands one attempt on the key it dialled with. A blank id is an engine's one
// credential, counted nowhere.
func countAttempt(deps Deps, r *http.Request, key domain.Credential, status int) {
	if key.ID == "" || deps.ProviderKeys == nil {
		return
	}
	if err := deps.ProviderKeys.Count(withoutCancel(r.Context()), key.ID, status); err != nil {
		deps.Log.Warn("provider key count failed", "key", key.ID, "err", err)
	}
}
