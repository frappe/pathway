package routing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository/memory"
)

// The pick rule itself is domain.PickRoute and is covered there. What is only true at this layer is
// the plumbing around it: which sticky key is read and written, when the synthetic session applies,
// that a slot is always claimed, and that an unreadable counter degrades instead of refusing.

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func engine(url string) domain.Route {
	return domain.Route{EngineURL: url, Healthy: true, Deployment: "MD-" + url}
}

func serviceOver(store *memory.Store, opts Options) *Service {
	return New(store.Repositories(), quiet(), opts)
}

// fixedTTL is the synthetic-session knob pinned for a test. In production it is read per pick, so
// turning it is an edit to the tunables file and a signal.
func fixedTTL(d time.Duration) func() time.Duration { return func() time.Duration { return d } }

func twoEngines() *memory.Store {
	store := memory.New()
	store.Routes["qwen3-4b"] = []domain.Route{engine("https://a"), engine("https://b")}
	return store
}

func pick(t *testing.T, svc *Service, session string) Decision {
	t.Helper()
	decision, err := svc.Pick(context.Background(), Request{
		Model: "qwen3-4b", Session: session, MeterID: "meter", KeyPrefix: "abc123",
	})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	return decision
}

// The default. Two idle engines and a caller that names no session must not land on the same box
// every time — that is the bug the whole least-in-flight path exists to fix.
func TestAKeylessCallerIsBalanced(t *testing.T) {
	store := twoEngines()
	svc := serviceOver(store, Options{})

	first := pick(t, svc, "")
	second := pick(t, svc, "")
	if first.EngineURL() == second.EngineURL() {
		t.Fatalf("both requests went to %s — the claim did not break the tie", first.EngineURL())
	}
}

// A caller that names a session keeps its engine, so its prefix cache stays warm.
func TestANamedSessionKeepsItsEngine(t *testing.T) {
	svc := serviceOver(twoEngines(), Options{})

	first := pick(t, svc, "acme-bot")
	for i := 0; i < 5; i++ {
		if got := pick(t, svc, "acme-bot").EngineURL(); got != first.EngineURL() {
			t.Fatalf("request %d moved to %s, want %s", i, got, first.EngineURL())
		}
	}
}

// The rollback lever. With it set, a whole key is pinned to one engine — what a single-placement
// fleet always did — even though the caller named no session of its own.
func TestTheSyntheticSessionPinsAKeylessCaller(t *testing.T) {
	store := twoEngines()
	svc := serviceOver(store, Options{SyntheticTTL: fixedTTL(StickyTTL)})

	first := pick(t, svc, "")
	for i := 0; i < 5; i++ {
		if got := pick(t, svc, "").EngineURL(); got != first.EngineURL() {
			t.Fatalf("request %d moved to %s — the synthetic session did not hold", i, got)
		}
	}
}

// Every admitted request claims a slot, and that claim is what the next pick balances against.
func TestPickClaimsASlotAndReleaseGivesItBack(t *testing.T) {
	store := twoEngines()
	svc := serviceOver(store, Options{})

	decision := pick(t, svc, "acme-bot")
	if got := len(store.InFlight[decision.EngineURL()]); got != 1 {
		t.Fatalf("in-flight = %d, want 1", got)
	}
	svc.Release(context.Background(), decision.Route, decision.RequestID)
	if got := len(store.InFlight[decision.EngineURL()]); got != 0 {
		t.Errorf("in-flight = %d after release, want 0 — the engine stays out of rotation", got)
	}
}

// A vendor row has no capacity of ours and nothing to balance against, so it costs no in-flight
// call: with that store failing, the pick and the release never reach it.
func TestAVendorRowHoldsNoSlot(t *testing.T) {
	store := memory.New()
	vendor := engine("https://vendor")
	vendor.Kind, vendor.Dialect = "provider", domain.DialectOpenAI
	store.Routes["qwen3-4b"] = []domain.Route{vendor}
	store.Fail["inflight"] = true
	logs := &strings.Builder{}
	svc := New(store.Repositories(), slog.New(slog.NewTextHandler(logs, nil)), Options{})

	decision, err := svc.Pick(context.Background(), Request{
		Model: "qwen3-4b", RequestID: "rid", Dialect: domain.DialectOpenAI, Path: "/v1/chat/completions",
	})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	svc.Release(context.Background(), decision.Route, decision.RequestID)
	if strings.Contains(logs.String(), "in-flight") {
		t.Errorf("a vendor row reached the in-flight store:\n%s", logs)
	}
}

