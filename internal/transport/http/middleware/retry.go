package middleware

import (
	"log/slog"
	"maps"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/frappe/pathway/internal/domain"
	"github.com/frappe/pathway/internal/service/routing"
	"github.com/frappe/pathway/internal/service/vendor"
	"github.com/frappe/pathway/internal/transport/http/proxy"
	"github.com/frappe/pathway/internal/transport/respond"
)

func init() {
	Register("retry", newRetry)
}

// retry runs the stages below it again with another credential when a vendor refuses the one an
// attempt dialled with. Below meter, so the request is billed and released once whatever it
// took; above transform and upstreamauth, so each attempt rewrites the body and the auth header
// afresh. Only a key's own failures move it (domain.Rotates): a rate limit spends the key until
// every other is spent too, then the whole ring is open once more — quota windows slide — while a
// rejected key, an exhausted account, a model the key cannot reach or a region it is refused from
// retires the key for the rest of the request (domain.Retires). A refusal the upstream's type
// says to (vendor.Peeks: a provider's 4xx, none of an engine's) is read to its end before it is
// classed, since each of those hides under one status or another, and it is classed as that type
// reads it (vendor.Classify). Nothing else is retried here: a 502 on one key is a 502 on the next.
// A rejected key or an exhausted account the ring cannot move off — or an engine's — never
// reaches the client as the upstream wrote it, nor which refusal it was: the gateway answers 502
// `upstream unavailable` (domain.Exhausts). A model or a region refusal every key got is the
// request's to see, as sent. Every attempt, held or answered, is counted against its key, a held
// one leaves its own line on the access log, and every failure of the upstream's — not the
// caller's — leaves what it said on the error log, with the key (domain.UpstreamFault).
func newRetry(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)
			keyring := state.Decision.Route.Keyring()
			// An upgrade or a multipart body cannot be replayed; a single key has nowhere to go.
			replayable := !isUpgrade(r) && state.Raw != nil && len(keyring) > 1
			upstream := vendor.Of(state.Decision.Route)
			ring := routing.NewKeyRing(keyring)
			for {
				marks := markAttempt(w, r)
				var winner domain.Credential
				writer := &attemptWriter{ResponseWriter: w,
					peek:     upstream.Peeks,
					classify: upstream.Classify,
					again: func(status int, class string) bool {
						// The proxy classed the status alone; the body is better. A status the
						// proxy did not record is the gateway's own answer to a dial that
						// failed, classed there.
						if state.Outcome.Status == status {
							state.Outcome.Class = class
						}
						if !replayable || r.Context().Err() != nil || !domain.Rotates(class) {
							return false
						}
						winner = ring.Next(state.Decision.Key, class)
						return winner.ID != ""
					},
					replace: func(_ int, class string) bool {
						if !domain.Exhausts(class) {
							return false
						}
						// The upstream's headers came with its answer; ours go with ours.
						clear(w.Header())
						maps.Copy(w.Header(), marks.response)
						respond.ErrorFor(w, r, http.StatusBadGateway, "upstream unavailable")
						return true
					},
				}
				serveAttempt(next, writer, r)
				writer.decide()
				countAttempt(deps, r, state.Decision.Key, domain.CountedStatus(writer.class, state.Outcome.Status))
				if state.Outcome.Status != 0 && domain.UpstreamFault(writer.class) {
					logFailure(deps, r, state, writer)
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
	attrs := []slog.Attr{
		slog.String("rid", state.RequestID),
		slog.Int("attempt", state.Attempts),
		slog.String("model", or(state.ServingModel(), "-")),
		slog.String("upstream", or(state.Decision.EngineURL(), "-")),
		slog.String("upstream_key", or(state.Decision.Key.ID, "-")),
		slog.String("upstream_rid", or(state.Outcome.UpstreamRID, "-")),
		slog.String("deployment", or(state.Decision.Route.Deployment, "-")),
		slog.String("engine", or(state.Outcome.Deployment, "-")),
		slog.String("served_model", servedModel(state)),
		slog.Int("upstream_status", state.Outcome.Status),
		slog.Int("upstream_attempts", state.Outcome.UpstreamAttempts),
		slog.String("retry_after", or(state.Outcome.RetryAfter, "-")),
		slog.String("remaining_requests", or(state.Outcome.RemainingRequests, "-")),
		slog.String("remaining_tokens", or(state.Outcome.RemainingTokens, "-")),
		slog.String("reason", reason),
		slog.String("cut", or(state.Outcome.Cut, "-")),
		slog.String("failure_class", or(state.Outcome.Class, "-")),
		slog.Float64("rt", time.Since(started).Seconds()),
		slog.String("moved", moved),
		slog.String("to", to),
	}
	attrs = append(attrs, timing(state, started, time.Time{})...)
	deps.AccessLog().LogAttrs(r.Context(), slog.LevelInfo, "attempt", attrs...)
}

// logFailure is the error log's record of an answer that was the upstream's failure, with the
// head of what it said — whether the client received it, another key got a go, or the gateway
// answered instead. A hop with no status has the proxy's own line.
func logFailure(deps Deps, r *http.Request, state *State, writer *attemptWriter) {
	deps.Log.Warn("upstream failed",
		"rid", state.RequestID,
		"failure_class", state.Outcome.Class,
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

const (
	// peekLimit bounds a body read before it is classed: a vendor's error, however much of the
	// prompt it echoes. Past it the status is decided on what has arrived.
	peekLimit = 64 << 10
	// excerptLimit is how much of a failed answer the error log keeps.
	excerptLimit = 512
)

// attemptWriter holds an attempt's answer until `again` has said whether another key gets a go
// and `replace` whether the gateway answers in the upstream's place: the status is decided inside
// WriteHeader, and a held attempt's body goes nowhere. A status `peek` wants read first is kept
// whole — an error answer, capped — and decided on what `classify` makes of its body once it
// ends; domain.FailureClass when nobody set one. 1xx go straight through — the proxy clears the
// shared header map for them itself.
type attemptWriter struct {
	http.ResponseWriter
	peek     func(status int) bool
	classify func(status int, body []byte) string
	again    func(status int, class string) bool
	replace  func(status int, class string) bool
	// status is what the upstream sent; class what it meant (classify), which the attempt is
	// moved and counted on. peeked: the body was read before the class was decided.
	status        int
	class         string
	wrote, peeked bool
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
	a.settle(a.classed(status, nil))
}

// classed is what an answer means, as the upstream's type reads it, or as every upstream's is
// read when the writer was given no type.
func (a *attemptWriter) classed(status int, body []byte) string {
	if a.classify == nil {
		return domain.FailureClass(status, body)
	}
	return a.classify(status, body)
}

// settle decides what an answer of this class gets: another key's go, the gateway's own answer,
// or the upstream's through.
func (a *attemptWriter) settle(class string) {
	a.class = class
	if a.again(a.status, class) {
		a.held.Store(true)
		return
	}
	if a.replace != nil && a.replace(a.status, class) {
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
	a.settle(a.classed(a.status, a.buf))
	a.keep(a.buf)
	if !a.keeping() {
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
	// The head of a failed answer is kept whoever gets the body.
	if a.status >= 400 || a.keeping() {
		a.keep(p)
	}
	if a.keeping() {
		return len(p), nil
	}
	return a.ResponseWriter.Write(p)
}

// keeping reports an answer the client is not receiving: kept to be decided, or swallowed.
func (a *attemptWriter) keeping() bool {
	return a.pending.Load() || a.held.Load() || a.replaced.Load()
}

// keep retains the head of a failed answer for the log.
func (a *attemptWriter) keep(p []byte) {
	if room := excerptLimit - len(a.excerpt); room > 0 {
		a.excerpt = append(a.excerpt, p[:min(room, len(p))]...)
	}
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
