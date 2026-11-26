package http

import (
	"encoding/json"
	goflag "flag"
	"fmt"
	"maps"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frappe/pathway/internal/config"
	"github.com/frappe/pathway/internal/domain"
	"github.com/frappe/pathway/internal/repository"
	"github.com/frappe/pathway/internal/repository/memory"
)

var (
	limitSeeds = goflag.Int("seeds", 20, "how many random limit sequences to check against the model")
	limitSeed  = goflag.Int64("seed", 0, "replay one random limit sequence")
)

// stack is the whole gateway over one store, seeded the way the control plane seeds it: through
// the admin push. The clock the limit windows are read off is the test's.
type stack struct {
	t     *testing.T
	data  http.Handler
	admin http.Handler
	clock *time.Time
	// pushes numbers the hashes, so no two pushes carry the same one.
	pushes int
}

func newStack(t *testing.T, repos repository.Store, clock *time.Time) *stack {
	t.Helper()
	now := func() time.Time { return *clock }
	server := newServerOver(t, repos, config.Config{}, 0, func(s *Services) {
		s.Admission.Now, s.Metering.Now = now, now
	})
	data, err := server.DataHandler(nil)
	if err != nil {
		t.Fatalf("DataHandler: %v", err)
	}
	return &stack{t: t, data: data, admin: server.AdminHandler(), clock: clock}
}

// engines serves each model from a fake engine of its own. → the routes section naming them, the
// group granting them all, and how often each was dialled.
func (s *stack) engines(handlers map[string]http.HandlerFunc, pricing *domain.Pricing) (map[string]any, map[string]*atomic.Int64) {
	s.t.Helper()
	table, dials, names := map[string][]domain.Route{}, map[string]*atomic.Int64{}, []string{}
	for model, handler := range handlers {
		count := &atomic.Int64{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count.Add(1)
			handler(w, r)
		}))
		s.t.Cleanup(server.Close)
		dials[model] = count
		names = append(names, model)
		table[model] = []domain.Route{{
			EngineURL: server.URL, InternalKey: "engine-key", Healthy: true,
			Deployment: "MD-" + model, Server: "INF-" + model, Kind: "direct", Pricing: pricing,
		}}
	}
	sections := map[string]any{
		"routes": map[string]any{"hash": s.hash(), "table": table},
		"groups": map[string]any{"hash": s.hash(), "records": []map[string]any{
			{"name": "all", "models": strings.Join(names, ",")},
		}},
	}
	return sections, dials
}

func (s *stack) hash() string {
	s.pushes++
	return "h" + strconv.Itoa(s.pushes)
}

// buckets is a bucketed section over records, each in the bucket its id names.
func (s *stack) buckets(idField string, records ...map[string]any) map[string]any {
	buckets := map[string]any{}
	grouped := map[string][]map[string]any{}
	for _, record := range records {
		label := domain.BucketOf(record[idField].(string))
		grouped[label] = append(grouped[label], record)
	}
	for label, rows := range grouped {
		buckets[label] = map[string]any{"hash": s.hash(), "records": rows}
	}
	return map[string]any{"buckets": buckets}
}

// seed pushes a whole world at once: each model on an engine of its own, the teams and the keys.
// No teams is a push without the section, as a control plane from before team records sends.
func (s *stack) seed(handlers map[string]http.HandlerFunc, pricing *domain.Pricing, teams, keys []map[string]any) map[string]*atomic.Int64 {
	s.t.Helper()
	sections, dials := s.engines(handlers, pricing)
	if teams != nil {
		sections["teams"] = s.buckets("team", teams...)
	}
	sections["keys"] = s.buckets("key_hash", keys...)
	s.push(sections)
	return dials
}

// push posts the sections as one state push, which must land.
func (s *stack) push(sections map[string]any) {
	s.t.Helper()
	body, err := json.Marshal(sections)
	if err != nil {
		s.t.Fatal(err)
	}
	if w := adminCall(s.t, s.admin, http.MethodPost, "/grove-admin/state", string(body)); w.Code != http.StatusOK {
		s.t.Fatalf("push = %d: %s", w.Code, w.Body)
	}
}

// ask sends one chat request for model under secret, naming any fallbacks.
func (s *stack) ask(secret, model string, fallbacks ...string) *httptest.ResponseRecorder {
	body := map[string]any{"model": model, "messages": []any{}}
	if len(fallbacks) > 0 {
		body["fallbacks"] = fallbacks
	}
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(string(raw)))
	r.Header.Set("Authorization", "Bearer "+secret)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.data.ServeHTTP(w, r)
	return w
}

