// Package catalog answers what models a key may see.
package catalog

import (
	"context"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
	"github.com/phot0n/pathway/internal/service/admission"
)

type Service struct {
	routes repository.Routes
}

func New(routes repository.Routes) *Service {
	return &Service{routes: routes}
}

// ForIdentity is the models this key may use: what is deployed, intersected with what its groups
// and its own allow/deny resolve to. The filter is domain.CanUse — literally the decision the inference
// path makes — so the list can never disagree with what a request would be admitted for.
func (s *Service) ForIdentity(ctx context.Context, id admission.Identity) ([]string, error) {
	deployed, err := s.routes.Models(ctx)
	if err != nil {
		return nil, domain.Deny(503, "route store error")
	}
	out := make([]string, 0, len(deployed))
	for _, model := range deployed {
		if domain.CanUse(id.Key, id.Grant, model) {
			out = append(out, model)
		}
	}
	return out, nil
}

// SpeakingDialect narrows a model list to those with at least one placement that natively
// answers the dialect. Dialect is end-to-end — nothing translates — so a model whose every
// route speaks the other shape would 404 the surface this list is served on, and advertising
// it there is the /v1/models version of a 503. An unreadable table keeps the model: a store
// hiccup must not empty the catalogue.
func (s *Service) SpeakingDialect(ctx context.Context, models []string, dialect string) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		table, err := s.routes.Get(ctx, model)
		if err != nil {
			out = append(out, model)
			continue
		}
		for _, route := range table {
			if route.SpeaksDialect(dialect) {
				out = append(out, model)
				break
			}
		}
	}
	return out
}
