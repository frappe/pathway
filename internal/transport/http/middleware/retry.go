package middleware

import (
	"log/slog"
	"maps"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/service/routing"
	"github.com/phot0n/pathway/internal/transport/http/proxy"
	"github.com/phot0n/pathway/internal/transport/respond"
)

func init() {
	Register("retry", newRetry)
}

// retry runs the stages below it again with another credential when a vendor refuses the one an
// attempt dialled with. Below meter, so the request is billed and released once whatever it
// took; above transform and upstreamauth, so each attempt rewrites the body and the auth header
// afresh. Only a key's own failures move it (domain.IsKeyFailure): a 429 spends the key until
// every other is spent too, then the whole ring is open once more — quota windows slide — while
// a 401/402/403 retires the key for the rest of the request. A provider row's 400 or 429 is read
// to its end before it is classed, since an exhausted account hides under both
// (domain.ClassifyUpstream). Nothing else is retried here: a 502 on one key is a 502 on the next.
// A 401/402/403 the ring cannot move off — or an engine's — never reaches the client as the
// upstream wrote it: the gateway answers 502 in its own words and logs what the upstream said.
// Every attempt, held or answered, is counted against its key, and a held one leaves its own
// line on the access log.
func newRetry(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)
			keyring := state.Decision.Route.Keyring()
			// An upgrade or a multipart body cannot be replayed; a single key has nowhere to go.
			replayable := !isUpgrade(r) && state.Raw != nil && len(keyring) > 1
			// Only a vendor hides a refusal under a 400 or 429; an engine's are its own.
			provider := state.Decision.Route.IsProvider()
			ring := newKeyRing(keyring)
			for {
				marks := markAttempt(w, r)
				var winner domain.Credential
				writer := &attemptWriter{ResponseWriter: w,
					peek:     func(status int) bool { return provider && (status == 400 || status == 429) },
					classify: domain.ClassifyUpstream,
					again: func(status int) bool {
						if refusal, ok := domain.RefusedCredential(status); ok {
							state.Outcome.Reason = "credential " + refusal.Class
						}
						if !replayable || r.Context().Err() != nil || !domain.IsKeyFailure(status) {
							return false
						}
						winner = ring.next(state.Decision.Key, status)
						return winner.ID != ""
					},
					replace: func(status int) bool {
						refusal, ok := domain.RefusedCredential(status)
						if !ok {
							return false
						}
						// The upstream's headers came with its answer; ours go with ours.
						clear(w.Header())
						maps.Copy(w.Header(), marks.response)
						respond.ErrorFor(w, r, http.StatusBadGateway, refusal.Message)
						return true
					},
				}
				serveAttempt(next, writer, r)
				writer.decide()
				countAttempt(deps, r, state.Decision.Key, writer.counted(state.Outcome.Status))
				if writer.replaced.Load() {
					logRefusal(deps, r, state, writer)
				}
				if !writer.held.Load() {
					return
				}

				logAttempt(deps, r, state, marks.started, or(state.DeniedReason, state.Outcome.Reason), "key", winner.ID)
				marks.restore(w, r, state)
				state.Decision.Key = winner
				replayBody(r, state)
			}
		})
	}, nil
}

// serveAttempt runs the stages below for one attempt. The proxy aborts a request whose upstream
// body broke off (http.ErrAbortHandler): right for a body the client was receiving, wrong for one
// the writer was keeping from them. That abort stops here, and the attempt is decided on what
// arrived.
func serveAttempt(next http.Handler, writer *attemptWriter, r *http.Request) {
	defer func() {
		if p := recover(); p != nil && !(p == http.ErrAbortHandler && writer.keeping()) {
			panic(p)
		}
	}()
	next.ServeHTTP(writer, r)
}

// logAttempt is the access log's record of an attempt the client never saw: where it went, how
// it ended, and what the request moved to — another `key` or another `model`. Joined to the
// request's own line by rid. Written before the attempt's marks come off.
func logAttempt(deps Deps, r *http.Request, state *State, started time.Time, reason, moved, to string) {
	deps.AccessLog().LogAttrs(r.Context(), slog.LevelInfo, "attempt",
		slog.String("rid", state.RequestID),
		slog.Int("attempt", state.Attempts),
		slog.String("model", or(state.ServingModel(), "-")),
		slog.String("upstream", or(state.Decision.EngineURL(), "-")),
		slog.String("upstream_key", or(state.Decision.Key.ID, "-")),
		slog.String("upstream_rid", or(state.Outcome.UpstreamRID, "-")),
		slog.String("deployment", or(state.Decision.Route.Deployment, "-")),
		slog.String("engine", or(state.Outcome.Deployment, "-")),
		slog.Int("upstream_status", state.Outcome.Status),
		slog.String("reason", reason),
		slog.String("cut", or(state.Outcome.Cut, "-")),
		slog.Float64("rt", time.Since(started).Seconds()),
		slog.String("moved", moved),
		slog.String("to", to),
	)
}

// logRefusal is the error log's record of a credential an upstream turned away, with what it
// said: the client was told nothing of it.
func logRefusal(deps Deps, r *http.Request, state *State, writer *attemptWriter) {
	deps.Log.Warn("upstream refused the credential",
		"rid", state.RequestID,
		"reason", state.Outcome.Reason,
		"model", or(state.ServingModel(), "-"),
		"upstream", or(state.Decision.EngineURL(), "-"),
		"upstream_key", or(state.Decision.Key.ID, "-"),
		"upstream_status", state.Outcome.Status,
		"upstream_rid", or(state.Outcome.UpstreamRID, "-"),
		"body", string(writer.excerpt),
	)
}