// pushedKey is a key record as the control plane pushes it now: its team's verdicts and limits
// ride the team's record, so extra carries only what a test adds.
func pushedKey(secret, prefix, team string, extra map[string]any) map[string]any {
	record := map[string]any{
		"key_hash": domain.SHA256Hex(secret), "prefix": prefix, "team": team, "status": "active",
		"group": "all", "budget": 1_000_000_000_000,
	}
	maps.Copy(record, extra)
	return record
}

func pushedTeam(name string, extra map[string]any) map[string]any {
	record := map[string]any{"team": name, "limited": false, "log_payloads": false, "limits": "", "cap_message": ""}
	maps.Copy(record, extra)
	return record
}

// wireLimits is rows as a push carries them: metric:window:value[:model], comma-joined.
func wireLimits(rows []domain.Limit) string {
	entries := make([]string, 0, len(rows))
	for _, row := range rows {
		entry := fmt.Sprintf("%s:%s:%d", row.Metric, row.Window, row.Value)
		if row.Model != "" {
			entry += ":" + row.Model
		}
		entries = append(entries, entry)
	}
	return strings.Join(entries, ",")
}

// limitModel is the limit rules written out a second time, on purpose not through domain: one
// count per holder, model, metric and window. A request is refused while any row that rules it is
// at its ceiling, and then counts nothing; an admitted one counts on every row that rules it.
type limitModel struct{ counts map[string]int64 }

type modelVerdict struct {
	admitted   bool
	retryAfter int
	// messages is every refusal the gateway may answer with: the rows that reset last, tied.
	messages map[string]bool
}

type ruledRow struct {
	kind, id string
	row      domain.Limit
}

// ruling is the rows that hold on model: a row naming it, else the blank row of that metric and window.
func ruling(rows []domain.Limit, model string) []domain.Limit {
	blank, named := map[string]domain.Limit{}, map[string]domain.Limit{}
	for _, row := range rows {
		slot := row.Metric + " " + row.Window
		switch row.Model {
		case "":
			blank[slot] = row
		case model:
			named[slot] = row
		}
	}
	held := []domain.Limit{}
	for slot, row := range blank {
		if _, replaced := named[slot]; !replaced {
			held = append(held, row)
		}
	}
	for _, row := range named {
		held = append(held, row)
	}
	return held
}

// windowOf is the calendar window now falls in, by its start, and the seconds until it ends.
func windowOf(window string, now time.Time) (string, int) {
	now = now.UTC()
	year, month, day := now.Date()
	var start, end time.Time
	switch window {
	case "1m":
		start = now.Truncate(time.Minute)
		end = start.Add(time.Minute)
	case "1h":
		start = now.Truncate(time.Hour)
		end = start.Add(time.Hour)
	case "1d":
		start = time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 0, 1)
	case "1M":
		start = time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
		end = start.AddDate(0, 1, 0)
	}
	return start.Format(time.RFC3339), int(end.Sub(now) / time.Second)
}

func (m *limitModel) ask(team string, teamRows []domain.Limit, key string, keyRows []domain.Limit, model string, now time.Time, tokens int64) modelVerdict {
	var rows []ruledRow
	for _, row := range ruling(teamRows, model) {
		rows = append(rows, ruledRow{"team", team, row})
	}
	for _, row := range ruling(keyRows, model) {
		rows = append(rows, ruledRow{"key", key, row})
	}
	counter := func(r ruledRow) string {
		start, _ := windowOf(r.row.Window, now)
		return strings.Join([]string{r.kind, r.id, model, r.row.Metric, r.row.Window, start}, "|")
	}
	verdict := modelVerdict{messages: map[string]bool{}}
	for _, r := range rows {
		if m.counts[counter(r)] < r.row.Value {
			continue
		}
		_, wait := windowOf(r.row.Window, now)
		if wait > verdict.retryAfter {
			verdict.retryAfter, verdict.messages = wait, map[string]bool{}
		}
		if wait == verdict.retryAfter {
			unit := "requests"
			if r.row.Metric == domain.LimitTokens {
				unit = "tokens"
			}
			verdict.messages[fmt.Sprintf("rate limit exceeded: %d %s per %s on %s (%s)", r.row.Value, unit, r.row.Window, model, r.kind)] = true
		}
	}
	if len(verdict.messages) > 0 {
		return verdict
	}
	for _, r := range rows {
		if r.row.Metric == domain.LimitTokens {
			m.counts[counter(r)] += tokens
		} else {
			m.counts[counter(r)]++
		}
	}
	return modelVerdict{admitted: true}
}

