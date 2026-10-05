// Package memory is a map-backed repository, test-only, and the reason the services are testable at
// all. Deliberately not a mock: it behaves, so a test states what the store held and what the
// service should answer, rather than which calls it expected.
package memory

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
)

// Store is every repository over one set of maps. Fail makes any call return an error, which is
// how the degrade-rather-than-refuse paths get tested.
type Store struct {
	mu sync.Mutex

	Keys     map[string]domain.KeyRecord
	Users    map[string]domain.UserRecord
	Groups   map[string]domain.GroupRecord
	Routes   map[string][]domain.Route
	Sticky   map[string]string
	InFlight map[string]map[string]bool // engine → request ids
	Failures map[string]int
	// KeyStats is what each vendor credential answered, the memory twin of pk:<id>.
	KeyStats map[string]domain.KeyStats
	Usage    map[string]map[string]int64
	// Limits is every limit counter, "<user>:<metric>:<window>:<bucket>" → count: the memory twin
	// of lim:….
	Limits map[string]int64
	Hashes map[string]string // grove:state_hash — section/bucket → hash

	// Drained is every counter set aside, drain id → prefix → counters; Unacked the "<id>:<prefix>"
	// pairs not yet acknowledged; Retained how long each acknowledged pair is kept. Adjusted is
	// every spend adjustment id already applied.
	Drained map[string]map[string]map[string]int64
	Unacked map[string]bool
	// Accrued is the request ids a replay has landed, the memory twin of accrued:<id>.
	Accrued  map[string]bool
	Retained map[string]time.Duration
	Adjusted map[string]bool

	// Fail names the repositories that should error, by interface name ("routes", "inflight",
	// "health", "sessions", "keys", "users", "groups", "usage", "limits", "state", "providerkeys").
	Fail map[string]bool
}

var errStore = errors.New("store unavailable")

func New() *Store {
	return &Store{
		Keys: map[string]domain.KeyRecord{}, Users: map[string]domain.UserRecord{},
		Groups: map[string]domain.GroupRecord{}, Routes: map[string][]domain.Route{},
		Sticky: map[string]string{}, InFlight: map[string]map[string]bool{},
		Failures: map[string]int{}, KeyStats: map[string]domain.KeyStats{}, Usage: map[string]map[string]int64{},
		Limits: map[string]int64{}, Hashes: map[string]string{}, Fail: map[string]bool{},
		Drained: map[string]map[string]map[string]int64{}, Unacked: map[string]bool{}, Accrued: map[string]bool{},
		Retained: map[string]time.Duration{}, Adjusted: map[string]bool{},
	}
}

// Repositories hands this store out behind the interfaces.
func (s *Store) Repositories() repository.Store {
	return repository.Store{
		Keys: keys{s}, Users: users{s}, Groups: groups{s}, Routes: routes{s},
		Sessions: sessions{s}, InFlight: inFlight{s}, Health: health{s},
		Usage: usage{s}, Limits: limits{s}, State: state{s}, ProviderKeys: providerKeys{s},
	}
}

func (s *Store) failed(name string) error {
	if s.Fail[name] {
		return errStore
	}
	return nil
}

// putUser matches the real store's HSET-merge: a push writes its fields and leaves Spent alone.
func (s *Store) putUser(rec repository.UserUpsert) {
	limits, _ := domain.ParseLimits(rec.Limits) // the push handler already refused one that does not read
	s.Users[rec.Name] = domain.UserRecord{
		Email: rec.Email, Groups: domain.ModelSet(rec.Groups),
		Allow: domain.ModelSet(rec.Allow), Deny: domain.ModelSet(rec.Deny),
		Limited: rec.Limited, LogPayloads: rec.LogPayloads, Geography: rec.Geography,
		Prepaid: rec.Prepaid, Budget: rec.Budget, Spent: s.Users[rec.Name].Spent,
		Limits: limits,
	}
}

type keys struct{ s *Store }

