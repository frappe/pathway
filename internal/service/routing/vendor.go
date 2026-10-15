package routing

import (
	"maps"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/frappe/pathway/internal/domain"
)

// pickVendor is the vendor router: a table of a third party's fronts, none of which we count. No
// slot is claimed — a vendor has no capacity of ours — no failure ejects a row, since it has no
// sibling to steer to, and no session is pinned, since a vendor keeps no prefix cache we can aim at.
// What it does pick is the credential, in turn round the row's ring.
func (s *Service) pickVendor(req Request, table []domain.Route) (Decision, error) {
	route, status := domain.PickRoute(table, "", s.region)
	if status != 200 {
		return Decision{}, domain.Deny(503, "no healthy server for model")
	}
	decision := Decision{Route: route, RequestID: req.RequestID, Key: s.PickKey(route.Keyring())}
	s.log.Debug("route picked",
		"model", req.Model, "engine", route.EngineURL, "kind", route.Kind, "vendor", route.Vendor,
		"key", decision.Key.ID, "candidates", len(table), "rid", decision.RequestID)
	return decision, nil
}

// PickKey is the route's one selection strategy, round robin: the credential a request dials with,
// the next in the ring's order after the last request's. The cursor is per ring, and a vendor's ring
// is the same on every model it serves, so its keys take even turns across models — the limits they
// share are the vendor's. A session still changes key between requests, so a vendor's prompt cache
// stays per credential; that is the strategy's known cost. Blank on an empty ring.
func (s *Service) PickKey(keyring []domain.Credential) domain.Credential {
	if len(keyring) == 0 {
		return domain.Credential{}
	}
	cursor, _ := s.cursors.LoadOrStore(ringID(keyring), new(atomic.Uint64))
	turn := cursor.(*atomic.Uint64).Add(1) - 1
	return keyring[turn%uint64(len(keyring))]
}

// ringID names a ring by its members: the ids the control plane counts the keys under, in the
// order it pushed them. An engine's one blank-id credential names the one ring every engine shares,
// which has a single position anyway.
func ringID(keyring []domain.Credential) string {
	ids := make([]string, len(keyring))
	for i, key := range keyring {
		ids[i] = key.ID
	}
	return strings.Join(ids, ",")
}

// KeyRing is one request's walk over a route's credentials, for the retry stage: which are spent,
// which are dead, and whether the rate-limited ones have been opened again.
type KeyRing struct {
	keys  []domain.Credential
	spent map[string]bool // rate-limited or dead this request
	dead  map[string]bool // refused outright: never opened again
	reset bool
}

func NewKeyRing(keys []domain.Credential) *KeyRing {
	return &KeyRing{keys: keys, spent: map[string]bool{}, dead: map[string]bool{}}
}

// Next retires `loser` for `status` and picks the key the next attempt dials with — the one after
// it round the ring, blank when none is left. The rate-limited keys are opened once when nothing
// else remains: a vendor's quota window may have slid by then. A second exhaustion is the answer
// the client gets.
func (k *KeyRing) Next(loser domain.Credential, status int) domain.Credential {
	k.spent[loser.ID] = true
	if domain.IsKeyRejected(status) {
		k.dead[loser.ID] = true
	}
	if winner := nextKey(k.keys, loser, k.spent); winner.ID != "" || k.reset {
		return winner
	}
	k.reset = true
	k.spent = maps.Clone(k.dead)
	return nextKey(k.keys, loser, k.spent)
}

// nextKey is the first key after `loser` in ring order that is not in `spent`, round the ring
// once. Blank when every key is spent.
func nextKey(keyring []domain.Credential, loser domain.Credential, spent map[string]bool) domain.Credential {
	start := slices.IndexFunc(keyring, func(key domain.Credential) bool { return key.ID == loser.ID })
	for step := 1; step <= len(keyring); step++ {
		if key := keyring[(start+step)%len(keyring)]; !spent[key.ID] {
			return key
		}
	}
	return domain.Credential{}
}
