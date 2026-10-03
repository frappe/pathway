package middleware

import (
	"encoding/json"
	"maps"
	"net/http"
	"sync/atomic"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/service/routing"
)

func init() {
	Register("retry", newRetry)
}

// retry runs the stages below it again with another credential when a vendor refuses the one an
// attempt dialled with. Below meter, so the request is billed and released once whatever it
// took; above transform and upstreamauth, so each attempt rewrites the body and the auth header
// afresh. Only a key's own failures move it (domain.IsKeyFailure): a 429 spends the key until
// every other is spent too, then the whole ring is open once more — quota windows slide — while
// a 401/402/403 retires the key for the rest of the request. Nothing else is retried here: a 502
// on one key is a 502 on the next. Every attempt, held or answered, is counted against its key.
func newRetry(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)
			keyring := state.Decision.Route.Keyring()
			// An upgrade or a multipart body cannot be replayed; a single key has nowhere to go.
			replayable := !isUpgrade(r) && state.Raw != nil && len(keyring) > 1
			ring := newKeyRing(keyring)
			for {
				headers, requestHeaders, query := w.Header().Clone(), r.Header.Clone(), r.URL.RawQuery
				var winner domain.Credential
				writer := &attemptWriter{ResponseWriter: w, again: func(status int) bool {
					if !replayable || r.Context().Err() != nil || !domain.IsKeyFailure(status) {
						return false
					}
					winner = ring.next(state.Decision.Key, status)
					return winner.ID != ""
				}}
				next.ServeHTTP(writer, r)
				countAttempt(deps, r, state.Decision.Key, state.UpstreamStatus)
				if !writer.held.Load() {
					return
				}

				deps.Log.Info("key rotated", "rid", state.RequestID, "model", state.Model,
					"status", state.UpstreamStatus, "from", state.Decision.Key.ID, "to", winner.ID,
					"attempt", state.Attempts+1)
				// The loser's marks come off before the winner makes its own: the headers it set
				// on the response, the auth and query the stages below rewrote, the body the
				// transforms edited (not idempotent — re-decoded from what the client sent), and
				// what the proxy recorded of the hop.
				clear(w.Header())
				maps.Copy(w.Header(), headers)
				r.Header, r.URL.RawQuery = requestHeaders, query
				state.Body = nil
				_ = json.Unmarshal(state.Raw, &state.Body)
				restoreBody(r, state.Raw)
				state.UpstreamStatus, state.UpstreamRID, state.Usage, state.UsageStart = 0, "", "", ""
				state.Deployment, state.Reason, state.Cut = "", "", ""
				state.Decision.Key = winner
			}
		})
	}, nil
}

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

// keyRing is one request's walk over a route's credentials: which are spent, which are dead, and
// whether the rate-limited ones have been opened again.
type keyRing struct {
	keys  []domain.Credential
	spent map[string]bool // rate-limited or dead this request
	dead  map[string]bool // refused outright: never opened again
	reset bool
}

func newKeyRing(keys []domain.Credential) *keyRing {
	return &keyRing{keys: keys, spent: map[string]bool{}, dead: map[string]bool{}}
}

// next retires `loser` for `status` and picks the key the next attempt dials with — blank when
// none is left. The rate-limited keys are opened once when nothing else remains: a vendor's quota
// window may have slid by then. A second exhaustion is the answer the client gets.
func (k *keyRing) next(loser domain.Credential, status int) domain.Credential {
	k.spent[loser.ID] = true
	if domain.IsKeyRejected(status) {
		k.dead[loser.ID] = true
	}
	if winner := routing.PickKey(k.keys, k.spent); winner.ID != "" || k.reset {
		return winner
	}
	k.reset = true
	k.spent = maps.Clone(k.dead)
	return routing.PickKey(k.keys, k.spent)
}

// attemptWriter holds an attempt's answer until `again` has said whether another key gets a go:
// the status is decided inside WriteHeader, and a held attempt's body goes nowhere. 1xx go
// straight through — the proxy clears the shared header map for them itself.
type attemptWriter struct {
	http.ResponseWriter
	again func(status int) bool
	// wrote: a final status has been seen; held: it was swallowed in favour of another attempt.
	// Atomic because a flush can arrive from the proxy's timer goroutine.
	wrote bool
	held  atomic.Bool
}

func (a *attemptWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		a.ResponseWriter.WriteHeader(status)
		return
	}
	if a.wrote {
		return
	}
	a.wrote = true
	if a.again(status) {
		a.held.Store(true)
		return
	}
	a.ResponseWriter.WriteHeader(status)
}

func (a *attemptWriter) Write(p []byte) (int, error) {
	if !a.wrote {
		a.WriteHeader(http.StatusOK)
	}
	if a.held.Load() {
		return len(p), nil
	}
	return a.ResponseWriter.Write(p)
}

// Flush is what the proxy's flush timer calls from its own goroutine; held, there is nothing to
// push and nothing may reach the client.
func (a *attemptWriter) Flush() {
	if a.held.Load() {
		return
	}
	_ = http.NewResponseController(a.ResponseWriter).Flush()
}

func (a *attemptWriter) Unwrap() http.ResponseWriter { return a.ResponseWriter }
