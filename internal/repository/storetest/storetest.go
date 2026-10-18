// Package storetest is the contract every store answers to, run against each implementation: memory
// on every `go test`, redis under the live tag. Every service and transport test runs against
// memory, so this is what keeps memory honest — a fake that drifted from redis would leave those
// tests green over a gateway that is not.
package storetest

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/frappe/pathway/internal/domain"
	"github.com/frappe/pathway/internal/repository"
)

// Run runs the contract, one fresh store per case from open.
func Run(t *testing.T, open func(t *testing.T) repository.Store) {
	cases := []struct {
		name string
		run  func(t *testing.T, store repository.Store)
	}{
		{"KeysResolveWhatWasPushed", keysResolveWhatWasPushed},
		{"AdjustSpentAppliesOncePerID", adjustSpentAppliesOncePerID},
		{"AnUnpushedGroupIsTheZeroValue", anUnpushedGroupIsTheZeroValue},
		{"RoutesListModelsSortedAndDelete", routesListModelsSortedAndDelete},
		{"ASessionPinsItsEngine", aSessionPinsItsEngine},
		{"InFlightCountsClaimsInTheOrderAsked", inFlightCountsClaimsInTheOrderAsked},
		{"ASuccessClearsTheFailures", aSuccessClearsTheFailures},
		{"ProviderKeysTakeClears", providerKeysTakeClears},
		{"AdmitTakesTheLastSlotOnce", admitTakesTheLastSlotOnce},
		{"ADrainIsResentUntilAcked", aDrainIsResentUntilAcked},
		{"APeekMovesNothing", aPeekMovesNothing},
		{"AReplayLandsOnce", aReplayLandsOnce},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(t, open(t)) })
	}
}

var ctx = context.Background()

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func keysResolveWhatWasPushed(t *testing.T, store repository.Store) {
	if _, found, err := store.Keys.Resolve(ctx, "never"); err != nil || found {
		t.Fatalf("unpushed key: found=%v err=%v, want not found", found, err)
	}
	must(t, store.Keys.Upsert(ctx, []repository.KeyUpsert{{MeterID: "k1", Prefix: "abc", Status: "active"}}))
	holder, found, err := store.Keys.Resolve(ctx, "k1")
	if err != nil || !found || holder.Key.KeyPrefix != "abc" || holder.Key.Status != "active" {
		t.Fatalf("pushed key: %+v found=%v err=%v", holder.Key, found, err)
	}
	// Delete answers how many ids it was named, not how many were there.
	if n, err := store.Keys.Delete(ctx, []string{"k1", "never", ""}); err != nil || n != 2 {
		t.Fatalf("Delete = %d, %v, want 2", n, err)
	}
	if _, found, _ := store.Keys.Resolve(ctx, "k1"); found {
		t.Fatal("deleted key still resolves")
	}
}

func adjustSpentAppliesOncePerID(t *testing.T, store repository.Store) {
	if _, _, found, err := store.Keys.AdjustSpent(ctx, "never", "a1", 5); err != nil || found {
		t.Fatalf("unpushed key: found=%v err=%v", found, err)
	}
	must(t, store.Keys.Upsert(ctx, []repository.KeyUpsert{{MeterID: "k1", Prefix: "abc", Status: "active"}}))
	if spent, applied, found, err := store.Keys.AdjustSpent(ctx, "k1", "a1", 300); err != nil || !applied || !found || spent != 300 {
		t.Fatalf("first: spent=%d applied=%v found=%v err=%v", spent, applied, found, err)
	}
	if spent, applied, _, err := store.Keys.AdjustSpent(ctx, "k1", "a1", 300); err != nil || applied || spent != 300 {
		t.Fatalf("repeat: spent=%d applied=%v err=%v, want 300 unapplied", spent, applied, err)
	}
}

func anUnpushedGroupIsTheZeroValue(t *testing.T, store repository.Store) {
	group, err := store.Groups.Get(ctx, "never")
	if err != nil || len(group.Models) != 0 {
		t.Fatalf("Get = %+v, %v, want the zero value", group, err)
	}
}

func routesListModelsSortedAndDelete(t *testing.T, store repository.Store) {
	route := domain.Route{EngineURL: "http://e1", Healthy: true}
	for _, model := range []string{"zeta", "alpha"} {
		must(t, store.Routes.Replace(ctx, model, []domain.Route{route, route}))
	}
	if models, err := store.Routes.Models(ctx); err != nil || !slices.Equal(models, []string{"alpha", "zeta"}) {
		t.Fatalf("Models = %v, %v", models, err)
	}
	if routes, err := store.Routes.Get(ctx, "alpha"); err != nil || len(routes) != 2 || routes[0].EngineURL != "http://e1" {
		t.Fatalf("Get = %+v, %v", routes, err)
	}
	must(t, store.Routes.DeleteModels(ctx, []string{"zeta"}))
	if models, _ := store.Routes.Models(ctx); !slices.Equal(models, []string{"alpha"}) {
		t.Fatalf("after delete Models = %v", models)
	}
}

func aSessionPinsItsEngine(t *testing.T, store repository.Store) {
	if engine, err := store.Sessions.Engine(ctx, "s1"); err != nil || engine != "" {
		t.Fatalf("unpinned: %q, %v", engine, err)
	}
	must(t, store.Sessions.Pin(ctx, "s1", "http://e1", time.Minute))
	if engine, err := store.Sessions.Engine(ctx, "s1"); err != nil || engine != "http://e1" {
		t.Fatalf("pinned: %q, %v", engine, err)
	}
}

