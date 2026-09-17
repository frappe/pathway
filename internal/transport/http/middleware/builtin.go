package middleware

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/service/metering"
	"github.com/phot0n/pathway/internal/service/routing"
	"github.com/phot0n/pathway/internal/service/transform"
	"github.com/phot0n/pathway/internal/transport/respond"
)

func init() {
	Register("recover", newRecover)
	Register("accesslog", newAccessLog)
	Register("drain", newDrain)
	Register("auth", newAuth)
	Register("quota", newQuota)
	Register("body", newBody)
	Register("modelaccess", newModelAccess)
	Register("route", newRoute)
	Register("meter", newMeter)
	Register("transform", newTransform)
	Register("upstreamauth", newUpstreamAuth)
	Register("ingressauth", newIngressAuth)
	Register("pick", newPick)
}

// recover answers a panic instead of dropping the connection. Outermost, so it covers every stage
// below including the ones that write the response.
func newRecover(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if p := recover(); p != nil {
					// ErrAbortHandler is net/http's own way of saying "this connection is going
					// away"; logging it as a crash would fill the log with client disconnects.
					if errors.Is(p.(error), http.ErrAbortHandler) {
						panic(p)
					}
					deps.Log.Error("panic serving request", "path", r.URL.Path, "panic", p)
					respond.Error(w, http.StatusInternalServerError, "gateway error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}, nil
}

// accesslog times the request and writes the one durable line per request. It also creates the
// State, being the outermost stage that needs one.
func newAccessLog(deps Deps) (Middleware, error) {
	access := deps.Access
	if access == nil {
		access = deps.Log
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Already inside an accesslog: a listener wraps its whole mux in one, and the data
			// chain carries its own. One line per request — the outermost wins.
			if !From(r).Started.IsZero() {
				next.ServeHTTP(w, r)
				return
			}
			r, state := newState(r)
			recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			// Deferred: a client hanging up mid-stream unwinds this stage through
			// http.ErrAbortHandler, which recover deliberately re-panics. A request served
			// until the moment it was abandoned still owes its line — the upstream generated
			// and billed whatever it had, and a log that silently drops those requests is one
			// that cannot be reconciled against the bill.
			defer func() {
				attrs := []slog.Attr{
					slog.String("remote", clientIP(r)),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", recorder.status),
					slog.Int64("bytes", recorder.written),
					slog.Float64("rt", time.Since(state.Started).Seconds()),
				}
				// ttft is an upstream measurement — the wait for the first byte an engine or vendor
				// sent back. On a request the gateway answered itself (a model list, a health check,
				// a refusal) it would only repeat rt, so it is left off the line.
				if state.Decision.EngineURL() != "" {
					attrs = append(attrs, slog.Float64("ttft", recorder.ttft(state.Started)))
				}
				attrs = append(attrs,
					// Constant until §A retry lands; emitted now so the log schema never moves.
					slog.Int("attempts", 1),
					slog.String("key", or(state.Identity.Prefix(), "-")),
					slog.String("model", or(state.Model, "-")),
					slog.String("rid", or(state.Decision.RequestID, "-")),
					slog.String("upstream", or(state.Decision.EngineURL(), "-")),
					slog.String("deployment", or(state.Decision.Route.Deployment, "-")),
					slog.String("engine", or(state.Deployment, "-")),
					slog.Int("upstream_status", state.UpstreamStatus),
					slog.String("reason", or(state.DeniedReason, state.Reason)),
				)
				access.LogAttrs(r.Context(), slog.LevelInfo, "access", attrs...)
			}()

			next.ServeHTTP(recorder, r)
		})
	}, nil
}

// drain answers while the process is shutting down. In-flight requests are past this stage and stay
// past it; only a new one on an already-open connection lands here, and it gets a real message with
// a retry hint rather than a reset.
func newDrain(deps Deps) (Middleware, error) {
	if deps.Drain == nil {
		return passthrough, nil
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !deps.Drain.Draining() {
				next.ServeHTTP(w, r)
				return
			}
			state := From(r)
			state.Denied, state.DeniedReason = http.StatusServiceUnavailable, "draining"
			w.Header().Set("Retry-After", "5")
			w.Header().Set("Connection", "close")
			respond.Error(w, http.StatusServiceUnavailable, "gateway is restarting, retry shortly")
		})
	}, nil
}

// Credential is the caller's key however their SDK spells it: Authorization Bearer, or the
// x-api-key header every Anthropic SDK sends instead. Normalised to the Bearer form so one
// admission path serves both dialects' clients.
func Credential(r *http.Request) string {
	if authorization := r.Header.Get("Authorization"); authorization != "" {
		return authorization
	}
	if key := r.Header.Get("x-api-key"); key != "" {
		return "Bearer " + key
	}
	return ""
}

