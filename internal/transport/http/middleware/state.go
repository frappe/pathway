package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/service/admission"
	"github.com/phot0n/pathway/internal/service/routing"
	"github.com/phot0n/pathway/internal/service/transform"
)

// State is what one request accumulates down the chain — one struct in the context rather than a
// key per field, since the stages are ordered and each reads what the ones above wrote. Scoped to
// the request: nothing outlives the handler and no two requests share one.
type State struct {
	Started time.Time
	// RequestID is minted at the edge, before anything can refuse: every line and header for this
	// request carries it, a health probe and a 401 included.
	RequestID string

	Identity admission.Identity
	Model    string
	Session  string
	Body     transform.Body
	// Raw is the request body exactly as the client sent it, kept for the payload log — Body above
	// is decoded and later mutated by transforms, so it cannot testify to what the customer wrote.
	// Nil on the bodyless paths (upgrade, multipart).
	Raw []byte

	Decision routing.Decision

	// Filled on the way back out, by the proxy.
	UpstreamStatus int
	// UpstreamRID is the upstream's own request id: a vendor's ticket key, blank on our engines.
	UpstreamRID string
	Usage       string
	Deployment  string
	Reason      string
	// Denied is the status a stage refused with, for the access log. 0 means the request reached
	// an upstream.
	Denied       int
	DeniedReason string
}

type stateKey struct{}

// newState attaches a fresh State. Called once, by accesslog, which is the outermost stage that
// needs one.
func newState(r *http.Request) (*http.Request, *State) {
	state := &State{Started: time.Now(), RequestID: domain.NewRequestID()}
	return r.WithContext(context.WithValue(r.Context(), stateKey{}, state)), state
}

// FromContext returns the request's State. Never nil for a request that went through the chain;
// a zero value for anything else, so a caller outside the chain does not have to nil-check.
func FromContext(ctx context.Context) *State {
	if state, ok := ctx.Value(stateKey{}).(*State); ok {
		return state
	}
	return &State{}
}

// From is the request-shaped form of FromContext.
func From(r *http.Request) *State { return FromContext(r.Context()) }
