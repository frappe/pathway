package provisioning

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository/memory"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func serving(model string) []domain.Route {
	return []domain.Route{{EngineURL: "https://box/e/" + model, Healthy: true}}
}

// Prune decides which models survive a complete push — the reason an ingress need not name every
// model in the catalogue every sync. Tested through the store, not by re-deriving the rule: the old
// test computed the stale set itself and compared it to itself, passing whatever the service did.
func TestPruneKeepsOnlyWhatThePayloadNames(t *testing.T) {
	for _, c := range []struct {
		name    string
		held    []string
		payload []string
		want    []string
	}{
		{"a model that moved away is dropped", []string{"a", "b", "c"}, []string{"a"}, []string{"a"}},
		{"nothing to do when the payload matches", []string{"a", "b"}, []string{"a", "b"}, []string{"a", "b"}},
		{"a payload naming more than is held drops nothing", []string{"a"}, []string{"a", "b"}, []string{"a", "b"}},
		{"an empty payload retires the whole table", []string{"a", "b"}, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := memory.New()
			for _, model := range c.held {
				store.Routes[model] = serving(model)
			}
			payload := map[string][]domain.Route{}
			for _, model := range c.payload {
				payload[model] = serving(model)
			}

			if _, _, err := New(store.Repositories(), quiet()).ReplaceRoutes(context.Background(), payload, true); err != nil {
				t.Fatalf("ReplaceRoutes: %v", err)
			}
			assertModels(t, store, c.want)
		})
	}
}

// Without prune the payload is a partial view, so a model it does not name is left alone. This is
// what an older control plane still expects.
func TestWithoutPruneAnUnnamedModelSurvives(t *testing.T) {
	store := memory.New()
	store.Routes["a"], store.Routes["b"] = serving("a"), serving("b")

	_, pruned, err := New(store.Repositories(), quiet()).ReplaceRoutes(
		context.Background(), map[string][]domain.Route{"a": serving("a")}, false)
	if err != nil {
		t.Fatalf("ReplaceRoutes: %v", err)
	}
	if pruned != 0 {
		t.Errorf("pruned = %d, want 0", pruned)
	}
	assertModels(t, store, []string{"a", "b"})
}

// An empty list is how a model is retired explicitly, with or without prune. Its absence from the
// table is what makes a request for it 503 rather than reaching a placement that is gone.
func TestAnEmptyListRetiresThatModel(t *testing.T) {
	store := memory.New()
	store.Routes["a"], store.Routes["b"] = serving("a"), serving("b")

	if _, _, err := New(store.Repositories(), quiet()).ReplaceRoutes(
		context.Background(), map[string][]domain.Route{"a": {}}, false); err != nil {
		t.Fatalf("ReplaceRoutes: %v", err)
	}
	assertModels(t, store, []string{"b"})
}

// A key is answered again until it is acknowledged, so a control plane that failed to record it
// gets the same delta under the same id; after the ack nothing is left to pull.
func TestUsageDrainsUntilAcked(t *testing.T) {
	store := memory.New()
	store.Usage["key-1"] = map[string]int64{"total_tokens": 42}
	svc := New(store.Repositories(), quiet())
	ctx := context.Background()

	first, err := svc.DrainUsage(ctx, nil)
	if err != nil || len(first) != 1 {
		t.Fatalf("first drain = %+v, %v", first, err)
	}
	var id string
	for id = range first {
	}
	if first[id]["key-1"]["total_tokens"] != "42" {
		t.Fatalf("first drain = %+v", first)
	}
	if again, _ := svc.DrainUsage(ctx, nil); again[id]["key-1"]["total_tokens"] != "42" || len(again) != 1 {
		t.Fatalf("unacked drain = %+v, want %s again", again, id)
	}
	if n, err := svc.AckUsage(ctx, map[string][]string{id: {"key-1"}}, time.Hour); err != nil || n != 1 {
		t.Fatalf("AckUsage = %d, %v", n, err)
	}
	if after, _ := svc.DrainUsage(ctx, nil); len(after) != 0 {
		t.Errorf("after ack = %+v — the same delta would be counted twice", after)
	}
}

// Drain ids sort by when they started and do not collide.
func TestDrainIDs(t *testing.T) {
	at := time.Date(2026, 9, 26, 10, 4, 5, 0, time.UTC)
	a, b := NewDrainID(at), NewDrainID(at)
	if a[:17] != "20260926T100405Z-" || len(a) != 25 || a == b {
		t.Errorf("ids = %q %q", a, b)
	}
}

func assertModels(t *testing.T, store *memory.Store, want []string) {
	t.Helper()
	if len(store.Routes) != len(want) {
		t.Fatalf("held %v, want %v", keysOf(store), want)
	}
	for _, model := range want {
		if _, ok := store.Routes[model]; !ok {
			t.Errorf("model %q was dropped; held %v", model, keysOf(store))
		}
	}
}

func keysOf(store *memory.Store) []string {
	out := make([]string, 0, len(store.Routes))
	for model := range store.Routes {
		out = append(out, model)
	}
	return out
}