// auth resolves the caller: bearer → key → user → group, once, into the State.
func newAuth(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity, err := deps.Admission.Identify(r.Context(), Credential(r))
			if err != nil {
				deny(w, r, err)
				return
			}
			From(r).Identity = identity
			next.ServeHTTP(w, r)
		})
	}, nil
}

// quota honours the monthly token budget the control plane pushed. The gateway keeps no counters of
// its own — it reads a flag someone else computed.
func newQuota(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if From(r).Identity.User.Limited {
				deny(w, r, domain.Deny(http.StatusTooManyRequests, "monthly token quota exhausted"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// body reads and decodes the request body, bounded. The model and the session hint come out of it,
// and every stage below reads them from the State rather than parsing again.
func newBody(deps Deps) (Middleware, error) {
	limit := func() int64 {
		if deps.MaxBodyBytes == nil {
			return 32 << 20
		}
		if configured := deps.MaxBodyBytes(); configured > 0 {
			return configured
		}
		return 32 << 20
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)
			maxBytes := limit()

			// A WebSocket upgrade carries no body, so the model comes from the query string — where
			// the OpenAI realtime API puts it. Nothing reads or restores the body here: stamping
			// Content-Length on an upgrade breaks the handshake.
			if isUpgrade(r) {
				query := r.URL.Query()
				state.Model = strings.TrimSpace(query.Get("model"))
				state.Session = strings.TrimSpace(query.Get("user"))
				applySessionHeader(r, state)
				next.ServeHTTP(w, r)
				return
			}

			// A multipart body gives up its fields without being materialised and is forwarded as it
			// arrived. It never becomes a transform.Body, so the transform stage below skips it
			// rather than re-encoding a form as JSON.
			if boundary := multipartBoundary(r); boundary != "" {
				if r.ContentLength > maxBytes {
					deny(w, r, domain.Deny(http.StatusRequestEntityTooLarge, "request body too large"))
					return
				}
				model, session, err := readMultipart(w, r, boundary, maxBytes, deps.Log)
				if err != nil {
					denyUnreadableBody(w, r, err)
					return
				}
				state.Model, state.Session = model, session
				applySessionHeader(r, state)
				next.ServeHTTP(w, r)
				return
			}

			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
			if err != nil {
				denyUnreadableBody(w, r, err)
				return
			}
			_ = r.Body.Close()

			// A body that is not a JSON object is not an error here: some /v1 endpoints take none
			// at all, and the engine is the right place to reject a malformed one.
			state.Raw = raw
			var decoded transform.Body
			if len(raw) > 0 && json.Unmarshal(raw, &decoded) == nil {
				state.Body = decoded
				state.Model = stringField(decoded, "model")
				state.Session = stringField(decoded, "user")
			}
			applySessionHeader(r, state)
			restoreBody(r, raw)
			next.ServeHTTP(w, r)
		})
	}, nil
}

// modelaccess is the grant check: the same decision /v1/models filters its list with.
func newModelAccess(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)
			if err := deps.Admission.Authorize(state.Identity, state.Model); err != nil {
				deny(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// route picks an engine and claims an in-flight slot on it. Everything below must give that slot
// back, which is meter's job and why meter sits directly under this.
func newRoute(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)
			decision, err := deps.Routing.Pick(r.Context(), routing.Request{
				Model:     state.Model,
				Session:   state.Session,
				MeterID:   state.Identity.MeterID,
				KeyPrefix: state.Identity.Prefix(),
				Path:      r.URL.Path,
			})
			if err != nil {
				deny(w, r, err)
				return
			}
			state.Decision = decision

			// Canonical, overriding any client-supplied value: vLLM adopts X-Request-Id as its own
			// request id and OpenAI-aware tooling reads it back.
			r.Header.Set("X-Request-Id", decision.RequestID)
			w.Header().Set("X-Request-Id", decision.RequestID)
			next.ServeHTTP(w, r)
		})
	}, nil
}

