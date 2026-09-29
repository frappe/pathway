// Package http is the only place in the binary that imports net/http. Every decision it serves
// belongs to a service below it; this layer does routing, decoding, status codes and nothing else.
package http

import (
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/phot0n/pathway/internal/config"
	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/service/admission"
	"github.com/phot0n/pathway/internal/service/catalog"
	"github.com/phot0n/pathway/internal/service/metering"
	"github.com/phot0n/pathway/internal/service/provisioning"
	"github.com/phot0n/pathway/internal/service/routing"
	"github.com/phot0n/pathway/internal/service/transform"
	"github.com/phot0n/pathway/internal/transport/http/middleware"
	"github.com/phot0n/pathway/internal/transport/http/proxy"
	"github.com/phot0n/pathway/internal/transport/respond"
)

// Server holds the services the handlers reach and the two secrets the transport itself checks.
type Server struct {
	admission    *admission.Service
	routing      *routing.Service
	metering     *metering.Service
	catalog      *catalog.Service
	provisioning *provisioning.Service
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
	isIngress  bool
	retention  func() time.Duration
}

// usageRetention is the configured window, or a week on a server built without one (tests).
func (s *Server) usageRetention() time.Duration {
	if s.retention == nil {
		return 7 * 24 * time.Hour
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
		catalog: svc.Catalog, provisioning: svc.Provisioning, proxy: svc.Proxy,
		log: log, drain: svc.Drain, maintenance: svc.Maintenance, retention: svc.UsageRetention,
		deps: middleware.Deps{
			Admission: svc.Admission, Routing: svc.Routing, Metering: svc.Metering,
			Transform: svc.Transform, Drain: svc.Drain, Maintenance: svc.Maintenance,
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
	// Exact match, so it wins over the /v1/ proxy and is never forwarded to an engine — an engine
	// only knows its own model. Outside the data chain: it needs no route and claims no slot.
	mux.Handle("GET /v1/models", drain(http.HandlerFunc(s.handleModels)))
	mux.Handle("/v1/", openaiRoot(&s.chain))
	// Anthropic clients live under the provider convention they arrive with,
	// ANTHROPIC_BASE_URL=<base>/anthropic, their SDK appending /v1/*. Root is the OpenAI surface.
	mux.Handle("GET /anthropic/v1/models", drain(http.HandlerFunc(s.handleAnthropicModels)))
	mux.Handle("/anthropic/v1/", anthropicAlias(&s.chain))
	mux.Handle("GET /{$}", drain(http.HandlerFunc(root)))
	// The whole surface behind recover+accesslog, so the routes the mux answers itself — the model
	// lists, the root banner, alias refusals, plain 404s — leave a line like everything else.
	return s.logged(mux), nil
}

// openaiRoot refuses the Anthropic surfaces at root, before any stage runs: root is the OpenAI
// surface, and an Anthropic client belongs under /anthropic.
func openaiRoot(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if domain.ClientDialect(r.URL.Path) == domain.DialectAnthropic {
			respond.Error(w, http.StatusNotFound, "no such path at root; Anthropic clients use /anthropic"+r.URL.Path)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// anthropicAlias strips the prefix and refuses everything that is not an Anthropic surface —
// serving an OpenAI path under an Anthropic-declared base would answer in the wrong shape.
func anthropicAlias(next http.Handler) http.Handler {
	stripped := http.StripPrefix("/anthropic", next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if domain.ClientDialect(strings.TrimPrefix(r.URL.Path, "/anthropic")) != domain.DialectAnthropic {
			respond.ErrorFor(w, r, http.StatusNotFound, "no such path under /anthropic")
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
	mux.HandleFunc("/grove-admin/routes", adminAuth(s.adminToken, s.handleAdminRoutes))
	// The state push replaces the per-section endpoints above (plan_agent_state_sync.md); both
	// planes serve it — an ingress simply only ever receives the routes section.
	mux.HandleFunc("/grove-admin/state", adminAuth(s.adminToken, s.handleAdminState))
	mux.HandleFunc("/grove-admin/state-hash", adminAuth(s.adminToken, s.handleAdminStateHash))
	// Read-only: maintenance is only ever changed through the tunables file.
	mux.HandleFunc("/grove-admin/in-flight", adminAuth(s.adminToken, s.handleAdminInFlight))
	if s.isIngress {
		return mux
	}
	mux.HandleFunc("/grove-admin/keys", adminAuth(s.adminToken, s.handleAdminKeys))
	mux.HandleFunc("/grove-admin/users", adminAuth(s.adminToken, s.handleAdminUsers))
	mux.HandleFunc("/grove-admin/groups", adminAuth(s.adminToken, s.handleAdminGroups))
	mux.HandleFunc("/grove-admin/usage", adminAuth(s.adminToken, s.handleAdminUsage))
	mux.HandleFunc("POST /grove-admin/usage/ack", adminAuth(s.adminToken, s.handleAdminUsageAck))
	mux.HandleFunc("POST /grove-admin/spend-adjust", adminAuth(s.adminToken, s.handleAdminSpendAdjust))
	return mux
}

// Health answers the check every tier in front of this box asks, and answers it with "can I serve",
// not "is my socket open" — a DNS tier that drops this box from its answers has to be told about a
// gateway that is up and useless. 503 the moment a drain starts, which pulls the box out of rotation
// BEFORE its socket goes anywhere, and 503 when a pick would fail for every model it holds.
//
// The body is a fixed word either way: this is reachable from the internet unauthenticated, so it
// names nothing.
func (s *Server) Health(w http.ResponseWriter, r *http.Request) {
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