// Resolve matches the real script: a record is asked for only when it is read, so a failing users
// or groups store is felt only by a key that reaches it.
func (k keys) Resolve(_ context.Context, meterID string) (repository.Holder, bool, error) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	if err := k.s.failed("keys"); err != nil {
		return repository.Holder{}, false, err
	}
	rec, ok := k.s.Keys[meterID]
	if !ok {
		return repository.Holder{}, false, nil
	}
	holder := repository.Holder{Key: rec, Groups: map[string]domain.GroupRecord{}}
	names := domain.ModelSet(rec.Legacy.Group)
	if rec.User != "" {
		if err := k.s.failed("users"); err != nil {
			return repository.Holder{}, false, err
		}
		if holder.User, holder.HasUser = k.s.Users[rec.User]; holder.HasUser {
			names = holder.User.Groups
		}
	}
	if len(names) > 0 {
		if err := k.s.failed("groups"); err != nil {
			return repository.Holder{}, false, err
		}
	}
	for name := range names {
		holder.Groups[name] = k.s.Groups[name]
	}
	return holder, true, nil
}

func (k keys) Upsert(_ context.Context, records []repository.KeyUpsert) error {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	if err := k.s.failed("keys"); err != nil {
		return err
	}
	for _, rec := range records {
		if rec.MeterID == "" {
			continue
		}
		k.s.Keys[rec.MeterID] = domain.KeyRecord{
			Status: rec.Status, User: rec.User, KeyPrefix: rec.Prefix, CanReadBalance: rec.CanReadBalance,
		}
	}
	return nil
}

func (k keys) Delete(_ context.Context, ids []string) (int, error) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	return deleteFrom(ids, func(id string) { delete(k.s.Keys, id) }), k.s.failed("keys")
}

type users struct{ s *Store }

func (u users) Get(_ context.Context, name string) (domain.UserRecord, bool, error) {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	if err := u.s.failed("users"); err != nil {
		return domain.UserRecord{}, false, err
	}
	rec, ok := u.s.Users[name]
	return rec, ok, nil
}

func (u users) Upsert(_ context.Context, records []repository.UserUpsert) error {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	if err := u.s.failed("users"); err != nil {
		return err
	}
	for _, rec := range records {
		if rec.Name == "" {
			continue
		}
		u.s.putUser(rec)
	}
	return nil
}

func (u users) Delete(_ context.Context, ids []string) (int, error) {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	return deleteFrom(ids, func(id string) { delete(u.s.Users, id) }), u.s.failed("users")
}

// AdjustSpent matches the real one: once per id, and only on a holder the store knows.
func (u users) AdjustSpent(_ context.Context, name, id string, delta int64) (int64, bool, bool, error) {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	if err := u.s.failed("users"); err != nil {
		return 0, false, false, err
	}
	holder, found := u.s.Users[name]
	if !found || u.s.Adjusted[id] {
		return holder.Spent, false, found, nil
	}
	holder.Spent += delta
	u.s.Users[name] = holder
	u.s.Adjusted[id] = true
	return holder.Spent, true, true, nil
}

type groups struct{ s *Store }

func (g groups) Get(_ context.Context, name string) (domain.GroupRecord, error) {
	g.s.mu.Lock()
	defer g.s.mu.Unlock()
	if err := g.s.failed("groups"); err != nil {
		return domain.GroupRecord{}, err
	}
	return g.s.Groups[name], nil // a group never pushed is the zero value, not an error
}

func (g groups) Upsert(_ context.Context, records []repository.GroupUpsert) error {
	g.s.mu.Lock()
	defer g.s.mu.Unlock()
	if err := g.s.failed("groups"); err != nil {
		return err
	}
	for _, rec := range records {
		if rec.Name == "" {
			continue
		}
		g.s.Groups[rec.Name] = domain.GroupRecord{
			Models: domain.ModelSet(rec.Models),
		}
	}
	return nil
}

type routes struct{ s *Store }

