// Package provisioning is the control plane's push/pull surface, one layer below the HTTP handlers
// that expose it. Grove is the source of truth; these calls project its state into this box's store.
package provisioning

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/frappe/pathway/internal/domain"
	"github.com/frappe/pathway/internal/repository"
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

func (s *Service) UpsertGroups(ctx context.Context, records []repository.GroupUpsert) error {
	return s.store.Groups.Upsert(ctx, records)
}

// ReplaceRoutes replaces the table per model named; an empty list retires that model, which is what
// makes it 503 rather than keep a stale placement. `prune` says the payload is the COMPLETE table,
// so anything unnamed is deleted — otherwise retiring one model means naming all of them.
func (s *Service) ReplaceRoutes(ctx context.Context, table map[string][]domain.Route, prune bool) (models, pruned int, err error) {
	if err := s.oneKindEach(table); err != nil {
		return 0, 0, err
	}
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

// DrainUsage sets live counters aside under a new id (only keys' when given) and answers every
// counter not yet acknowledged. Nothing is deleted until AckUsage, so a key the control plane fails
// to record comes back under its own id on its next pull.
func (s *Service) DrainUsage(ctx context.Context, keys []string) (repository.Drains, error) {
	return s.store.Usage.Drain(ctx, repository.NewDrainID(time.Now()), keys)
}

// AckUsage says these (drain, prefix, day) counters are recorded: kept for retention, then expired.
func (s *Service) AckUsage(ctx context.Context, acks repository.Acks, retention time.Duration) (int, error) {
	return s.store.Usage.Ack(ctx, acks, retention)
}

// AdjustSpent corrects a key's spend for the current month on this store, once per id.
func (s *Service) AdjustSpent(ctx context.Context, meterID, id string, delta int64) (int64, bool, bool, error) {
	return s.store.Keys.AdjustSpent(ctx, meterID, id, delta)
}

// Spent answers each key's spend this month on this store in nano-USD, by the id it was pushed
// under: the control plane's figure for a cap check, off the counter the cap is gated on. A key
// not held here is left out.
func (s *Service) Spent(ctx context.Context, ids []string) (map[string]int64, error) {
	return s.store.Keys.Spent(ctx, ids)
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
	if push.Routes != nil {
		if err := s.oneKindEach(push.Routes.Table); err != nil {
			return repository.StateCounts{}, err
		}
	}
	return s.store.State.Apply(ctx, push)
}

// oneKindEach refuses a push before any of it lands when a model's rows are both a vendor's and
// ours (domain.MixedKinds): routing hands a whole table to one router. The whole push, not the one
// model: a push is one transaction, and the control plane retries it each tick until the row is
// fixed — so the model is named here too, where an operator with no control-plane access looks.
// Models are walked in order so the one named is the same on every push.
func (s *Service) oneKindEach(table map[string][]domain.Route) error {
	for _, model := range slices.Sorted(maps.Keys(table)) {
		if domain.MixedKinds(table[model]) {
			s.log.Warn("routes push refused: model mixes vendor and self-hosted rows", "model", model)
			return domain.Deny(400, model+" mixes vendor and self-hosted rows")
		}
	}
	return nil
}