// meter releases the slot and records what the request cost. Deferred, so it runs on a panic, a
// client disconnect and a dead upstream alike — the cases a metering call placed after the proxy
// would miss, and the ones that leave an engine counted as busy.
func newMeter(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)
			defer func() {
				// The request's own context is cancelled the moment the client hangs up, which is
				// exactly when this matters most. context.WithoutCancel keeps the store call alive.
				ctx := withoutCancel(r.Context())
				deps.Routing.Release(ctx, state.Decision.EngineURL(), state.Decision.RequestID)
				deps.Metering.Record(ctx, metering.Report{
					Prefix:         state.Identity.Prefix(),
					Model:          state.Model,
					Deployment:     or(state.Deployment, state.Decision.Route.Deployment),
					Usage:          state.Usage,
					Target:         state.Decision.EngineURL(),
					UpstreamStatus: statusText(state.UpstreamStatus),
					Reason:         state.Reason,
				})
			}()
			next.ServeHTTP(w, r)
		})
	}, nil
}

// transform runs the registered body rewrites and re-encodes only if one of them changed something.
func newTransform(deps Deps) (Middleware, error) {
	if deps.Transform == nil {
		return passthrough, nil
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)
			// An upgrade carries the model in the query and no body at all, so this is the only
			// rewrite it will get — the rule below never runs on it.
			rewriteQueryModel(r, state.Decision.Route.UpstreamModel)
			if state.Body == nil {
				next.ServeHTTP(w, r)
				return
			}
			changed, err := deps.Transform.Apply(transform.Context{
				Path:          r.URL.Path,
				UpstreamModel: state.Decision.Route.UpstreamModel,
				User:          state.Identity.Key.User,
				Provider:      state.Decision.Route.IsProvider(),
			}, state.Body)
			if err != nil {
				deps.Log.Error("request transform failed", "path", r.URL.Path, "err", err)
				deny(w, r, domain.Deny(http.StatusInternalServerError, "gateway error"))
				return
			}
			if changed {
				encoded, err := json.Marshal(state.Body)
				if err != nil {
					deny(w, r, domain.Deny(http.StatusInternalServerError, "gateway error"))
					return
				}
				restoreBody(r, encoded)
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// rewriteQueryModel puts the upstream's own id in the query, where the realtime API carries it.
// The body form of this is the modelmap transform; both are blank-safe, and blank is every route
// we run ourselves. Untouched when the caller named no model — inventing one would send a request
// they did not make, and an upgrade with no model has already been refused above.
func rewriteQueryModel(r *http.Request, upstreamModel string) {
	if upstreamModel == "" {
		return
	}
	query := r.URL.Query()
	if query.Get("model") == "" {
		return
	}
	query.Set("model", upstreamModel)
	r.URL.RawQuery = query.Encode()
}

// upstreamauth swaps the client's key for the target's own and sets the headers the hop needs. The
// client's credential never reaches an engine.
func newUpstreamAuth(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)
			route := state.Decision.Route
			// The caller may have authenticated with either header; neither may travel onward.
			r.Header.Del("x-api-key")
			switch {
			case route.IsProvider():
				// A vendor authenticates its own way, and would read our Bearer as a caller's
				// credential leaking outward — so it is deleted, not overwritten. The scheme
				// follows the front's dialect: an Anthropic front takes x-api-key and its version
				// header, an OpenAI-compatible one takes the Bearer everyone else does.
				r.Header.Del("Authorization")
				if route.Dialect != domain.DialectAnthropic {
					if route.InternalKey != "" {
						r.Header.Set("Authorization", "Bearer "+route.InternalKey)
					}
					break
				}
				if route.InternalKey != "" {
					r.Header.Set("x-api-key", route.InternalKey)
				}
				if route.APIVersion != "" {
					r.Header.Set("anthropic-version", route.APIVersion)
				}
			case route.InternalKey != "":
				r.Header.Set("Authorization", "Bearer "+route.InternalKey)
			}
			// This is the edge, so a client-sent forwarding header is a claim, not a fact.
			// Overwritten rather than appended: the appending form leaves the caller's entries in
			// front of ours.
			r.Header.Set("X-Forwarded-For", clientIP(r))
			r.Header.Set("X-Real-IP", clientIP(r))

			if route.IsIngress() {
				// An ingress reads no body, so the model it would otherwise have parsed goes as a
				// header — and the session key with it, hashed, because it is the one tier that
				// must not learn whose request this is.
				r.Header.Set("X-Grove-Model", state.Model)
				r.Header.Set("X-Grove-Session-Key", state.Decision.SessionKey)
			} else {
				// A direct route reaches vLLM, which adopts X-Request-Id and nothing else.
				// Clearing these keeps a client-supplied value from reaching an engine that would
				// only ignore it.
				r.Header.Del("X-Grove-Model")
				r.Header.Del("X-Grove-Session-Key")
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// ingressauth proves the caller is a gateway. Third layer on a hop that already carries verified
// server TLS and a firewall — and the only one this process can check itself.
func newIngressAuth(deps Deps) (Middleware, error) {
	want := []byte(deps.IngressToken)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := []byte(domain.Bearer(r.Header.Get("Authorization")))
			// A blank configured token refuses everything rather than waving callers through: that
			// is what a misrendered env file looks like, and it would open every engine in the VPC.
			if len(want) == 0 || subtle.ConstantTimeCompare(got, want) != 1 {
				failIngress(w, r, http.StatusUnauthorized, "not-a-gateway")
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// pick is the ingress's routing stage: model and session key off headers the gateway set, no body
// read at all. It also releases, because an ingress has no metering to hang that on.
func newPick(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)
			state.Model = r.Header.Get("X-Grove-Model")
			if state.Model == "" {
				failIngress(w, r, http.StatusBadRequest, "no-model")
				return
			}
			sessionKey := r.Header.Get("X-Grove-Session-Key")
			requestID := r.Header.Get("X-Request-Id")

			route, err := deps.Routing.PickReplica(r.Context(), state.Model, sessionKey, requestID)
			if err != nil {
				var denial domain.Denial
				if !errors.As(err, &denial) {
					denial = domain.Deny(http.StatusServiceUnavailable, "no-replica")
				}
				failIngress(w, r, denial.Status, denial.Reason)
				return
			}
			state.Decision = routing.Decision{Route: route, RequestID: requestID}

			// Stamped before the request leaves, so it is set whatever status the engine comes back
			// with. The gateway reads it off the response to attribute usage to a placement it
			// never chose.
			w.Header().Set("X-Grove-Engine", or(route.Deployment, "-"))
			defer func() {
				deps.Routing.Release(withoutCancel(r.Context()), route.EngineURL, requestID)
			}()
			next.ServeHTTP(w, r)
		})
	}, nil
}

// failIngress answers with a NAMED reason. The gateway ejects a whole network on a connection
// failure or a 502/504 and must NOT eject one on a no-replica 503, or a single unplaced model takes
// the network out of rotation for every other model on it.
func failIngress(w http.ResponseWriter, r *http.Request, status int, reason string) {
	state := From(r)
	state.Denied, state.DeniedReason = status, reason
	w.Header().Set("X-Grove-Reason", reason)
	respond.Status(w, status, map[string]any{
		"error": map[string]any{"message": reason, "type": "grove_ingress"},
	})
}

func deny(w http.ResponseWriter, r *http.Request, err error) {
	var denial domain.Denial
	if errors.As(err, &denial) {
		state := From(r)
		state.Denied, state.DeniedReason = denial.Status, denial.Reason
	}
	respond.DenialFor(w, r, err)
}

func passthrough(next http.Handler) http.Handler { return next }

// restoreBody puts a fully-read body back on the request so the proxy can forward it.
// isUpgrade reports whether the client asked to switch protocols. Connection is a comma list and
// both header values are case-insensitive, so neither can be compared directly.
func isUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, value := range r.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

func denyUnreadableBody(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		deny(w, r, domain.Deny(http.StatusRequestEntityTooLarge, "request body too large"))
		return
	}
	deny(w, r, domain.Deny(http.StatusBadRequest, "could not read request body"))
}