func (r routes) Get(_ context.Context, model string) ([]domain.Route, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if err := r.s.failed("routes"); err != nil {
		return nil, err
	}
	// Copied: PickRoute's caller fills InFlight in place, and a test that ran twice would
	// otherwise see the first run's counts.
	return append([]domain.Route(nil), r.s.Routes[model]...), nil
}

func (r routes) Models(_ context.Context) ([]string, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if err := r.s.failed("routes"); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(r.s.Routes))
	for model := range r.s.Routes {
		out = append(out, model)
	}
	sort.Strings(out)
	return out, nil
}

func (r routes) Replace(_ context.Context, model string, table []domain.Route) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if err := r.s.failed("routes"); err != nil {
		return err
	}
	r.s.Routes[model] = table
	return nil
}

func (r routes) DeleteModels(_ context.Context, models []string) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if err := r.s.failed("routes"); err != nil {
		return err
	}
	for _, model := range models {
		delete(r.s.Routes, model)
	}
	return nil
}

type sessions struct{ s *Store }

func (s sessions) Engine(_ context.Context, session string) (string, error) {
	s.s.mu.Lock()
	defer s.s.mu.Unlock()
	if err := s.s.failed("sessions"); err != nil {
		return "", err
	}
	return s.s.Sticky[session], nil
}

func (s sessions) Pin(_ context.Context, session, engineURL string, _ time.Duration) error {
	s.s.mu.Lock()
	defer s.s.mu.Unlock()
	if err := s.s.failed("sessions"); err != nil {
		return err
	}
	s.s.Sticky[session] = engineURL
	return nil
}

type inFlight struct{ s *Store }

func (f inFlight) Counts(_ context.Context, engineURLs []string) ([]int, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if err := f.s.failed("inflight"); err != nil {
		return nil, err
	}
	counts := make([]int, len(engineURLs))
	for i, url := range engineURLs {
		counts[i] = len(f.s.InFlight[url])
	}
	return counts, nil
}

func (f inFlight) Claim(_ context.Context, engineURL, requestID string) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if err := f.s.failed("inflight"); err != nil {
		return err
	}
	if f.s.InFlight[engineURL] == nil {
		f.s.InFlight[engineURL] = map[string]bool{}
	}
	f.s.InFlight[engineURL][requestID] = true
	return nil
}

func (f inFlight) Release(_ context.Context, engineURL, requestID string) error {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if err := f.s.failed("inflight"); err != nil {
		return err
	}
	delete(f.s.InFlight[engineURL], requestID)
	return nil
}

type health struct{ s *Store }

func (h health) Failures(_ context.Context, targets []string) ([]int, error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if err := h.s.failed("health"); err != nil {
		return nil, err
	}
	counts := make([]int, len(targets))
	for i, target := range targets {
		counts[i] = h.s.Failures[target]
	}
	return counts, nil
}

func (h health) RecordFailure(_ context.Context, target string) error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if err := h.s.failed("health"); err != nil {
		return err
	}
	h.s.Failures[target]++
	return nil
}

func (h health) RecordSuccess(_ context.Context, target string) error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if err := h.s.failed("health"); err != nil {
		return err
	}
	delete(h.s.Failures, target)
	return nil
}

type providerKeys struct{ s *Store }

func (p providerKeys) Count(_ context.Context, id string, status int) error {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if err := p.s.failed("providerkeys"); err != nil {
		return err
	}
	stats, now := p.s.KeyStats[id], time.Now().Unix()
	stats.Requests++
	stats.LastUsed = now
	switch domain.KeyStatusClass(status) {
	case "rate_limited":
		stats.RateLimited++
		stats.LastRateLimited = now
	case "rejected":
		stats.Rejected++
	case "ok":
		stats.OK++
	default:
		stats.Failed++
	}
	p.s.KeyStats[id] = stats
	return nil
}

func (p providerKeys) Stats(_ context.Context, ids []string) (map[string]domain.KeyStats, error) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if err := p.s.failed("providerkeys"); err != nil {
		return nil, err
	}
	stats := make(map[string]domain.KeyStats, len(ids))
	for _, id := range ids {
		stats[id] = p.s.KeyStats[id]
	}
	return stats, nil
}

