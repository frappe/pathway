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

	"github.com/frappe/pathway/internal/domain"
	"github.com/frappe/pathway/internal/repository"
)

// Store is every repository over one set of maps. Fail makes any call return an error, which is
// how the degrade-rather-than-refuse paths get tested.
type Store struct {
	mu sync.Mutex

	Keys map[string]domain.KeyRecord
	// Field is the spent field each key's Spent counts — spent:total, or spent:<YYYY-MM> — and
	// Shelved what the key holds under its other fields: the memory twin of the real store keeping
	// one field per window and month and reading the one the window names now. A key not listed
	// counts the field its window names.
	Field    map[string]string
	Shelved  map[string]map[string]int64
	Groups   map[string]domain.GroupRecord
	Teams    map[string]domain.TeamRecord
	Routes   map[string][]domain.Route
	Sticky   map[string]string
	InFlight map[string]map[string]bool // engine → request ids
	Failures map[string]int
	// KeyStats is what each vendor credential answered, the memory twin of pk:<id>.
	KeyStats map[string]domain.KeyStats
	// Usage is every live counter, bucket ("<key prefix>:<YYYY-MM-DD>", repository.UsageBucket) →
	// counters: the memory twin of usage:<bucket>.
	Usage map[string]map[string]int64
	// Limits is every limit counter, "<key prefix>:<metric>:<window>:<bucket>" → count: the memory
	// twin of lim:….
	Limits map[string]int64
	Hashes map[string]string // grove:state_hash — section/bucket → hash

	// Drained is every counter set aside, drain id → bucket → counters; Unacked the "<id>:<bucket>"
	// pairs not yet acknowledged; Retained how long each acknowledged pair is kept. Adjusted is
	// every spend adjustment id already applied.
	Drained map[string]map[string]map[string]int64
	Unacked map[string]bool
	// Stamps is the team charged for a set-aside pair, "<id>:<bucket>" → team, only for pairs a
	// hand-over set aside: the memory twin of the `team` field handOverScript writes.
	Stamps map[string]string
	// Accrued is the request ids a replay has landed, the memory twin of accrued:<id>.
	Accrued  map[string]bool
	Retained map[string]time.Duration
	Adjusted map[string]bool

	// Fail names the repositories that should error, by interface name ("routes", "inflight",
	// "health", "sessions", "keys", "groups", "usage", "limits", "state", "providerkeys").
	Fail map[string]bool
}

var errStore = errors.New("store unavailable")

func New() *Store {
	return &Store{
		Keys: map[string]domain.KeyRecord{}, Field: map[string]string{}, Shelved: map[string]map[string]int64{},
		Groups: map[string]domain.GroupRecord{},
		Teams:  map[string]domain.TeamRecord{}, Routes: map[string][]domain.Route{},
		Sticky: map[string]string{}, InFlight: map[string]map[string]bool{},
		Failures: map[string]int{}, KeyStats: map[string]domain.KeyStats{}, Usage: map[string]map[string]int64{},
		Limits: map[string]int64{}, Hashes: map[string]string{}, Fail: map[string]bool{},
		Drained: map[string]map[string]map[string]int64{}, Unacked: map[string]bool{}, Stamps: map[string]string{},
		Accrued: map[string]bool{}, Retained: map[string]time.Duration{}, Adjusted: map[string]bool{},
	}
}

