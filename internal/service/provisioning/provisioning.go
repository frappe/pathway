// Package provisioning is the control plane's push/pull surface, one layer below the HTTP handlers
// that expose it. Grove is the source of truth; these calls project its state into this box's store.
package provisioning

import (
	"context"
	"log/slog"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
)

type Service struct {
	store repository.Store
	log   *slog.Logger
}

func New(store repository.Store, log *slog.Logger) *Service {
	return &Service{store: store, log: log}
}

func (s *Service) UpsertKeys(ctx context.Context, records []repository.KeyUpsert) error {
	return s.store.Keys.Upsert(ctx, records)
}

func (s *Service) DeleteKeys(ctx context.Context, ids []string) (int, error) {
	return s.store.Keys.Delete(ctx, ids)
}

func (s *Service) UpsertUsers(ctx context.Context, records []repository.UserUpsert) error {
	return s.store.Users.Upsert(ctx, records)
}

func (s *Service) DeleteUsers(ctx context.Context, ids []string) (int, error) {
	return s.store.Users.Delete(ctx, ids)
}

func (s *Service) UpsertGroups(ctx context.Context, records []repository.GroupUpsert) error {
	return s.store.Groups.Upsert(ctx, records)
}

// ReplaceRoutes replaces the table per model named; an empty list retires that model, which is what
// makes it 503 rather than keep a stale placement. `prune` says the payload is the COMPLETE table,
// so anything unnamed is deleted — otherwise retiring one model means naming all of them.
func (s *Service) ReplaceRoutes(ctx context.Context, table map[string][]domain.Route, prune bool) (models, pruned int, err error) {
	var retired []string
	for model, routes := range table {
		if len(routes) == 0 {
			retired = append(retired, model)
			continue
		}
		if err := s.store.Routes.Replace(ctx, model, routes); err != nil {
			return 0, 0, err
		}
	}
	if err := s.store.Routes.DeleteModels(ctx, retired); err != nil {
		return 0, 0, err
	}
	if !prune {
		return len(table), 0, nil
	}
	stale, err := s.staleModels(ctx, table)
	if err != nil {
		// The writes above already landed, so the table is correct and merely wider than it should
		// be. Saying so beats failing a push that did its real work.
		s.log.Warn("routes pruned partially", "err", err)
		return len(table), 0, nil
	}
	if err := s.store.Routes.DeleteModels(ctx, stale); err != nil {
		s.log.Warn("routes pruned partially", "err", err)
		return len(table), 0, nil
	}
	return len(table), len(stale), nil
}

// staleModels is every held model the payload did not name.
func (s *Service) staleModels(ctx context.Context, keep map[string][]domain.Route) ([]string, error) {
	held, err := s.store.Routes.Models(ctx)
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, model := range held {
		if _, named := keep[model]; !named {
			stale = append(stale, model)
		}
	}
	return stale, nil
}

// DrainUsage atomically reads and deletes every live counter, so the snapshot returned is the only
// copy. No second round trip: a failed insert control-plane-side drops that cycle rather than
// double-counting it.
func (s *Service) DrainUsage(ctx context.Context) (map[string]map[string]string, error) {
	return s.store.Usage.Drain(ctx)
}

// StateHashes is what this box holds, per section/bucket — the control plane diffs its desired
// state against this and pushes only what differs. Empty on a fresh or wiped store, which is
// what makes the next push carry everything.
func (s *Service) StateHashes(ctx context.Context) (map[string]string, error) {
	return s.store.State.Hashes(ctx)
}

// ApplyState projects a desired-state push (plan_agent_state_sync.md): upsert what each present
// section names, delete what it does not, store the carried hashes — one transaction. An error
// means none of it landed, and the control plane retries on its next tick.
func (s *Service) ApplyState(ctx context.Context, push repository.StatePush) (repository.StateCounts, error) {
	return s.store.State.Apply(ctx, push)
}