// Stickiness loses to capacity: a warm prefix cache is not worth queueing behind a full engine
// when a replica is idle.
func TestAFullStickyEngineIsAbandoned(t *testing.T) {
	store := memory.New()
	full, idle := engine("https://full"), engine("https://idle")
	full.Capacity = 1
	store.Routes["qwen3-4b"] = []domain.Route{full, idle}
	store.Sticky["acme-bot"] = "https://full"
	store.InFlight["https://full"] = map[string]bool{"someone-else": true}

	svc := serviceOver(store, Options{})
	if got := pick(t, svc, "acme-bot").EngineURL(); got != "https://idle" {
		t.Errorf("picked %s, want the idle replica", got)
	}
}

// A target that has failed EjectAfter times in a row stops being chosen, from live traffic alone.
func TestAnEjectedTargetIsNotChosen(t *testing.T) {
	store := twoEngines()
	store.Failures["https://a"] = domain.EjectAfter

	svc := serviceOver(store, Options{})
	for i := 0; i < 5; i++ {
		if got := pick(t, svc, "").EngineURL(); got != "https://b" {
			t.Fatalf("picked the ejected target %s", got)
		}
	}
}

// Ejection is an optimisation on top of a table that is already correct. An unreadable health
// counter must not take a working engine out — that would turn one broken store into an outage.
func TestAnUnreadableHealthCounterStillRoutes(t *testing.T) {
	store := twoEngines()
	store.Fail["health"] = true

	if got := pick(t, serviceOver(store, Options{}), "").EngineURL(); got == "" {
		t.Error("a health-store failure refused a request the route table could serve")
	}
}

// Same rule for the in-flight counts: falling back to the first healthy route beats 503ing a
// working engine over a counter.
func TestUnreadableInFlightCountsStillRoute(t *testing.T) {
	store := twoEngines()
	store.Fail["inflight"] = true

	if got := pick(t, serviceOver(store, Options{}), "").EngineURL(); got == "" {
		t.Error("an in-flight-store failure refused a request the route table could serve")
	}
}

func TestPickRefusals(t *testing.T) {
	full := engine("https://full")
	full.Capacity = 1

	for _, c := range []struct {
		name   string
		table  []domain.Route
		busy   map[string]bool
		want   int
		reason string
	}{
		{"a model with no placements", nil, nil, 503, "model unavailable"},
		{"every placement unhealthy",
			[]domain.Route{{EngineURL: "https://a"}}, nil, 503, "no healthy server for model"},
		{"every replica at capacity",
			[]domain.Route{full}, map[string]bool{"other": true}, 429, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := memory.New()
			if c.table != nil {
				store.Routes["qwen3-4b"] = c.table
			}
			if c.busy != nil {
				store.InFlight["https://full"] = c.busy
			}
			_, err := serviceOver(store, Options{}).Pick(
				context.Background(), Request{Model: "qwen3-4b", MeterID: "meter"})

			var denial domain.Denial
			if !errors.As(err, &denial) {
				t.Fatalf("err = %v, want a Denial", err)
			}
			if denial.Status != c.want {
				t.Errorf("status = %d (%s), want %d", denial.Status, denial.Reason, c.want)
			}
		})
	}
}

// CanServe is what the DNS tier in front of this box reads. It has to distinguish three states an
// open socket cannot: this box is broken, this box is fine, and nothing is deployed anywhere.
func TestCanServe(t *testing.T) {
	ejected := twoEngines()
	ejected.Failures["https://a"] = domain.EjectAfter
	ejected.Failures["https://b"] = domain.EjectAfter

	unhealthy := memory.New()
	unhealthy.Routes["qwen3-4b"] = []domain.Route{{EngineURL: "https://a"}}

	// Capacity is not health: a box turning traffic away is working, and pulling it out of DNS would
	// move that load to a region that has to serve it over a long hop.
	full := engine("https://full")
	full.Capacity = 1
	atCapacity := memory.New()
	atCapacity.Routes["qwen3-4b"] = []domain.Route{full}
	atCapacity.InFlight["https://full"] = map[string]bool{"someone": true}

	oneGood := twoEngines()
	oneGood.Failures["https://a"] = domain.EjectAfter

	broken := twoEngines()
	broken.Fail["routes"] = true

	for _, c := range []struct {
		name  string
		store *memory.Store
		want  error
	}{
		{"an engine to serve", twoEngines(), nil},
		{"one of two ejected", oneGood, nil},
		{"at capacity but healthy", atCapacity, nil},
		{"nothing deployed at all", memory.New(), nil},
		{"every target ejected", ejected, ErrNoEngine},
		{"every placement unhealthy", unhealthy, ErrNoEngine},
		{"the store is unreadable", broken, ErrStoreUnreachable},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := serviceOver(c.store, Options{}).CanServe(context.Background())
			if !errors.Is(got, c.want) {
				t.Errorf("CanServe = %v, want %v", got, c.want)
			}
		})
	}
}

