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

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if models, ok := s.modelsForCaller(w, r); ok {
		writeModelList(w, models)
	}
}

// modelsForCaller resolves the caller to the models they may see, writing the refusal itself
// when there are none to show.
func (s *Server) modelsForCaller(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	ctx := r.Context()
	credential := r.Header.Get("Authorization")
	if domain.Bearer(credential) == "" {
		// No key at all → the public catalogue, so a prospect can see what is on offer before
		// signing up. A key that is present but wrong still 401s below: answering it with the
		// anonymous list would hide a broken key behind a shorter, plausible one.
		models, err := s.catalog.Public(ctx)
		if err != nil {
			respond.Denial(w, err)
			return nil, false
		}
		return models, true
	}

	identity, err := s.admission.Identify(ctx, credential)
	if err != nil {
		respond.Denial(w, err)
		return nil, false
	}
	// Attribute the access line: auth happens here rather than in the chain, so the accesslog
	// stage wrapped around this handler would otherwise log every keyed listing as anonymous.
	middleware.From(r).Identity = identity
	// Revoked/inactive cannot list. Over budget still can: this shows what the key is entitled to,
	// and that lives on the user, so only inference is blocked.
	if identity.Key.Status != "active" {
		respond.Error(w, http.StatusUnauthorized, "unknown or revoked api key")
		return nil, false
	}
	models, err := s.catalog.ForIdentity(ctx, identity)
	if err != nil {
		respond.Denial(w, err)
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
