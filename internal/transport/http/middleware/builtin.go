package middleware

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
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
	access := deps.AccessLog()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Already inside an accesslog: a listener wraps its whole mux in one, and the data
			// chain carries its own. One line per request — the outermost wins.
			if !From(r).Started.IsZero() {
				next.ServeHTTP(w, r)
				return
			}
			r, state := newState(r)
			// Answered under both names — OpenAI SDKs read X-Request-Id, Anthropic SDKs Request-Id —
			// and set before any stage can refuse, so a denial carries it as well as a completion.
			w.Header().Set("X-Request-Id", state.RequestID)
			w.Header().Set("Request-Id", state.RequestID)
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
					slog.Int("attempts", state.Attempts),
					slog.String("key", or(state.Identity.Prefix(), "-")),
					slog.String("model", or(state.Model, "-")),
					slog.String("fallback", or(state.Fallback, "-")),
					slog.String("rid", state.RequestID),
					slog.String("upstream", or(state.Decision.EngineURL(), "-")),
					slog.String("upstream_rid", or(state.Outcome.UpstreamRID, "-")),
					slog.String("deployment", or(state.Decision.Route.Deployment, "-")),
					slog.String("engine", or(state.Outcome.Deployment, "-")),
					slog.Int("upstream_status", state.Outcome.Status),
					slog.String("reason", or(state.DeniedReason, state.Outcome.Reason)),
					slog.String("cut", or(state.Outcome.Cut, "-")),
				)
				access.LogAttrs(r.Context(), slog.LevelInfo, "access", attrs...)
			}()

			next.ServeHTTP(recorder, r)
		})
	}, nil
}