// attemptMarks is the response and the request as they stood before an attempt, and when it began.
type attemptMarks struct {
	response, request http.Header
	query             string
	started           time.Time
}

func markAttempt(w http.ResponseWriter, r *http.Request) attemptMarks {
	return attemptMarks{
		response: w.Header().Clone(), request: r.Header.Clone(), query: r.URL.RawQuery, started: time.Now(),
	}
}

// restore takes a held attempt's marks off before the next makes its own: the headers it set on
// the response, the auth and query the stages below rewrote, a refusal of the gateway's own, and
// what the proxy recorded of the hop. The body is the next attempt's to replay.
func (m attemptMarks) restore(w http.ResponseWriter, r *http.Request, state *State) {
	clear(w.Header())
	maps.Copy(w.Header(), m.response)
	r.Header, r.URL.RawQuery = m.request, m.query
	state.Outcome = proxy.Outcome{}
	state.Denied, state.DeniedReason = 0, ""
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

// next retires `loser` for `status` and picks the key the next attempt dials with — the one after
// it round the ring, blank when none is left. The rate-limited keys are opened once when nothing
// else remains: a vendor's quota window may have slid by then. A second exhaustion is the answer
// the client gets.
func (k *keyRing) next(loser domain.Credential, status int) domain.Credential {
	k.spent[loser.ID] = true
	if domain.IsKeyRejected(status) {
		k.dead[loser.ID] = true
	}
	if winner := routing.NextKey(k.keys, loser, k.spent); winner.ID != "" || k.reset {
		return winner
	}
	k.reset = true
	k.spent = maps.Clone(k.dead)
	return routing.NextKey(k.keys, loser, k.spent)
}

const (
	// peekLimit bounds a body read before it is classed: a vendor's error, however much of the
	// prompt it echoes. Past it the status is decided on what has arrived.
	peekLimit = 64 << 10
	// excerptLimit is how much of a swallowed answer the error log keeps.
	excerptLimit = 512
)

// attemptWriter holds an attempt's answer until `again` has said whether another key gets a go
// and `replace` whether the gateway answers in the upstream's place: the status is decided inside
// WriteHeader, and a held attempt's body goes nowhere. A status `peek` wants read first is kept
// whole — an error answer, capped — and decided on what `classify` makes of it once it ends.
// 1xx go straight through — the proxy clears the shared header map for them itself.
type attemptWriter struct {
	http.ResponseWriter
	peek     func(status int) bool
	classify func(status int, body []byte) int
	again    func(status int) bool
	replace  func(status int) bool
	// status is what the upstream sent; effective what it meant, which the attempt is moved and
	// counted on. peeked: the body was read before the two were told apart.
	status, effective int
	wrote, peeked     bool
	// pending: the body is being kept until it is decided; held: it was swallowed in favour of
	// another attempt; replaced: the gateway answered instead. Atomic because a flush can arrive
	// from the proxy's timer goroutine.
	pending, held, replaced atomic.Bool
	buf, excerpt            []byte
}

func (a *attemptWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		a.ResponseWriter.WriteHeader(status)
		return
	}
	if a.wrote {
		return
	}
	a.wrote, a.status = true, status
	if a.peek != nil && a.peek(status) {
		a.peeked = true
		a.pending.Store(true)
		return
	}
	a.settle(status)
}

// settle decides what a status gets: another key's go, the gateway's own answer, or the
// upstream's through.
func (a *attemptWriter) settle(effective int) {
	a.effective = effective
	if a.again(effective) {
		a.held.Store(true)
		return
	}
	if a.replace != nil && a.replace(effective) {
		a.replaced.Store(true)
		return
	}
	a.ResponseWriter.WriteHeader(a.status)
}

// decide settles a kept body on what it means, and lets it through when nothing else claimed
// it. Once: from Write when the body outgrows its cap, else after the attempt.
func (a *attemptWriter) decide() {
	if !a.pending.Load() {
		return
	}
	a.pending.Store(false)
	a.settle(a.classify(a.status, a.buf))
	if a.keeping() {
		a.keep(a.buf)
	} else {
		_, _ = a.ResponseWriter.Write(a.buf)
	}
	a.buf = nil
}

func (a *attemptWriter) Write(p []byte) (int, error) {
	if !a.wrote {
		a.WriteHeader(http.StatusOK)
	}
	if a.pending.Load() {
		a.buf = append(a.buf, p...)
		if len(a.buf) > peekLimit {
			a.decide()
		}
		return len(p), nil
	}
	if a.keeping() {
		a.keep(p)
		return len(p), nil
	}
	return a.ResponseWriter.Write(p)
}

// keeping reports an answer the client is not receiving: kept to be decided, or swallowed.
func (a *attemptWriter) keeping() bool {
	return a.pending.Load() || a.held.Load() || a.replaced.Load()
}

// keep retains the head of a swallowed answer for the log.
func (a *attemptWriter) keep(p []byte) {
	if room := excerptLimit - len(a.excerpt); room > 0 {
		a.excerpt = append(a.excerpt, p[:min(room, len(p))]...)
	}
}

// counted is the status the attempt lands on its key under: what the upstream meant when its
// body was read first, else `status` as the hop recorded it.
func (a *attemptWriter) counted(status int) int {
	if a.peeked {
		return a.effective
	}
	return status
}

// Flush is what the proxy's flush timer calls from its own goroutine; an answer the client is
// not receiving has nothing to push.
func (a *attemptWriter) Flush() {
	if a.keeping() {
		return
	}
	_ = http.NewResponseController(a.ResponseWriter).Flush()
}

func (a *attemptWriter) Unwrap() http.ResponseWriter { return a.ResponseWriter }