// A caller-chosen session can name the tenant. Only its hash crosses to the infra plane, and only
// for a route that actually hands off.
func TestOnlyAnIngressRouteCarriesASessionKey(t *testing.T) {
	store := memory.New()
	ingress := engine("https://ingress")
	ingress.Kind = "ingress"
	store.Routes["qwen3-4b"] = []domain.Route{ingress}

	decision := pick(t, serviceOver(store, Options{}), "acme-support-bot")
	if decision.SessionKey != domain.SHA256Hex("acme-support-bot") {
		t.Errorf("session key = %q, want the hash", decision.SessionKey)
	}
	if decision.SessionKey == "acme-support-bot" {
		t.Error("the caller's own session string reached the infra plane")
	}

	direct := pick(t, serviceOver(twoEngines(), Options{}), "acme-support-bot")
	if direct.SessionKey != "" {
		t.Errorf("a direct route carried a session key (%q); vLLM has no use for one", direct.SessionKey)
	}
}

// The ingress tier: same rule, its own sticky key, and no region — every replica is in its own VPC.
func TestPickReplicaPinsOnTheForwardedKey(t *testing.T) {
	store := twoEngines()
	svc := serviceOver(store, Options{})
	const sessionKey = "opaque-hash"

	first, err := svc.PickReplica(context.Background(), "qwen3-4b", sessionKey, "rid-1")
	if err != nil {
		t.Fatalf("PickReplica: %v", err)
	}
	if store.Sticky[sessionKey] != first.EngineURL {
		t.Errorf("pinned %q, want %q", store.Sticky[sessionKey], first.EngineURL)
	}
	second, err := svc.PickReplica(context.Background(), "qwen3-4b", sessionKey, "rid-2")
	if err != nil || second.EngineURL != first.EngineURL {
		t.Errorf("second pick went to %q, want %q", second.EngineURL, first.EngineURL)
	}
}

// The gateway must not eject a whole network over one model that has nowhere to go inside it, so
// this refusal is named rather than left as a bare 503.
func TestAModelWithNoReplicaIsNamedAsSuch(t *testing.T) {
	_, err := serviceOver(memory.New(), Options{}).PickReplica(context.Background(), "gone", "", "rid")

	var denial domain.Denial
	if !errors.As(err, &denial) || denial.Reason != "no-replica" {
		t.Fatalf("err = %v, want a no-replica Denial", err)
	}
}

// A model that does not give what the requested surface asks for is refused before an engine is
// chosen — so nothing is dialled, no slot is claimed, and the meter stage below never runs.
func TestAWrongSurfaceIsRefusedBeforeAnythingIsClaimed(t *testing.T) {
	store := memory.New()
	store.Routes["nemotron-asr"] = []domain.Route{{
		EngineURL: "https://asr", Healthy: true, Deployment: "pod-1", OutputModalities: []string{"transcription"},
	}}
	svc := serviceOver(store, Options{})

	_, err := svc.Pick(context.Background(), Request{
		Model: "nemotron-asr", MeterID: "meter", KeyPrefix: "abc123",
		Path: "/v1/chat/completions",
	})

	var denial domain.Denial
	if !errors.As(err, &denial) {
		t.Fatalf("err = %v, want a denial", err)
	}
	if denial.Status != 404 {
		t.Errorf("status = %d, want 404", denial.Status)
	}
	if len(store.InFlight) != 0 {
		t.Errorf("a refused request claimed a slot: %v", store.InFlight)
	}
}

// The same model on the surface it is for still routes.
func TestTheRightSurfaceStillRoutes(t *testing.T) {
	store := memory.New()
	store.Routes["nemotron-asr"] = []domain.Route{{
		EngineURL: "https://asr", Healthy: true, Deployment: "pod-1", OutputModalities: []string{"transcription"},
	}}
	svc := serviceOver(store, Options{})

	decision, err := svc.Pick(context.Background(), Request{
		Model: "nemotron-asr", MeterID: "meter", KeyPrefix: "abc123",
		Path: "/v1/audio/transcriptions",
	})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if decision.Route.EngineURL != "https://asr" {
		t.Errorf("engine = %q", decision.Route.EngineURL)
	}
}