type limits struct{ s *Store }

func limitKey(user string, limit domain.Limit, now time.Time) string {
	label, _, _ := limit.Bucket(now)
	return user + ":" + limit.Metric + ":" + limit.Window + ":" + label
}

// Admit matches the real script: every limit is checked before any is counted, under one lock.
func (l limits) Admit(_ context.Context, user string, lims []domain.Limit, now time.Time) ([]domain.Limit, error) {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if err := l.s.failed("limits"); err != nil {
		return nil, err
	}
	var exceeded []domain.Limit
	for _, limit := range lims {
		if l.s.Limits[limitKey(user, limit, now)] >= limit.Value {
			exceeded = append(exceeded, limit)
		}
	}
	if len(exceeded) > 0 {
		return exceeded, nil
	}
	for _, limit := range lims {
		if limit.Metric == domain.LimitRequests {
			l.s.Limits[limitKey(user, limit, now)]++
		}
	}
	return nil, nil
}

func (l limits) Debit(_ context.Context, user string, lims []domain.Limit, tokens int64, now time.Time) error {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if err := l.s.failed("limits"); err != nil {
		return err
	}
	for _, limit := range lims {
		l.s.Limits[limitKey(user, limit, now)] += tokens
	}
	return nil
}

type usage struct{ s *Store }

// Accrue matches the real one: counters, cost and the holder's spend land under one lock, and the
// spend moves only on a holder the store knows.
func (u usage) Accrue(_ context.Context, a repository.Accrual) error {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	if err := u.s.failed("usage"); err != nil {
		return err
	}
	u.accrue(a)
	return nil
}

// Replay matches the real one: once per request id.
func (u usage) Replay(_ context.Context, a repository.Accrual) (bool, error) {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	if err := u.s.failed("usage"); err != nil {
		return false, err
	}
	if u.s.Accrued[a.ID] {
		return false, nil
	}
	u.s.Accrued[a.ID] = true
	u.accrue(a)
	return true, nil
}

func (u usage) Ping(_ context.Context) error {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	return u.s.failed("ping")
}

func (u usage) accrue(a repository.Accrual) {
	if u.s.Usage[a.Prefix] == nil {
		u.s.Usage[a.Prefix] = map[string]int64{}
	}
	for field, n := range a.Fields {
		u.s.Usage[a.Prefix][field] += n
	}
	holder, known := u.s.Users[a.User]
	if a.User == "" || !known {
		return
	}
	holder.Spent += a.Cost
	u.s.Users[a.User] = holder
	u.s.Usage[a.Prefix]["user_spent"] = holder.Spent
	u.s.Usage[a.Prefix]["user_balance"] = a.Budget - holder.Spent
}

// Drain matches the real one: live counters (all, or only keys) are set aside under newID, and
// every unacknowledged pair is answered. Nothing is deleted until Ack.
func (u usage) Drain(_ context.Context, newID string, keys []string) (repository.Drains, error) {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	if err := u.s.failed("usage"); err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, key := range keys {
		wanted[key] = true
	}
	for prefix, fields := range u.s.Usage {
		if len(wanted) > 0 && !wanted[prefix] {
			continue
		}
		if u.s.Drained[newID] == nil {
			u.s.Drained[newID] = map[string]map[string]int64{}
		}
		u.s.Drained[newID][prefix] = fields
		u.s.Unacked[newID+":"+prefix] = true
		delete(u.s.Usage, prefix)
	}
	out := repository.Drains{}
	for member := range u.s.Unacked {
		id, prefix, _ := strings.Cut(member, ":")
		if len(wanted) > 0 && !wanted[prefix] {
			continue
		}
		snapshot := map[string]string{}
		for field, n := range u.s.Drained[id][prefix] {
			snapshot[field] = strconv.FormatInt(n, 10)
		}
		if out[id] == nil {
			out[id] = map[string]map[string]string{}
		}
		out[id][prefix] = snapshot
	}
	return out, nil
}