// randomRows is up to four rows, no two on one model, metric and window.
func randomRows(rng *rand.Rand, models []string) []domain.Limit {
	var rows []domain.Limit
	taken := map[string]bool{}
	for range rng.Intn(5) {
		row := domain.Limit{
			Metric: []string{domain.LimitRequests, domain.LimitTokens}[rng.Intn(2)],
			Window: []string{"1m", "1h", "1d", "1M"}[rng.Intn(4)],
			Model:  append([]string{"", ""}, models...)[rng.Intn(2+len(models))],
		}
		row.Value = int64(1 + rng.Intn(5))
		if row.Metric == domain.LimitTokens {
			row.Value = []int64{120, 240, 300, 600}[rng.Intn(4)]
		}
		if slot := row.Model + row.Metric + row.Window; !taken[slot] {
			taken[slot] = true
			rows = append(rows, row)
		}
	}
	return rows
}

type sequenceKey struct {
	secret, prefix, team string
	rows                 []domain.Limit
}

// runLimitSequence drives one random world of teams, keys, models and limit rows through the real
// chain over repos, moving the clock as it goes, and holds every answer against the model.
func runLimitSequence(t *testing.T, repos repository.Store, seed int64, steps int) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	clock := time.Date(2026, 10, 4, 12, 0, 10, 0, time.UTC)
	s := newStack(t, repos, &clock)
	models := []string{"m1", "m2", "m3"}
	answer := jsonEngine(`{` + usageObject + `}`)

	teamRows := map[string][]domain.Limit{"T-1": randomRows(rng, models), "T-2": randomRows(rng, models)}
	var teams, pushed []map[string]any
	for name, rows := range teamRows {
		teams = append(teams, pushedTeam(name, map[string]any{"limits": wireLimits(rows)}))
	}
	var keys []sequenceKey
	for i, team := range []string{"T-1", "T-1", "T-2"} {
		key := sequenceKey{fmt.Sprintf("secret-%d", i), fmt.Sprintf("K-%d", i), team, randomRows(rng, models)}
		keys = append(keys, key)
		pushed = append(pushed, pushedKey(key.secret, key.prefix, key.team, map[string]any{"key_limits": wireLimits(key.rows)}))
	}
	s.seed(map[string]http.HandlerFunc{"m1": answer, "m2": answer, "m3": answer}, nil, teams, pushed)

	model := limitModel{counts: map[string]int64{}}
	jumps := []time.Duration{time.Second, 7 * time.Second, 50 * time.Second, 61 * time.Second, time.Hour, 25 * time.Hour, 31 * 24 * time.Hour}
	for step := range steps {
		if rng.Intn(4) == 0 {
			clock = clock.Add(jumps[rng.Intn(len(jumps))])
			continue
		}
		key, asked := keys[rng.Intn(len(keys))], models[rng.Intn(len(models))]
		want := model.ask(key.team, teamRows[key.team], key.prefix, key.rows, asked, clock, 120)
		got := s.ask(key.secret, asked)
		where := fmt.Sprintf("seed %d step %d: %s on %s at %s (team rows %q, own %q)",
			seed, step, key.prefix, asked, clock.Format(time.RFC3339), wireLimits(teamRows[key.team]), wireLimits(key.rows))
		if want.admitted {
			if got.Code != http.StatusOK {
				t.Fatalf("%s: got %d %s, want admitted", where, got.Code, got.Body)
			}
			continue
		}
		message, _ := refusal(t, got.Body.Bytes())
		if got.Code != http.StatusTooManyRequests || !want.messages[message] || got.Header().Get("Retry-After") != strconv.Itoa(want.retryAfter) {
			t.Fatalf("%s: got %d %q Retry-After %q, want 429 %v Retry-After %d",
				where, got.Code, message, got.Header().Get("Retry-After"), want.messages, want.retryAfter)
		}
		// A wait past a minute is not one for a client's own retry to sit out.
		if told := got.Header().Get("X-Should-Retry") == "false"; told != (want.retryAfter > 60) {
			t.Fatalf("%s: X-Should-Retry %q on a wait of %ds", where, got.Header().Get("X-Should-Retry"), want.retryAfter)
		}
	}
}

// limitSeedsToRun is the one seed asked for, else the first -seeds of them.
func limitSeedsToRun() []int64 {
	if *limitSeed != 0 {
		return []int64{*limitSeed}
	}
	seeds := make([]int64, *limitSeeds)
	for i := range seeds {
		seeds[i] = int64(i + 1)
	}
	return seeds
}

// Random teams, keys, models and limit rows, three hundred requests and clock jumps each: every
// answer of the chain is the one the rules, written out a second time, give. -seed replays one.
func TestLimitsAgreeWithTheModel(t *testing.T) {
	for _, seed := range limitSeedsToRun() {
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) {
			runLimitSequence(t, memory.New().Repositories(), seed, 300)
		})
	}
}