// applySessionHeader lets an explicit header beat the body's `user`, which is a best-effort hint.
func applySessionHeader(r *http.Request, state *State) {
	if header := strings.TrimSpace(r.Header.Get("X-Grove-Session")); header != "" {
		state.Session = header
	}
}

func restoreBody(r *http.Request, raw []byte) {
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	// A stale Content-Length header would contradict the body a transform just resized.
	r.Header.Set("Content-Length", itoa(len(raw)))
}

func stringField(body transform.Body, name string) string {
	raw, ok := body[name]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

// statusRecorder remembers what was answered, for the access line.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
	// firstByte is when the first body byte left — what TTFT means on a streaming response,
	// and the number a log store can aggregate that total time hides.
	firstByte time.Time
}

func (s *statusRecorder) WriteHeader(status int) {
	if !s.wrote {
		s.status, s.wrote = status, true
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	s.wrote = true
	if s.firstByte.IsZero() {
		s.firstByte = time.Now()
	}
	n, err := s.ResponseWriter.Write(p)
	s.written += int64(n)
	return n, err
}

// ttft is seconds to the first body byte, 0 when nothing was ever written.
func (s *statusRecorder) ttft(started time.Time) float64 {
	if s.firstByte.IsZero() {
		return 0
	}
	return s.firstByte.Sub(started).Seconds()
}

// Unwrap lets net/http find the underlying writer for Flush and Hijack. Without it, wrapping the
// writer would turn every streaming response into a buffered one.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