func inFlightCountsClaimsInTheOrderAsked(t *testing.T, store repository.Store) {
	must(t, store.InFlight.Claim(ctx, "http://e1", "r1"))
	must(t, store.InFlight.Claim(ctx, "http://e1", "r2"))
	must(t, store.InFlight.Claim(ctx, "http://e2", "r3"))
	must(t, store.InFlight.Release(ctx, "http://e1", "r1"))
	counts, err := store.InFlight.Counts(ctx, []string{"http://e2", "http://none", "http://e1"})
	if err != nil || !slices.Equal(counts, []int{1, 0, 1}) {
		t.Fatalf("Counts = %v, %v, want [1 0 1]", counts, err)
	}
}

func aSuccessClearsTheFailures(t *testing.T, store repository.Store) {
	must(t, store.Health.RecordFailure(ctx, "a"))
	must(t, store.Health.RecordFailure(ctx, "a"))
	must(t, store.Health.RecordFailure(ctx, "b"))
	must(t, store.Health.RecordSuccess(ctx, "b"))
	if failures, err := store.Health.Failures(ctx, []string{"b", "a", "none"}); err != nil || !slices.Equal(failures, []int{0, 2, 0}) {
		t.Fatalf("Failures = %v, %v, want [0 2 0]", failures, err)
	}
}

func providerKeysTakeClears(t *testing.T, store repository.Store) {
	must(t, store.ProviderKeys.Count(ctx, "p1", 200))
	must(t, store.ProviderKeys.Count(ctx, "p1", 429))
	stats, err := store.ProviderKeys.Take(ctx, []string{"p1", "never"})
	if err != nil || stats["p1"].Requests != 2 || stats["p1"].OK != 1 || stats["p1"].RateLimited != 1 || stats["never"].Requests != 0 {
		t.Fatalf("Take = %+v, %v", stats, err)
	}
	if again, _ := store.ProviderKeys.Take(ctx, []string{"p1"}); again["p1"].Requests != 0 {
		t.Fatalf("second Take = %+v, want zero", again["p1"])
	}
}

func admitTakesTheLastSlotOnce(t *testing.T, store repository.Store) {
	limits := []domain.Limit{{Metric: domain.LimitRequests, Window: "1h", Value: 1}}
	now := time.Now()
	if spent, err := store.Limits.Admit(ctx, "k1", limits, now); err != nil || len(spent) != 0 {
		t.Fatalf("first Admit = %v, %v, want admitted", spent, err)
	}
	if spent, err := store.Limits.Admit(ctx, "k1", limits, now); err != nil || len(spent) != 1 {
		t.Fatalf("second Admit = %v, %v, want the limit spent", spent, err)
	}
	if spent, _ := store.Limits.Admit(ctx, "k2", limits, now); len(spent) != 0 {
		t.Fatalf("another key's Admit = %v, want admitted", spent)
	}
}

func accrual(id string) repository.Accrual {
	return repository.Accrual{ID: id, Prefix: "abc", Fields: map[string]int64{"request_count": 1}}
}

// requests sums request_count over every day a drain id carries for prefix abc.
func requests(drains repository.Drains, id string) int {
	n := 0
	for _, counters := range drains[id]["abc"] {
		count, _ := strconv.Atoi(counters["request_count"])
		n += count
	}
	return n
}

func aDrainIsResentUntilAcked(t *testing.T, store repository.Store) {
	must(t, store.Usage.Accrue(ctx, accrual("r1")))
	first, err := store.Usage.Drain(ctx, "d1", nil)
	if err != nil || requests(first, "d1") != 1 {
		t.Fatalf("first Drain = %v, %v", first, err)
	}
	must(t, store.Usage.Accrue(ctx, accrual("r2")))
	second, err := store.Usage.Drain(ctx, "d2", nil)
	if err != nil || requests(second, "d1") != 1 || requests(second, "d2") != 1 {
		t.Fatalf("second Drain = %v, %v, want d1 resent beside d2", second, err)
	}
	acks := repository.Acks{"d1": {}}
	for day := range second["d1"]["abc"] {
		acks["d1"]["abc"] = append(acks["d1"]["abc"], day)
	}
	if n, err := store.Usage.Ack(ctx, acks, time.Hour); err != nil || n != 1 {
		t.Fatalf("Ack = %d, %v, want 1", n, err)
	}
	third, err := store.Usage.Drain(ctx, "d3", nil)
	if err != nil || requests(third, "d1") != 0 || requests(third, "d2") != 1 {
		t.Fatalf("after ack Drain = %v, %v, want d2 only", third, err)
	}
}

func aPeekMovesNothing(t *testing.T, store repository.Store) {
	must(t, store.Usage.Accrue(ctx, accrual("r1")))
	peek, err := store.Usage.Peek(ctx, nil)
	if err != nil || requests(peek, "") != 1 {
		t.Fatalf("Peek = %v, %v, want the live counter under \"\"", peek, err)
	}
	if drained, _ := store.Usage.Drain(ctx, "d1", nil); requests(drained, "d1") != 1 {
		t.Fatalf("Drain after Peek = %v, want the counter still there", drained)
	}
}

func aReplayLandsOnce(t *testing.T, store repository.Store) {
	if landed, err := store.Usage.Replay(ctx, accrual("r1")); err != nil || !landed {
		t.Fatalf("first Replay = %v, %v", landed, err)
	}
	if landed, err := store.Usage.Replay(ctx, accrual("r1")); err != nil || landed {
		t.Fatalf("second Replay = %v, %v, want not landed", landed, err)
	}
	if drained, _ := store.Usage.Drain(ctx, "d1", nil); requests(drained, "d1") != 1 {
		t.Fatalf("Drain = %v, want one request", drained)
	}
}
