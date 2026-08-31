// Package admission answers who is calling and whether they may call this model. It is the first
// two hops of every request: bearer → key → user → group, then the pure decision over the three.
package admission

import (
	"context"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
)

// Identity is everything the rest of the request path needs to know about the caller. Carried in
// the request context from the auth middleware onward, so no later stage re-reads the store.
type Identity struct {
	MeterID string // sha256(secret) — the key's record id, and the usage bucket's owner
	Key     domain.KeyRecord
	User    domain.UserRecord
	Grant   domain.GroupRecord // every group the user is in, unioned into one grant
}

// Prefix is the API Key doc name: what usage accrues against and what appears in logs.
func (i Identity) Prefix() string { return i.Key.KeyPrefix }

type Service struct {
	keys   repository.Keys
	users  repository.Users
	groups repository.Groups
}

func New(keys repository.Keys, users repository.Users, groups repository.Groups) *Service {
	return &Service{keys: keys, users: users, groups: groups}
}

// Identify resolves an Authorization header to its holder. Two reads plus one per group they are
// in, and only the two for an ungrouped user — who reaches nothing but their own Allow list, so a
// group read would cost a round trip to learn nothing.
func (s *Service) Identify(ctx context.Context, authorization string) (Identity, error) {
	secret := domain.Bearer(authorization)
	if secret == "" {
		return Identity{}, domain.Deny(401, "missing api key")
	}
	meterID := domain.SHA256Hex(secret)

	rec, found, err := s.keys.Get(ctx, meterID)
	if err != nil {
		return Identity{}, domain.Deny(503, "key store error")
	}
	if !found {
		return Identity{}, domain.Deny(401, "unknown api key")
	}

	usr, err := s.resolveUser(ctx, rec)
	if err != nil {
		return Identity{}, domain.Deny(503, "user store error")
	}
	grant, err := s.grant(ctx, usr.Groups)
	if err != nil {
		return Identity{}, domain.Deny(503, "group store error")
	}
	return Identity{MeterID: meterID, Key: rec, User: usr, Grant: grant}, nil
}

// grant is the union of what every group the user is in grants. Always a fresh map: the store hands
// back its own set by reference, and unioning into that would edit the group itself.
//
// ponytail: one sequential HGETALL per group, which is one round trip for the overwhelmingly common
// single-group user. Add a Groups.GetMany that pipelines if anyone ever holds more than a handful.
func (s *Service) grant(ctx context.Context, names map[string]bool) (domain.GroupRecord, error) {
	if len(names) == 0 {
		return domain.GroupRecord{}, nil
	}
	models := map[string]bool{}
	for name := range names {
		grp, err := s.groups.Get(ctx, name)
		if err != nil {
			return domain.GroupRecord{}, err
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
	if status, reason := domain.Evaluate(id.Key, id.User, id.Grant, model); status != 200 {
		return domain.Deny(status, reason)
	}
	return nil
}

// resolveUser is the second hop: key → user. A key whose user record is missing falls back to what
// the key itself carries, which is how a box still holding pre-split records keeps serving. A key
// with nothing to fall back to reaches nothing.
func (s *Service) resolveUser(ctx context.Context, rec domain.KeyRecord) (domain.UserRecord, error) {
	if rec.User != "" {
		usr, found, err := s.users.Get(ctx, rec.User)
		if err != nil {
			return domain.UserRecord{}, err
		}
		if found {
			return usr, nil
		}
	}
	if !rec.Legacy.HasProjection() {
		return domain.UserRecord{}, nil
	}
	return domain.SynthUser(rec), nil
}