// Repositories hands this store out behind the interfaces.
func (s *Store) Repositories() repository.Store {
	return repository.Store{
		Keys: keys{s}, Groups: groups{s}, Routes: routes{s},
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

// putKey matches the real store's HSET-merge: a push writes its fields and leaves Spent alone.
func (s *Store) putKey(rec repository.KeyUpsert) {
	limits, _ := domain.ParseLimits(rec.Limits) // the push handler already refused one that does not read
	s.Keys[rec.MeterID] = domain.KeyRecord{
		Status: rec.Status, Team: rec.Team, KeyPrefix: rec.Prefix, Groups: domain.ModelSet(rec.Groups),
		Allow: domain.ModelSet(rec.Allow), Deny: domain.ModelSet(rec.Deny),
		Limited: rec.Limited, LogPayloads: rec.LogPayloads, Geography: rec.Geography,
		Budget: rec.Budget, Spent: s.Keys[rec.MeterID].Spent, BudgetWindow: rec.BudgetWindow,
		Limits: limits,
	}
}

func (s *Store) putTeam(rec repository.TeamUpsert) {
	limits, _ := domain.ParseLimits(rec.Limits) // the push handler already refused one that does not read
	s.Teams[rec.Team] = domain.TeamRecord{
		Limited: rec.Limited, LogPayloads: rec.LogPayloads, Limits: limits,
	}
}

// rollField matches the real store reading the field the key's window names now: a month key in
// a new month, or a key whose window a push moved, shelves what it counted and picks that field
// up — 0 unless it was written, so a total key spent for good reads 0 on the month and its total
// again when moved back. Called with the lock held, on a key the store knows.
func (s *Store) rollField(id string) {
	rec := s.Keys[id]
	want := spentFieldFor(rec.BudgetWindow, time.Now())
	if have, ok := s.Field[id]; ok && have != want {
		if s.Shelved[id] == nil {
			s.Shelved[id] = map[string]int64{}
		}
		s.Shelved[id][have] = rec.Spent
		rec.Spent = s.Shelved[id][want]
		delete(s.Shelved[id], want)
		s.Keys[id] = rec
	}
	s.Field[id] = want
}

func spentFieldFor(window string, now time.Time) string {
	if window == domain.WindowTotal {
		return "spent:total"
	}
	return "spent:" + domain.MonthOf(now)
}

type keys struct{ s *Store }

// Resolve matches the real script: a group is asked for only when the key lists one, so a failing
// groups store is felt only by a key that reaches it.
func (k keys) Resolve(_ context.Context, meterID string) (repository.Holder, bool, error) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	if err := k.s.failed("keys"); err != nil {
		return repository.Holder{}, false, err
	}
	if _, ok := k.s.Keys[meterID]; !ok {
		return repository.Holder{}, false, nil
	}
	k.s.rollField(meterID)
	rec := k.s.Keys[meterID]
	holder := repository.Holder{Key: rec, Groups: map[string]domain.GroupRecord{}}
	if team, ok := k.s.Teams[rec.Team]; ok && rec.Team != "" {
		holder.Team = &team
	}
	if len(rec.Groups) > 0 {
		if err := k.s.failed("groups"); err != nil {
			return repository.Holder{}, false, err
		}
	}
	for name := range rec.Groups {
		holder.Groups[name] = k.s.Groups[name]
	}
	return holder, true, nil
}

// Upsert matches the real one: a key held under another team has its live buckets set aside,
// stamped with that team, before the record is written.
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
		if held := k.s.Keys[rec.MeterID].Team; held != "" && held != rec.Team {
			k.s.closeBooks(rec.Prefix, held)
		}
		k.s.putKey(rec)
	}
	return nil
}

// closeBooks sets every live bucket of prefix aside under a fresh drain id, stamped with team.
// Caller holds mu.
func (s *Store) closeBooks(prefix, team string) {
	id := repository.NewDrainID(time.Now())
	for bucket := range s.Usage {
		if p, _ := repository.SplitBucket(bucket); p == prefix {
			s.setAside(id, bucket)
			s.Stamps[id+":"+bucket] = team
		}
	}
}

// setAside moves one live bucket under id and marks the pair unacknowledged. Caller holds mu.
func (s *Store) setAside(id, bucket string) {
	if s.Drained[id] == nil {
		s.Drained[id] = map[string]map[string]int64{}
	}
	s.Drained[id][bucket] = s.Usage[bucket]
	s.Unacked[id+":"+bucket] = true
	delete(s.Usage, bucket)
}

func (k keys) Delete(_ context.Context, ids []string) (int, error) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	return deleteFrom(ids, func(id string) { delete(k.s.Keys, id) }), k.s.failed("keys")
}

// Spent matches the real one: what each key the store holds counts under its window, nothing for
// one never pushed.
func (k keys) Spent(_ context.Context, ids []string) (map[string]int64, error) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	if err := k.s.failed("keys"); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, id := range ids {
		if _, ok := k.s.Keys[id]; ok {
			k.s.rollField(id)
			out[id] = k.s.Keys[id].Spent
		}
	}
	return out, nil
}

// AdjustSpent matches the real one: once per id, and only on a key the store knows.
func (k keys) AdjustSpent(_ context.Context, meterID, id string, delta int64) (int64, bool, bool, error) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	if err := k.s.failed("keys"); err != nil {
		return 0, false, false, err
	}
	if _, found := k.s.Keys[meterID]; !found {
		return 0, false, false, nil
	}
	k.s.rollField(meterID)
	rec := k.s.Keys[meterID]
	if k.s.Adjusted[id] {
		return rec.Spent, false, true, nil
	}
	rec.Spent += delta
	k.s.Keys[meterID] = rec
	k.s.Adjusted[id] = true
	return rec.Spent, true, true, nil
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
	case "unauthorized":
		stats.Unauthorized, stats.Rejected = stats.Unauthorized+1, stats.Rejected+1
	case "unpaid":
		stats.Unpaid, stats.Rejected = stats.Unpaid+1, stats.Rejected+1
	case "forbidden":
		stats.Forbidden, stats.Rejected = stats.Forbidden+1, stats.Rejected+1
	case "ok":
		stats.OK++
	default:
		stats.Failed++
	}
	p.s.KeyStats[id] = stats
	return nil
}

func (p providerKeys) Take(_ context.Context, ids []string) (map[string]domain.KeyStats, error) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if err := p.s.failed("providerkeys"); err != nil {
		return nil, err
	}
	stats := make(map[string]domain.KeyStats, len(ids))
	for _, id := range ids {
		stats[id] = p.s.KeyStats[id]
		delete(p.s.KeyStats, id)
	}
	return stats, nil
}

type limits struct{ s *Store }

func limitKey(key string, limit domain.Limit, now time.Time) string {
	label, _, _ := limit.Bucket(now)
	return key + ":" + limit.Metric + ":" + limit.Window + ":" + label
}

