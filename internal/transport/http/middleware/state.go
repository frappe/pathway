package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/service/admission"
	"github.com/phot0n/pathway/internal/service/routing"
	"github.com/phot0n/pathway/internal/service/transform"
	"github.com/phot0n/pathway/internal/transport/http/proxy"
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
	// Form is what a multipart body said before its model field — the text fields, and a stand-in
	// naming each file — for the payload log. Never the file itself. Nil on every other path.
	Form map[string]string

	// Fallbacks is the other models the caller named, in their order, still untried. Fallback is
	// the one of them serving this request; blank while the model asked for is.
	Fallbacks []string
	Fallback  string

	Decision routing.Decision
	// Attempts is how many times an upstream was dialled for this request; 0 when none was.
	Attempts int

	// Outcome is what the proxy learned of the attempt that is serving this request, filled on the
	// way back out. Blank again before another attempt.
	Outcome proxy.Outcome
	// Denied is the status a stage refused with, for the access log. 0 means the request reached
	// an upstream.
	Denied       int
	DeniedReason string
}

// ServingModel is the model this request is served by: a fallback once one took over.
func (s *State) ServingModel() string { return or(s.Fallback, s.Model) }

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
