// Package admission answers who is calling and whether they may call this model. It is the first
// hop of every request: bearer → key → group, then the pure decision over the two.
package admission

import (
	"context"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
)

// Identity is everything the rest of the request path needs to know about the caller. Carried in
// the request context from the auth middleware onward, so no later stage re-reads the store.
type Identity struct {
	MeterID string // sha256(secret) — the key's record id, and the usage bucket's owner
	Key     domain.KeyRecord
	Grant   domain.GroupRecord // every group the key is in, unioned into one grant
}

// Prefix is the API Key doc name: what usage accrues against and what appears in logs.
func (i Identity) Prefix() string { return i.Key.KeyPrefix }

type Service struct {
	keys   repository.Keys
	groups repository.Groups
	limits repository.Limits
	// Now is the clock the limit windows are read off. A field so a test can move it.
	Now func() time.Time
}

func New(keys repository.Keys, groups repository.Groups, limits repository.Limits) *Service {
	return &Service{keys: keys, groups: groups, limits: limits, Now: time.Now}
}

// Admit counts this request against the key's rate limits, and refuses with a 429 naming the
// limit already spent. A key with none costs no store call.
func (s *Service) Admit(ctx context.Context, id Identity) error {
	if len(id.Key.Limits) == 0 {
		return nil
	}
	now := s.Now()
	exceeded, err := s.limits.Admit(ctx, id.Prefix(), id.Key.Limits, now)
	if err != nil {
		return domain.Deny(503, "limit store error")
	}
	if len(exceeded) > 0 {
		return domain.LimitDenial(exceeded, now)
	}
	return nil
}

// Identify resolves an Authorization header to its key. One read: the key and the groups it is
// in come back together.
func (s *Service) Identify(ctx context.Context, authorization string) (Identity, error) {
	secret := domain.Bearer(authorization)
	if secret == "" {
		return Identity{}, domain.Deny(401, "missing api key")
	}
	meterID := domain.SHA256Hex(secret)

	holder, found, err := s.keys.Resolve(ctx, meterID)
	if err != nil {
		return Identity{}, domain.Deny(503, "key store error")
	}
	if !found {
		return Identity{}, domain.Deny(401, "unknown api key")
	}

	grant, err := s.grant(ctx, holder.Key.Groups, holder.Groups)
	if err != nil {
		return Identity{}, domain.Deny(503, "group store error")
	}
	return Identity{MeterID: meterID, Key: holder.Key, Grant: grant}, nil
}

// grant is the union of what every group the key is in grants. Always a fresh map: the store hands
// back its own set by reference, and unioning into that would edit the group itself.
//
// The names are the key's; read is what the store fetched beside it. A name it did not fetch is
// read on its own, so a store that splits a group list differently costs a round trip, not a grant.
func (s *Service) grant(ctx context.Context, names map[string]bool, read map[string]domain.GroupRecord) (domain.GroupRecord, error) {
	if len(names) == 0 {
		return domain.GroupRecord{}, nil
	}
	models := map[string]bool{}
	for name := range names {
		grp, ok := read[name]
		if !ok {
			var err error
			if grp, err = s.groups.Get(ctx, name); err != nil {
				return domain.GroupRecord{}, err
			}
		}
		for model := range grp.Models {
			models[model] = true
		}
	}
	return domain.GroupRecord{Models: models}, nil
}

// Authorize is the admission decision itself: pure, and the same one /v1/models filters its list
// with, so the catalogue can never disagree with what the inference path admits.
func (s *Service) Authorize(id Identity, model string) error {
	if status, reason := domain.Evaluate(id.Key, id.Grant, model); status != 200 {
		return domain.Deny(status, reason)
	}
	return nil
}
