// Package http is the only place in the binary that imports net/http. Every decision it serves
// belongs to a service below it; this layer does routing, decoding, status codes and nothing else.
package http

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/frappe/pathway/internal/config"
	"github.com/frappe/pathway/internal/domain"
	"github.com/frappe/pathway/internal/repository"
	"github.com/frappe/pathway/internal/service/admission"
	"github.com/frappe/pathway/internal/service/catalog"
	"github.com/frappe/pathway/internal/service/metering"
	"github.com/frappe/pathway/internal/service/provisioning"
	"github.com/frappe/pathway/internal/service/routing"
	"github.com/frappe/pathway/internal/service/transform"
	"github.com/frappe/pathway/internal/transport/http/middleware"
	"github.com/frappe/pathway/internal/transport/http/proxy"
	"github.com/frappe/pathway/internal/transport/respond"
)

// Server holds the services the handlers reach and the two secrets the transport itself checks.
type Server struct {
	admission    *admission.Service
	routing      *routing.Service
	metering     *metering.Service
	catalog      *catalog.Service
	provisioning *provisioning.Service
	providerKeys repository.ProviderKeys
	proxy        *proxy.Proxy
	log          *slog.Logger

	deps        middleware.Deps
	drain       middleware.DrainState
	maintenance func() bool
	// inFlight is every data request past the drain stage, for GET /grove-admin/in-flight.
	inFlight atomic.Int64
	// chain is swapped in place when the tunables file changes the middleware list, so the mux
	// built at startup keeps serving and only what it dispatches to moves.
	chain swappable
	// logged wraps a whole listener handler in recover+accesslog, so everything the server
	// answers — health checks, admin calls, 404s — leaves its one line, nginx-style. The data
	// chain's own accesslog stage sees the outer State and steps aside.
	logged middleware.Middleware

	adminToken string
	// contact is when the control plane last called an admin endpoint with the right token, unix
	// seconds; 0 since this process started. It polls every tier on a fixed tick, so a growing age
	// is a control plane that is gone, which no push-based signal says on a quiet fleet.
	contact   atomic.Int64
	isIngress bool
	retention func() time.Duration
}

// usageRetention is the configured window, or three days on a server built without one (tests).
func (s *Server) usageRetention() time.Duration {
	if s.retention == nil {
		return 3 * 24 * time.Hour
	}
	return s.retention()
}

// Services is what New needs. A struct rather than eight positional arguments, so adding one does
// not silently reorder the others.
type Services struct {
	Admission    *admission.Service
	Routing      *routing.Service
	Metering     *metering.Service
	Catalog      *catalog.Service
	Provisioning *provisioning.Service
	// ProviderKeys counts what each vendor credential answered; unused on an ingress.
	ProviderKeys repository.ProviderKeys
	Transform    *transform.Chain
	Proxy        *proxy.Proxy
	Drain        middleware.DrainState
	Access       *slog.Logger
	Payload      *slog.Logger
	// MaxBodyBytes and Maintenance are read per request, so a reload moves them.
	MaxBodyBytes func() int64
	Maintenance  func() bool
	// UsageRetention is how long an acknowledged drain is kept, read per ack.
	UsageRetention func() time.Duration
}

func New(cfg config.Config, svc Services, log *slog.Logger) *Server {
	server := &Server{
		admission: svc.Admission, routing: svc.Routing, metering: svc.Metering,
		catalog: svc.Catalog, provisioning: svc.Provisioning, providerKeys: svc.ProviderKeys, proxy: svc.Proxy,
		log: log, drain: svc.Drain, maintenance: svc.Maintenance, retention: svc.UsageRetention,
		deps: middleware.Deps{
			Admission: svc.Admission, Routing: svc.Routing, Metering: svc.Metering,
			Transform: svc.Transform, ProviderKeys: svc.ProviderKeys, Drain: svc.Drain, Maintenance: svc.Maintenance,
			Log: log, Access: svc.Access, Payload: svc.Payload,
			MaxBodyBytes: svc.MaxBodyBytes, IngressToken: cfg.IngressToken,
			Geography: cfg.Geography,
		},
		adminToken: cfg.AdminToken,
		isIngress:  cfg.IsIngress(),
	}
	server.deps.InFlight = &server.inFlight
	logged, err := middleware.Chain(server.deps, []string{"recover", "accesslog"})
	if err != nil {
		// Both names are literals registered in this module; failing here means the registry
		// itself is broken, and no request could be served accountably.
		panic("building the access-log wrapper: " + err.Error())
	}
	server.logged = logged
	return server
}