// drain answers while the process is shutting down or in maintenance. In-flight requests are past
// this stage and stay past it; a new one gets a real message with a retry hint rather than a reset.
// Everything it lets through is counted, so the control plane can wait for zero.
func newDrain(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if deps.Drain != nil && deps.Drain.Draining() {
				refuse(w, r, "draining", "5", "gateway is restarting, retry shortly")
				return
			}
			// Counted BEFORE the check: a request that saw maintenance off is already in the count
			// any reader who turned it on sees afterwards.
			if deps.InFlight != nil {
				deps.InFlight.Add(1)
				defer deps.InFlight.Add(-1)
			}
			if deps.Maintenance != nil && deps.Maintenance() {
				refuse(w, r, "maintenance", "30", "gateway is under maintenance, retry shortly")
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

func refuse(w http.ResponseWriter, r *http.Request, reason, retryAfter, message string) {
	state := From(r)
	state.Denied, state.DeniedReason = http.StatusServiceUnavailable, reason
	w.Header().Set("Retry-After", retryAfter)
	w.Header().Set("Connection", "close")
	respond.TypedErrorFor(w, r, http.StatusServiceUnavailable, reason, message)
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

// quota honours the geography pin, the credit flag the control plane pushed, and the prepaid
// balance this box keeps — all read off the user record, before the body is. The rate limits come
// last: a holder refused above must not use up a request.
func newQuota(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity := From(r).Identity
			if err := domain.GeographyDenial(identity.User, deps.Geography); err != nil {
				deny(w, r, err)
				return
			}
			if err := domain.ExhaustedDenial(identity.User); err != nil {
				deny(w, r, err)
				return
			}
			if err := deps.Admission.Admit(r.Context(), identity); err != nil {
				deny(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

// bodyReadTimeout bounds how long the gateway waits for the body it reads itself. ReadHeaderTimeout
// stops at the headers, so without it a client that drips its body holds a goroutine and up to
// max_body_bytes of buffer for as long as it likes. A var so a test can lower it.
var bodyReadTimeout = 60 * time.Second

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
				serveNamed(next, w, r, state)
				return
			}

			// Only around what this stage reads, cleared before the hop: the rest of a multipart
			// upload streams through the proxy, where a client-side timeout would read as the
			// upstream failing. Not supported on a recorder, which is fine — nothing drips there.
			// ponytail: an upload's tail past its model field is still unbounded in time.
			reading := http.NewResponseController(w)
			_ = reading.SetReadDeadline(time.Now().Add(bodyReadTimeout))
			// Cleared only on success: after a failed read the deadline must stay, or the server
			// waits out the rest of the stalled body before the refusal goes out.
			doneReading := func(err error) {
				if err == nil {
					_ = reading.SetReadDeadline(time.Time{})
				}
			}

			// A multipart body gives up its fields without being materialised and is forwarded as it
			// arrived. It never becomes a transform.Body, so the transform stage below skips it
			// rather than re-encoding a form as JSON.
			if boundary := multipartBoundary(r); boundary != "" {
				if r.ContentLength > maxBytes {
					deny(w, r, domain.Deny(http.StatusRequestEntityTooLarge, "request body too large"))
					return
				}
				model, session, form, err := readMultipart(w, r, boundary, maxBytes, deps.Log)
				doneReading(err)
				if err != nil {
					denyUnreadableBody(w, r, err)
					return
				}
				state.Model, state.Session, state.Form = model, session, form
				applySessionHeader(r, state)
				serveNamed(next, w, r, state)
				return
			}

			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
			doneReading(err)
			if err != nil {
				denyUnreadableBody(w, r, err)
				return
			}
			_ = r.Body.Close()

			// Only the model is this stage's to judge: a body it cannot be read out of has nowhere
			// to go. What else the body says is the engine's to reject.
			state.Raw = raw
			var decoded transform.Body
			if len(raw) > 0 && json.Unmarshal(raw, &decoded) != nil {
				deny(w, r, domain.Deny(http.StatusBadRequest, "request body is not a JSON object"))
				return
			}
			state.Body = decoded
			state.Model = stringField(decoded, "model")
			state.Session = stringField(decoded, "user")
			if state.Fallbacks, err = fallbackModels(decoded); err != nil {
				deny(w, r, err)
				return
			}
			applySessionHeader(r, state)
			restoreBody(r, raw)
			rewriteBody(r, state)
			serveNamed(next, w, r, state)
		})
	}, nil
}

// serveNamed hands on a request that names its model. One that names none cannot be routed, and
// the grant check's 403 for a model called "" would send the caller looking at their access.
func serveNamed(next http.Handler, w http.ResponseWriter, r *http.Request, state *State) {
	if state.Model == "" {
		deny(w, r, domain.Deny(http.StatusBadRequest, "request names no model"))
		return
	}
	next.ServeHTTP(w, r)
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
			state, started := From(r), time.Now()
			decision, err := deps.Routing.Pick(r.Context(), pickRequest(r, state, state.Model, state.Session))
			// A model with nowhere to go hands the request to the caller's fallbacks before any
			// dial; a pick refused for the request's own fault (the wrong surface) does not.
			var denial domain.Denial
			if errors.As(err, &denial) && domain.IsModelFailure(denial.Status) {
				if winner, model := nextFallback(deps, r, state); model != "" {
					logAttempt(deps, r, state, started, denial.Reason, "model", model)
					serveFallback(w, r, state, model, winner)
					decision, err = winner, nil
				}
			}
			if err != nil {
				deny(w, r, err)
				return
			}
			state.Decision = decision

			// Outbound: canonical over anything the client sent. vLLM adopts X-Request-Id as its own
			// request id; an ingress carries it on unchanged.
			r.Header.Set("X-Request-Id", decision.RequestID)
			next.ServeHTTP(w, r)
		})
	}, nil
}