// Admit matches the real script: every limit is checked before any is counted, under one lock.
func (l limits) Admit(_ context.Context, key string, lims []domain.Limit, now time.Time) ([]domain.Limit, error) {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if err := l.s.failed("limits"); err != nil {
		return nil, err
	}
	var exceeded []domain.Limit
	for _, limit := range lims {
		if l.s.Limits[limitKey(key, limit, now)] >= limit.Value {
			exceeded = append(exceeded, limit)
		}
	}
	if len(exceeded) > 0 {
		return exceeded, nil
	}
	for _, limit := range lims {
		if limit.Metric == domain.LimitRequests {
			l.s.Limits[limitKey(key, limit, now)]++
		}
	}
	return nil, nil
}

func (l limits) Debit(_ context.Context, key string, lims []domain.Limit, tokens int64, now time.Time) error {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if err := l.s.failed("limits"); err != nil {
		return err
	}
	for _, limit := range lims {
		l.s.Limits[limitKey(key, limit, now)] += tokens
	}
	return nil
}

// UsageOf is the live counters prefix has today: what a test reads back after an Accrue.
func (s *Store) UsageOf(prefix string) map[string]int64 {
	return s.Usage[repository.UsageBucket(prefix, time.Now())]
}

type usage struct{ s *Store }

// Accrue matches the real one: counters, cost and the key's spend land under one lock, and the
// spend moves only on a key the store knows.
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
	bucket := repository.UsageBucket(a.Prefix, time.Now())
	if u.s.Usage[bucket] == nil {
		u.s.Usage[bucket] = map[string]int64{}
	}
	for field, n := range a.Fields {
		u.s.Usage[bucket][field] += n
	}
	if _, known := u.s.Keys[a.Key]; a.Key == "" || !known {
		return
	}
	u.s.rollField(a.Key)
	rec := u.s.Keys[a.Key]
	rec.Spent += a.Cost
	u.s.Keys[a.Key] = rec
	u.s.Usage[bucket]["key_spent"] = rec.Spent
	u.s.Usage[bucket]["key_balance"] = a.Budget - rec.Spent
}

// Drain matches the real one: live counters (all, or only keys' buckets) are set aside under
// newID, and every unacknowledged pair is answered. Nothing is deleted until Ack.
func (u usage) Drain(_ context.Context, newID string, keys []string) (repository.Drains, error) {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	if err := u.s.failed("usage"); err != nil {
		return nil, err
	}
	want := wantedBuckets(keys)
	for bucket := range u.s.Usage {
		if want(bucket) {
			u.s.setAside(newID, bucket)
		}
	}
	return u.unacked(want), nil
}

// unacked is every set-aside pair not yet acknowledged, as the wire carries it. Caller holds mu.
func (u usage) unacked(want func(string) bool) repository.Drains {
	out := repository.Drains{}
	for member := range u.s.Unacked {
		id, bucket, _ := strings.Cut(member, ":")
		if !want(bucket) {
			continue
		}
		prefix, day := repository.SplitBucket(bucket)
		snapshot := counters(u.s.Drained[id][bucket])
		if team, stamped := u.s.Stamps[member]; stamped {
			snapshot["team"] = team
		}
		out.Put(id, prefix, day, snapshot)
	}
	return out
}

// wantedBuckets says whether a bucket is one of keys' — every bucket when none are named.
func wantedBuckets(keys []string) func(bucket string) bool {
	wanted := map[string]bool{}
	for _, key := range keys {
		wanted[key] = true
	}
	return func(bucket string) bool {
		prefix, _ := repository.SplitBucket(bucket)
		return len(wanted) == 0 || wanted[prefix]
	}
}

func counters(fields map[string]int64) map[string]string {
	snapshot := map[string]string{}
	for field, n := range fields {
		snapshot[field] = strconv.FormatInt(n, 10)
	}
	return snapshot
}

// Ack matches the real one: only unacknowledged pairs move anything.
func (u usage) Ack(_ context.Context, acks repository.Acks, retention time.Duration) (int, error) {
	u.s.mu.Lock()
	defer u.s.mu.Unlock()
	if err := u.s.failed("usage"); err != nil {
		return 0, err
	}
	count := 0
	for id, keys := range acks {
		for prefix, days := range keys {
			for _, day := range days {
				if member := id + ":" + repository.Bucket(prefix, day); u.s.Unacked[member] {
					delete(u.s.Unacked, member)
					u.s.Retained[member] = retention
					count++
				}
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
	if push.Teams != nil {
		named := map[string]bool{}
		for label, bucket := range push.Teams {
			for _, rec := range bucket.Records {
				if rec.Team == "" {
					continue
				}
				named[rec.Team] = true
				counts.Teams++
				st.s.putTeam(rec)
			}
			st.setBucketHash("teams:"+label, bucket.Hash, len(bucket.Records))
		}
		for id := range st.s.Teams {
			if _, pushed := push.Teams[domain.BucketOf(id)]; pushed && !named[id] {
				delete(st.s.Teams, id)
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
				st.s.putKey(rec)
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