// A vendor row that denies a tool the request names is refused before anything is claimed, with
// the tool named; an engine of ours beside it takes the request instead; a row that denies nothing
// runs everything.
func TestAVendorRowThatDeniesTheToolNamedIsRefused(t *testing.T) {
	vendor := engine("https://vendor")
	vendor.Kind, vendor.Dialect, vendor.DeniedTools = "provider", domain.DialectOpenAI, []string{"web_search_options", "web_search_preview"}
	search := Request{
		Model: "qwen3-4b", MeterID: "meter", KeyPrefix: "abc123", Dialect: domain.DialectOpenAI,
		Path: "/v1/chat/completions", Tools: []string{"messages", "model", "web_search_options"},
	}
	function := search
	function.Tools = []string{"messages", "model", "tools", "function"}

	store := memory.New()
	store.Routes["qwen3-4b"] = []domain.Route{vendor}
	svc := serviceOver(store, Options{})
	_, err := svc.Pick(context.Background(), search)
	var denial domain.Denial
	if !errors.As(err, &denial) || denial.Status != 400 || denial.Reason != "qwen3-4b does not run web_search_options" {
		t.Fatalf("err = %v, want a 400 naming the tool", err)
	}
	if len(store.InFlight) != 0 {
		t.Errorf("a refused request claimed a slot: %v", store.InFlight)
	}
	if _, err := svc.Pick(context.Background(), function); err != nil {
		t.Errorf("a function tool was refused: %v", err)
	}

	store.Routes["qwen3-4b"] = []domain.Route{vendor, engine("https://ours")}
	decision, err := svc.Pick(context.Background(), search)
	if err != nil || decision.Route.EngineURL != "https://ours" {
		t.Errorf("decision = %+v, %v; want the engine of ours", decision.Route, err)
	}
}

// A route pushed before the control plane said what a model gives carries nothing, and must keep
// serving what it always did.
func TestARouteThatDeclaresNothingIsUnrestricted(t *testing.T) {
	svc := serviceOver(twoEngines(), Options{})

	if _, err := svc.Pick(context.Background(), Request{
		Model: "qwen3-4b", MeterID: "meter", KeyPrefix: "abc123",
		Path: "/v1/audio/transcriptions",
	}); err != nil {
		t.Fatalf("a route that declares nothing refused a request: %v", err)
	}
}

func picks(s *Service, ring []domain.Credential, n int) string {
	var ids []string
	for range n {
		ids = append(ids, s.PickKey(ring).ID)
	}
	return strings.Join(ids, "")
}

func TestPickKeyTakesTurns(t *testing.T) {
	s := &Service{}
	ring := []domain.Credential{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	if got := picks(s, ring, 7); got != "abcabca" {
		t.Errorf("turns = %q", got)
	}
	// The same ids on another model's route are the same vendor: its turn carries on.
	twin := []domain.Credential{{ID: "a", Secret: "x"}, {ID: "b", Secret: "y"}, {ID: "c", Secret: "z"}}
	if got := picks(s, twin, 2); got != "bc" {
		t.Errorf("the twin ring started over: %q", got)
	}
	// Another ring, another cursor.
	if got := picks(s, []domain.Credential{{ID: "p"}, {ID: "q"}}, 3); got != "pqp" {
		t.Errorf("a second ring shared the cursor: %q", got)
	}
	if key := s.PickKey([]domain.Credential{{Secret: "only"}}); key.Secret != "only" {
		t.Errorf("an engine's one key was not picked: %+v", key)
	}
	if key := s.PickKey(nil); key != (domain.Credential{}) {
		t.Errorf("picked %+v from an empty ring", key)
	}
}

func TestNextKeyWalksTheRingPastTheSpent(t *testing.T) {
	ring := []domain.Credential{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	if key := NextKey(ring, ring[0], map[string]bool{"a": true, "b": true}); key.ID != "c" {
		t.Errorf("after a with b spent: %q", key.ID)
	}
	if key := NextKey(ring, ring[2], map[string]bool{"c": true}); key.ID != "a" {
		t.Errorf("after c did not wrap: %q", key.ID)
	}
	if key := NextKey(ring, ring[1], map[string]bool{"a": true, "b": true, "c": true}); key.ID != "" {
		t.Errorf("picked %q from an exhausted ring", key.ID)
	}
}

// A full fleet waits up to capacity_wait for a slot rather than turning the request away at once,
// and gives up early on a client that left.
func TestPickWaitsForRoom(t *testing.T) {
	full := func() *memory.Store {
		route := engine("https://full")
		route.Capacity = 1
		store := memory.New()
		store.Routes["qwen3-4b"] = []domain.Route{route}
		store.InFlight["https://full"] = map[string]bool{"someone": true}
		return store
	}
	request := Request{Model: "qwen3-4b", MeterID: "meter"}

	if _, err := serviceOver(full(), Options{}).Pick(context.Background(), request); err == nil {
		t.Error("no wait configured, and a full engine was picked")
	}

	store := full()
	svc := serviceOver(store, Options{CapacityWait: fixedTTL(time.Second)})
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = store.Repositories().InFlight.Release(context.Background(), "https://full", "someone")
	}()
	if _, err := svc.Pick(context.Background(), request); err != nil {
		t.Errorf("a slot freed during the wait, and the pick still failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if _, err := serviceOver(full(), Options{CapacityWait: fixedTTL(time.Minute)}).Pick(ctx, request); err == nil {
		t.Error("a client that left was given a slot")
	}
	if waited := time.Since(started); waited > time.Second {
		t.Errorf("waited %v for a client that had already left", waited)
	}
}
