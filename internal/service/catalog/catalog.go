// Package catalog answers what models a caller may see. Two lists: what a key is entitled to, and
// what is on public offer to someone who has not signed up yet.
package catalog

import (
	"context"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
	"github.com/phot0n/pathway/internal/service/admission"
)

type Service struct {
	routes  repository.Routes
	catalog repository.Catalog
}

func New(routes repository.Routes, public repository.Catalog) *Service {
	return &Service{routes: routes, catalog: public}
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
		if domain.CanUse(id.User, id.Grant, model) {
			out = append(out, model)
		}
	}
	return out, nil
}

// Public is what an unauthenticated caller sees: the advertised set intersected with what is
// actually deployed. It grants nothing — CanUse is untouched, so calling one without a key is still
// refused on the inference path.
func (s *Service) Public(ctx context.Context) ([]string, error) {
	csv, found, err := s.catalog.Get(ctx)
	if err != nil {
		return nil, domain.Deny(503, "catalog store error")
	}
	if !found {
		return nil, nil
	}
	advertised := domain.ModelSet(csv)
	if len(advertised) == 0 {
		return nil, nil
	}
	deployed, err := s.routes.Models(ctx)
	if err != nil {
		return nil, domain.Deny(503, "route store error")
	}
	return Intersect(advertised, deployed), nil
}

// Intersect keeps the deployed order routes.Models already sorted, and is pure so the rule is
// testable without a store. Advertising a model that is gone is worse than omitting it — the caller
// signs up, calls it, and gets a 503.
func Intersect(advertised map[string]bool, deployed []string) []string {
	out := make([]string, 0, len(deployed))
	for _, model := range deployed {
		if advertised[model] {
			out = append(out, model)
		}
	}
	return out
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
