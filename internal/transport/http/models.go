package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/transport/http/middleware"
	"github.com/phot0n/pathway/internal/transport/respond"
)

// GET /v1/models — the gateway answers directly with the models THIS key may use, instead of
// proxying to a single engine (which only knows its own model).

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// Each surface lists only what is callable through it: nothing translates, so a model whose every
// placement speaks the other dialect would 404 there and is left off.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if models, ok := s.modelsForCaller(w, r); ok {
		writeModelList(w, s.catalog.SpeakingDialect(r.Context(), models, domain.DialectOpenAI))
	}
}

// The same allow-list under /anthropic, in that dialect's shape.
func (s *Server) handleAnthropicModels(w http.ResponseWriter, r *http.Request) {
	if models, ok := s.modelsForCaller(w, r); ok {
		writeAnthropicModelList(w, s.catalog.SpeakingDialect(r.Context(), models, domain.DialectAnthropic))
	}
}

// A body sent to the list is a request meant for an inference path, so the answer names the method.
func modelListIsGetOnly(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", http.MethodGet)
	respond.TypedErrorFor(w, r, http.StatusMethodNotAllowed, "invalid_request_error",
		r.Method+" "+r.URL.Path+" is not allowed: the model list is GET only")
}

// modelsForCaller resolves the caller to the models they may see, writing the refusal itself
// when there are none to show.
func (s *Server) modelsForCaller(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	ctx := r.Context()
	identity, err := s.admission.Identify(ctx, middleware.Credential(r))
	if err != nil {
		respond.DenialFor(w, r, err)
		return nil, false
	}
	// Attribute the access line: auth happens here rather than in the chain, so the accesslog
	// stage wrapped around this handler would otherwise log every keyed listing as anonymous.
	middleware.From(r).Identity = identity
	// Revoked/inactive cannot list. Over budget still can: this shows what the key is entitled to,
	// and that lives on the user, so only inference is blocked.
	if identity.Key.Status != "active" {
		respond.ErrorFor(w, r, http.StatusUnauthorized, "unknown or revoked api key")
		return nil, false
	}
	if err := domain.GeographyDenial(identity.User, s.deps.Geography); err != nil {
		respond.DenialFor(w, r, err)
		return nil, false
	}
	models, err := s.catalog.ForIdentity(ctx, identity)
	if err != nil {
		respond.DenialFor(w, r, err)
		return nil, false
	}
	return models, true
}

// The id is `<provider>/<model>`, so owned_by is read off the id itself rather than pushed as a
// second field that could drift from it. An unprefixed id predates providers — it is ours.
func ownerOf(id string) string {
	if provider, _, found := strings.Cut(id, "/"); found {
		return provider
	}
	return "frappe"
}

func writeModelList(w http.ResponseWriter, models []string) {
	created := time.Now().Unix()
	data := make([]modelObject, 0, len(models))
	for _, id := range models {
		data = append(data, modelObject{ID: id, Object: "model", Created: created, OwnedBy: ownerOf(id)})
	}
	respond.JSON(w, map[string]any{"object": "list", "data": data})
}

type anthropicModelObject struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

func writeAnthropicModelList(w http.ResponseWriter, models []string) {
	created := time.Now().UTC().Format(time.RFC3339)
	data := make([]anthropicModelObject, 0, len(models))
	for _, id := range models {
		data = append(data, anthropicModelObject{Type: "model", ID: id, DisplayName: id, CreatedAt: created})
	}
	// first_id/last_id are pagination cursors; the whole list fits in one page, so they simply
	// name its edges and has_more stays false.
	var first, last any
	if len(data) > 0 {
		first, last = data[0].ID, data[len(data)-1].ID
	}
	respond.JSON(w, map[string]any{"data": data, "has_more": false, "first_id": first, "last_id": last})
}
