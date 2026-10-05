// Package admission answers who is calling and whether they may call this model. It is the first
// two hops of every request: bearer → key → user → group, then the pure decision over the three.
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
	User    domain.UserRecord
	Grant   domain.GroupRecord // every group the user is in, unioned into one grant
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

// Admit counts this request against the holder's rate limits, and refuses with a 429 naming the
// limit already spent. A holder with none costs no store call.
func (s *Service) Admit(ctx context.Context, id Identity) error {
	if len(id.User.Limits) == 0 {
		return nil
	}
	now := s.Now()
	exceeded, err := s.limits.Admit(ctx, id.Key.User, id.User.Limits, now)
	if err != nil {
		return domain.Deny(503, "limit store error")
	}
	if len(exceeded) > 0 {
		return domain.LimitDenial(exceeded, now)
	}
	return nil
}

// Identify resolves an Authorization header to its holder. One read: the key, its user and the
// groups that user is in come back together.
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

	usr := resolveUser(holder)
	grant, err := s.grant(ctx, usr.Groups, holder.Groups)
	if err != nil {
		return Identity{}, domain.Deny(503, "group store error")
	}
	return Identity{MeterID: meterID, Key: holder.Key, User: usr, Grant: grant}, nil
}

// grant is the union of what every group the user is in grants. Always a fresh map: the store hands
// back its own set by reference, and unioning into that would edit the group itself.
//
// The names are the user's; read is what the store fetched beside the key. A name it did not fetch
// is read on its own, so a store that splits a group list differently costs a round trip, not a grant.
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
	if status, reason := domain.Evaluate(id.Key, id.User, id.Grant, model); status != 200 {
		return domain.Deny(status, reason)
	}
	return nil
}

// resolveUser is the second hop: key → user. A key whose user record is missing falls back to what
// the key itself carries, which is how a box still holding pre-split records keeps serving. A key
// with nothing to fall back to reaches nothing.
func resolveUser(holder repository.Holder) domain.UserRecord {
	if holder.HasUser {
		return holder.User
	}
	if !holder.Key.Legacy.HasProjection() {
		return domain.UserRecord{}
	}
	return domain.SynthUser(holder.Key)
}
