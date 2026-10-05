package http

import (
	"net/http"

	"github.com/phot0n/pathway/internal/transport/http/middleware"
	"github.com/phot0n/pathway/internal/transport/http/proxy"
	"github.com/phot0n/pathway/internal/transport/respond"
)

// proxyHandler is the bottom of the chain: every decision has been made above it, so all this does
// is forward to the engine that was picked and record what came back.
func (s *Server) proxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := middleware.From(r)
		target := state.Decision.EngineURL()
		if target == "" {
			// Nothing picked a route, which means the chain was assembled without one. A config
			// fault, not a caller's.
			s.log.Error("no route on an admitted request", "path", r.URL.Path)
			respond.Error(w, http.StatusInternalServerError, "gateway error")
			return
		}

		// A provider hop leaves our network, which is what decides whether its certificate is
		// checked — an engine's is one we signed, a vendor's is not. A route that rewrote the
		// model on the way out gets it swapped back on the way in: the response must speak the id
		// the client knows the serving model by, not the upstream's own spelling.
		route := state.Decision.Route
		state.Attempts++
		s.proxy.Forward(w, r, target, route.IsProvider(),
			proxy.ModelSwap{Upstream: route.UpstreamModel, Client: state.ServingModel()}, &state.Outcome)
	})
}

// root is what a bare GET / answers, so a browser aimed at the gateway sees something other than a
// 404.
func root(w http.ResponseWriter, _ *http.Request) {
	respond.JSON(w, map[string]any{
		"status":  "ok",
		"message": "Grove Gateway Service",
		"usage":   "POST /v1/chat/completions, or /anthropic/v1/messages for Anthropic clients",
	})
}