// DataHandler is the customer-facing surface: /v1/, plus the model list the gateway answers itself.
// The chain resolves here rather than per request, so an unknown middleware name fails at startup —
// a misspelt `quota` that merely warned would silently stop enforcing the budget.
func (s *Server) DataHandler(chain []string) (http.Handler, error) {
	if err := s.SetChain(chain); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	if s.isIngress {
		// An ingress forwards whatever the gateway sends and reads none of it.
		mux.Handle("/", &s.chain)
		return s.logged(mux), nil
	}

	// The routes the mux answers itself still pass drain, so a box in maintenance refuses every
	// customer path the way the chain does. /grove-admin, /healthz and the scrape are not on this mux.
	drain, err := middleware.Chain(s.deps, []string{"drain"})
	if err != nil {
		return nil, err
	}
	// Each dialect lives under the provider convention its clients arrive with:
	// OPENAI_BASE_URL=<base>/openai/v1 (that SDK's base carries the /v1) and
	// ANTHROPIC_BASE_URL=<base>/anthropic (that one appends /v1/messages itself).
	// The model list is an exact match, so it wins over the proxy and is never forwarded to
	// an engine, which only knows its own model. Outside the data chain: no route, no slot. Any
	// other method on it is refused by name: left to the proxy it would read a model out of the
	// body and answer that the MODEL does not serve the path.
	mux.Handle("GET /openai/v1/models", drain(http.HandlerFunc(s.handleModels)))
	mux.Handle("/openai/v1/models", drain(http.HandlerFunc(modelListIsGetOnly)))
	mux.Handle("/openai/", s.surface("/openai", domain.DialectOpenAI, servesOpenAI, &s.chain))
	mux.Handle("GET /anthropic/v1/models", drain(http.HandlerFunc(s.handleAnthropicModels)))
	mux.Handle("/anthropic/v1/models", drain(http.HandlerFunc(modelListIsGetOnly)))
	mux.Handle("/anthropic/", s.surface("/anthropic", domain.DialectAnthropic, servesAnthropic, &s.chain))
	// Root holds only the gateway's own answers: the holder's credit as this store counts it, and
	// the banner. A vendor path at root is sent to its base.
	mux.Handle("GET /v1/credits", drain(http.HandlerFunc(s.handleCredits)))
	mux.Handle("/v1/", http.HandlerFunc(rootRefusal))
	mux.Handle("GET /{$}", drain(http.HandlerFunc(root)))
	// The whole surface behind recover+accesslog, so the routes the mux answers itself — the model
	// lists, the root banner, alias refusals, plain 404s — leave a line like everything else.
	return s.logged(mux), nil
}

// rootRefusal answers a data path at root: nothing is served there, so the caller is sent to the
// base its path belongs under.
func rootRefusal(w http.ResponseWriter, r *http.Request) {
	base, client := "/openai", "OpenAI"
	if domain.PathDialect(r.URL.Path) == domain.DialectAnthropic {
		base, client = "/anthropic", "Anthropic"
	}
	respond.Error(w, http.StatusNotFound, "no such path at root; "+client+" clients use "+base+r.URL.Path)
}

// The OpenAI surface is open — engines of ours answer embeddings, audio and realtime on that
// shape — and refuses only the other dialect's own path. The Anthropic surface is closed to its
// own: serving an OpenAI path under an Anthropic-declared base would answer in the wrong shape.
func servesOpenAI(path string) bool    { return domain.PathDialect(path) != domain.DialectAnthropic }
func servesAnthropic(path string) bool { return domain.PathDialect(path) == domain.DialectAnthropic }

// surface strips the base a client declared, marks the dialect it speaks, and refuses what that
// surface does not serve. The caller is identified before the refusal, as the chain does: a
// keyless hit on the bare base is told its key is missing, not that the base does not exist.
func (s *Server) surface(prefix, dialect string, serves func(string) bool, next http.Handler) http.Handler {
	stripped := http.StripPrefix(prefix, next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(respond.WithDialect(r.Context(), dialect))
		if !serves(strings.TrimPrefix(r.URL.Path, prefix)) {
			if _, ok := s.identifyCaller(w, r); ok {
				respond.ErrorFor(w, r, http.StatusNotFound, "no such path under "+prefix+"; Anthropic clients use /anthropic/v1/messages")
			}
			return
		}
		stripped.ServeHTTP(w, r)
	})
}

