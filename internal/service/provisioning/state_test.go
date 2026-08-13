package provisioning

import (
	"context"
	"fmt"
	"testing"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
	"github.com/phot0n/pathway/internal/repository/memory"
)

// The state push (plan_agent_state_sync.md): a present section is authoritative for its
// namespace, absence deletes, and the carried hashes land with the records — the whole
// tombstone-free sync stands on these three.

func keyBucket(hash string, ids ...string) repository.KeyBucket {
	bucket := repository.KeyBucket{Hash: hash}
	for _, id := range ids {
		bucket.Records = append(bucket.Records, repository.KeyUpsert{
			MeterID: id, Prefix: "K-" + id, User: "GU-1", Status: "active",
		})
	}
	return bucket
}

func apply(t *testing.T, store *memory.Store, push repository.StatePush) repository.StateCounts {
	t.Helper()
	counts, err := New(store.Repositories(), quiet()).ApplyState(context.Background(), push)
	if err != nil {
		t.Fatalf("ApplyState: %v", err)
	}
	return counts
}

func TestAGroupThePushDoesNotNameIsDeleted(t *testing.T) {
	store := memory.New()
	store.Groups["kept"] = domain.GroupRecord{}
	store.Groups["stale"] = domain.GroupRecord{}

	apply(t, store, repository.StatePush{Groups: &repository.GroupsPush{
		Hash: "h", Records: []repository.GroupUpsert{{Name: "kept", Models: "m"}},
	}})

	if _, ok := store.Groups["stale"]; ok {
		t.Error("unnamed group survived the push")
	}
	if _, ok := store.Groups["kept"]; !ok {
		t.Error("named group was dropped")
	}
	if store.Hashes["groups"] != "h" {
		t.Errorf(`Hashes["groups"] = %q, want "h"`, store.Hashes["groups"])
	}
}

func TestAnEmptyCatalogClearsThePublishedOne(t *testing.T) {
	store := memory.New()
	old := "old-model"
	store.Public = &old

	apply(t, store, repository.StatePush{Groups: &repository.GroupsPush{Hash: "h"}})

	if store.Public != nil {
		t.Errorf("catalog = %q, want cleared", *store.Public)
	}
}

// sibling finds a distinct id in the same bucket as `id` — a bucket push only ever carries and
// prunes its own members, so the fixture must respect the labeling the control plane uses.
func sibling(t *testing.T, id string) string {
	t.Helper()
	for i := 0; i < 4096; i++ {
		candidate := fmt.Sprintf("%s-sib%d", id, i)
		if domain.BucketOf(candidate) == domain.BucketOf(id) {
			return candidate
		}
	}
	t.Fatal("no sibling found")
	return ""
}

func TestABucketPrunesOnlyItsOwnMembers(t *testing.T) {
	store := memory.New()
	store.Keys["aa"] = domain.KeyRecord{Status: "active"} // in the pushed bucket, unnamed → dies
	store.Keys["bb"] = domain.KeyRecord{Status: "active"} // its bucket is not pushed → survives
	pushed, kept := domain.BucketOf("aa"), sibling(t, "aa")
	if pushed == domain.BucketOf("bb") {
		t.Fatal("fixture ids landed in one bucket; pick ids that split")
	}

	apply(t, store, repository.StatePush{Keys: map[string]repository.KeyBucket{
		pushed: keyBucket("h1", kept),
	}})

	if _, ok := store.Keys["aa"]; ok {
		t.Error("unnamed key in the pushed bucket survived")
	}
	if _, ok := store.Keys["bb"]; !ok {
		t.Error("key in an untouched bucket was deleted")
	}
	if _, ok := store.Keys[kept]; !ok {
		t.Error("named key in the pushed bucket was dropped")
	}
	if store.Hashes["keys:"+pushed] != "h1" {
		t.Errorf("bucket hash = %q, want h1", store.Hashes["keys:"+pushed])
	}
}

func TestAnEmptyBucketPrunesItsMembersAndDropsItsHash(t *testing.T) {
	store := memory.New()
	store.Keys["aa"] = domain.KeyRecord{Status: "active"}
	label := domain.BucketOf("aa")
	store.Hashes["keys:"+label] = "stale"

	apply(t, store, repository.StatePush{Keys: map[string]repository.KeyBucket{label: {}}})

	if len(store.Keys) != 0 {
		t.Errorf("keys = %v, want the emptied bucket pruned", store.Keys)
	}
	if _, ok := store.Hashes["keys:"+label]; ok {
		t.Error("an empty bucket must drop its hash — the snapshot no longer has one for it")
	}
}

func TestRoutesReplaceTheWholeTable(t *testing.T) {
	store := memory.New()
	store.Routes["stale"] = serving("stale")

	counts := apply(t, store, repository.StatePush{Routes: &repository.RoutesPush{
		Hash: "h", Table: map[string][]domain.Route{"kept": serving("kept"), "empty": {}},
	}})

	assertModels(t, store, []string{"kept"})
	if counts.Routes != 1 {
		t.Errorf("counts.Routes = %d, want 1", counts.Routes)
	}
	if store.Hashes["routes"] != "h" {
		t.Errorf(`Hashes["routes"] = %q, want "h"`, store.Hashes["routes"])
	}
}

func TestAnAbsentSectionIsUntouched(t *testing.T) {
	store := memory.New()
	store.Keys["aa"] = domain.KeyRecord{Status: "active"}
	store.Groups["g"] = domain.GroupRecord{}

	apply(t, store, repository.StatePush{Routes: &repository.RoutesPush{Hash: "h"}})

	if len(store.Keys) != 1 || len(store.Groups) != 1 {
		t.Error("a routes-only push (the ingress shape) must not touch keys or groups")
	}
}

func TestAFailedApplyStoresNoHashes(t *testing.T) {
	store := memory.New()
	store.Fail["state"] = true

	_, err := New(store.Repositories(), quiet()).ApplyState(context.Background(),
		repository.StatePush{Groups: &repository.GroupsPush{Hash: "h"}})

	if err == nil {
		t.Fatal("want an error — an acknowledged push that did not land is divergence")
	}
	if len(store.Hashes) != 0 {
		t.Errorf("hashes = %v, want none on failure", store.Hashes)
	}
}