// Ack matches the real one: only unacknowledged pairs move anything.
func (u usage) Ack(_ context.Context, acks map[string][]string, retention time.Duration) (int, error) {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	if err := u.s.failed("usage"); err != nil {
		return 0, err
	}
	count := 0
	for id, prefixes := range acks {
		for _, prefix := range prefixes {
			if member := id + ":" + prefix; u.s.Unacked[member] {
				delete(u.s.Unacked, member)
				u.s.Retained[member] = retention
				count++
			}
		}
	}
	return count, nil
}

type state struct{ s *Store }

func (st state) Hashes(_ context.Context) (map[string]string, error) {
	st.s.mu.Lock()
	defer st.s.mu.Unlock()
	if err := st.s.failed("state"); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range st.s.Hashes {
		out[k] = v
	}
	return out, nil
}

// Apply matches the real store: a present section is authoritative for its namespace, a bucket
// prunes only its own members, and the hashes land with the records — or, on Fail, not at all.
func (st state) Apply(_ context.Context, push repository.StatePush) (repository.StateCounts, error) {
	st.s.mu.Lock()
	defer st.s.mu.Unlock()
	var counts repository.StateCounts
	if err := st.s.failed("state"); err != nil {
		return counts, err
	}
	if push.Groups != nil {
		named := map[string]bool{}
		for _, rec := range push.Groups.Records {
			if rec.Name == "" {
				continue
			}
			named[rec.Name] = true
			st.s.Groups[rec.Name] = domain.GroupRecord{Models: domain.ModelSet(rec.Models)}
		}
		for name := range st.s.Groups {
			if !named[name] {
				delete(st.s.Groups, name)
			}
		}
		st.s.Hashes["groups"] = push.Groups.Hash
		counts.Groups = len(named)
	}
	if push.Users != nil {
		named := map[string]bool{}
		for label, bucket := range push.Users {
			for _, rec := range bucket.Records {
				if rec.Name == "" {
					continue
				}
				named[rec.Name] = true
				counts.Users++
				st.s.putUser(rec)
			}
			st.setBucketHash("users:"+label, bucket.Hash, len(bucket.Records))
		}
		for name := range st.s.Users {
			if _, pushed := push.Users[domain.BucketOf(name)]; pushed && !named[name] {
				delete(st.s.Users, name)
			}
		}
	}
	if push.Keys != nil {
		named := map[string]bool{}
		for label, bucket := range push.Keys {
			for _, rec := range bucket.Records {
				if rec.MeterID == "" {
					continue
				}
				named[rec.MeterID] = true
				counts.Keys++
				st.s.Keys[rec.MeterID] = domain.KeyRecord{
					Status: rec.Status, User: rec.User, KeyPrefix: rec.Prefix, CanReadBalance: rec.CanReadBalance,
				}
			}
			st.setBucketHash("keys:"+label, bucket.Hash, len(bucket.Records))
		}
		for id := range st.s.Keys {
			if _, pushed := push.Keys[domain.BucketOf(id)]; pushed && !named[id] {
				delete(st.s.Keys, id)
			}
		}
	}
	if push.Routes != nil {
		named := map[string]bool{}
		for model, table := range push.Routes.Table {
			if len(table) == 0 {
				continue
			}
			named[model] = true
			counts.Routes++
			st.s.Routes[model] = table
		}
		for model := range st.s.Routes {
			if !named[model] {
				delete(st.s.Routes, model)
			}
		}
		st.s.Hashes["routes"] = push.Routes.Hash
	}
	return counts, nil
}

func (st state) setBucketHash(field, hash string, records int) {
	if records == 0 {
		delete(st.s.Hashes, field)
		return
	}
	st.s.Hashes[field] = hash
}

// deleteFrom skips blank ids, matching the real store: a blank one would name the key prefix
// itself, which is a different record.
func deleteFrom(ids []string, remove func(string)) int {
	count := 0
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		remove(id)
		count++
	}
	return count
}