// SetChain builds the data chain and swaps it in. Returning an error leaves the running chain
// exactly as it was — which is what makes a bad middleware name in the tunables file a log line
// rather than an outage.
func (s *Server) SetChain(chain []string) error {
	if len(chain) == 0 {
		chain = middleware.GatewayChain
		if s.isIngress {
			chain = middleware.IngressChain
		}
	}
	wrap, err := middleware.Chain(s.deps, chain)
	if err != nil {
		return err
	}
	s.chain.set(wrap(s.proxyHandler()))
	return nil
}

// swappable dispatches to whatever handler is current. One atomic load per request, which is the
// cost of not having to rebuild the mux — or drop a connection — when the chain changes.
type swappable struct {
	current atomic.Pointer[http.Handler]
}

func (s *swappable) set(h http.Handler) { s.current.Store(&h) }

func (s *swappable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	handler := s.current.Load()
	if handler == nil {
		respond.Error(w, http.StatusServiceUnavailable, "gateway not ready")
		return
	}
	(*handler).ServeHTTP(w, r)
}

// AdminHandler is the control plane's surface. Mounted on this box's OWN name, never on the shared
// one: a push has to reach one gateway, and Gateway Host names them all.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	// The replica table is the one thing both planes hold, so both take this push.
	mux.HandleFunc("/grove-admin/routes", s.admin(s.handleAdminRoutes))
	// The state push replaces the per-section endpoints above (plan_agent_state_sync.md); both
	// planes serve it — an ingress simply only ever receives the routes section.
	mux.HandleFunc("/grove-admin/state", s.admin(s.handleAdminState))
	mux.HandleFunc("/grove-admin/state-hash", s.admin(s.handleAdminStateHash))
	// Read-only: maintenance is only ever changed through the tunables file.
	mux.HandleFunc("/grove-admin/in-flight", s.admin(s.handleAdminInFlight))
	if s.isIngress {
		return mux
	}
	mux.HandleFunc("/grove-admin/keys", s.admin(s.handleAdminKeys))
	mux.HandleFunc("/grove-admin/groups", s.admin(s.handleAdminGroups))
	mux.HandleFunc("/grove-admin/usage", s.admin(s.handleAdminUsage))
	mux.HandleFunc("POST /grove-admin/usage/ack", s.admin(s.handleAdminUsageAck))
	mux.HandleFunc("POST /grove-admin/spend-adjust", s.admin(s.handleAdminSpendAdjust))
	mux.HandleFunc("/grove-admin/spent", s.admin(s.handleAdminSpent))
	mux.HandleFunc("/grove-admin/provider-keys", s.admin(s.handleAdminProviderKeys))
	return mux
}

// admin gates a control-plane endpoint on the token and, once it passes, stamps the contact: the
// call itself is the control plane saying it is alive, whatever it then asks.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return adminAuth(s.adminToken, func(w http.ResponseWriter, r *http.Request) {
		s.contact.Store(time.Now().Unix())
		next(w, r)
	})
}

// ContactAt is when the control plane last spoke to this box, zero when it has not since this
// process started.
func (s *Server) ContactAt() time.Time {
	if at := s.contact.Load(); at != 0 {
		return time.Unix(at, 0)
	}
	return time.Time{}
}

// Health answers the check every tier in front of this box asks, and answers it with "can I serve",
// not "is my socket open" — a DNS tier that drops this box from its answers has to be told about a
// gateway that is up and useless. 503 the moment a drain starts, which pulls the box out of rotation
// BEFORE its socket goes anywhere, and 503 when a pick would fail for every model it holds.
//
// The body is a fixed word either way: this is reachable from the internet unauthenticated, so it
// names nothing. The one header, X-Grove-Contact-Age, is seconds since the control plane last
// spoke to this box, absent until it has: a monitor's number, not a reason to leave DNS — a box
// whose control plane went quiet still serves what it holds.
func (s *Server) Health(w http.ResponseWriter, r *http.Request) {
	if at := s.ContactAt(); !at.IsZero() {
		w.Header().Set("X-Grove-Contact-Age", strconv.FormatInt(int64(time.Since(at).Seconds()), 10))
	}
	if s.drain != nil && s.drain.Draining() {
		respond.TypedError(w, http.StatusServiceUnavailable, "draining", "draining")
		return
	}
	if s.inMaintenance() {
		respond.TypedError(w, http.StatusServiceUnavailable, "maintenance", "maintenance")
		return
	}
	if s.routing != nil {
		if err := s.routing.CanServe(r.Context()); err != nil {
			respond.Error(w, http.StatusServiceUnavailable, err.Error())
			return
		}
	}
	_, _ = w.Write([]byte("ok\n"))
}