// pickRequest asks for an engine of `model` on this request's surface.
func pickRequest(r *http.Request, state *State, model, session string) routing.Request {
	return routing.Request{
		Model:     model,
		Session:   session,
		MeterID:   state.Identity.MeterID,
		KeyPrefix: state.Identity.Prefix(),
		Path:      r.URL.Path,
		Dialect:   respond.Dialect(r.Context()),
		RequestID: state.RequestID,
	}
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
				deps.Routing.Release(ctx, state.Decision.Route, state.Decision.RequestID)
				deps.Metering.Record(ctx, metering.Report{
					RequestID:      state.RequestID,
					Prefix:         state.Identity.Prefix(),
					Model:          state.ServingModel(),
					Deployment:     or(state.Outcome.Deployment, state.Decision.Route.Deployment),
					Usage:          state.Outcome.Usage,
					UsageStart:     state.Outcome.UsageStart,
					Pricing:        state.Decision.Route.Pricing,
					User:           state.Identity.Key.User,
					Prepaid:        state.Identity.User.Prepaid,
					Budget:         state.Identity.User.Budget,
					Limits:         state.Identity.User.Limits,
					Target:         state.Decision.EngineURL(),
					UpstreamStatus: statusText(state.Outcome.Status),
					Reason:         state.Outcome.Reason,
					Cut:            state.Outcome.Cut,
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
			var changes []string
			changed, err := deps.Transform.Apply(transform.Context{
				Path:          r.URL.Path,
				UpstreamModel: state.Decision.Route.UpstreamModel,
				User:          state.Identity.Key.User,
				Provider:      state.Decision.Route.IsProvider(),
				Vendor:        vendor(state.Decision.Route),
				Changed:       &changes,
			}, state.Body)
			if err != nil {
				deps.Log.Error("request transform failed", "path", r.URL.Path, "err", err)
				deny(w, r, domain.Deny(http.StatusInternalServerError, "gateway error"))
				return
			}
			// Per attempt: a fallback to another vendor is not what the first one was sent.
			w.Header().Del("X-Grove-Changed")
			if len(changes) > 0 {
				w.Header().Set("X-Grove-Changed", strings.Join(changes, ", "))
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
			route, secret := state.Decision.Route, state.Decision.Key.Secret
			// The caller may have authenticated with either header; neither may travel onward.
			r.Header.Del("x-api-key")
			switch {
			case route.IsProvider():
				// A vendor authenticates its own way, and would read our Bearer as a caller's
				// credential leaking outward — so it is deleted, not overwritten. The scheme
				// follows the front's dialect: an Anthropic front takes x-api-key and its version
				// header, an OpenAI-compatible one takes the Bearer everyone else does — and so
				// does an Anthropic front the `vendors` table says reads one. Our request id stays
				// inside our network too: a vendor ignores it and mints its own. The secret is the
				// decision's, not the row's: a row carries several and the retry stage moves
				// between them.
				r.Header.Del("Authorization")
				r.Header.Del("X-Request-Id")
				anthropic := route.Dialect == domain.DialectAnthropic
				if anthropic && route.APIVersion != "" {
					r.Header.Set("anthropic-version", route.APIVersion)
				}
				switch {
				case secret == "":
				case anthropic && !transform.HasBearerAnthropicFront(vendor(route)):
					r.Header.Set("x-api-key", secret)
				default:
					r.Header.Set("Authorization", "Bearer "+secret)
				}
			case secret != "":
				r.Header.Set("Authorization", "Bearer "+secret)
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
				r.Header.Set("X-Grove-Model", state.ServingModel())
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
			// The gateway's id, adopted over the one this box minted: one grep crosses both logs, and
			// the caller is an authenticated gateway, not a client. Read first, so a refusal below is
			// attributable too.
			if rid := r.Header.Get("X-Request-Id"); rid != "" {
				state.RequestID = rid
				w.Header().Set("X-Request-Id", rid)
				w.Header().Set("Request-Id", rid)
			}
			state.Model = r.Header.Get("X-Grove-Model")
			if state.Model == "" {
				failIngress(w, r, http.StatusBadRequest, "no-model")
				return
			}
			sessionKey := r.Header.Get("X-Grove-Session-Key")
			requestID := state.RequestID

			route, err := deps.Routing.PickReplica(r.Context(), state.Model, sessionKey, requestID)
			if err != nil {
				var denial domain.Denial
				if !errors.As(err, &denial) {
					denial = domain.Deny(http.StatusServiceUnavailable, "no-replica")
				}
				failIngress(w, r, denial.Status, denial.Reason)
				return
			}
			state.Decision = routing.Decision{Route: route, RequestID: requestID, Key: deps.Routing.PickKey(route.Keyring())}

			// Stamped before the request leaves, so it is set whatever status the engine comes back
			// with. The gateway reads it off the response to attribute usage to a placement it
			// never chose.
			w.Header().Set("X-Grove-Engine", or(route.Deployment, "-"))
			defer func() {
				deps.Routing.Release(withoutCancel(r.Context()), route, requestID)
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
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		deny(w, r, domain.Deny(http.StatusRequestTimeout, "request body not received in time"))
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

// vendor names the third party a provider route dials — the control plane pushes the provider's
// name as the row's deployment. Blank on anything we run.
func vendor(route domain.Route) string {
	if !route.IsProvider() {
		return ""
	}
	return route.Deployment
}
