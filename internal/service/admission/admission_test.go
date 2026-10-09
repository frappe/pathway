package admission

import (
	"context"
	"errors"
	"testing"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
	"github.com/phot0n/pathway/internal/repository/memory"
)

const secret = "gr_test"

func meterID() string { return domain.SHA256Hex(secret) }

func serviceOver(store *memory.Store) *Service {
	repos := store.Repositories()
	return New(repos.Keys, repos.Groups, repos.Limits)
}

// liveKey is an active key in the named groups.
func liveKey(groups string) domain.KeyRecord {
	return domain.KeyRecord{Status: "active", Team: "T-1", KeyPrefix: "abc123", Groups: domain.ModelSet(groups)}
}

// The happy path is two hops — key → group — and the whole reason the group is its own record.
func TestIdentifyFollowsKeyToGroup(t *testing.T) {
	store := memory.New()
	store.Keys[meterID()] = liveKey("acme")
	store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b")}

	id, err := serviceOver(store).Identify(context.Background(), "Bearer "+secret)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if id.Prefix() != "abc123" {
		t.Errorf("prefix = %q, want abc123 — usage would accrue to the wrong bucket", id.Prefix())
	}
	if !id.Grant.Models["qwen3-4b"] {
		t.Errorf("grant = %v, want the group's model", id.Grant.Models)
	}
}

func TestIdentifyRefusals(t *testing.T) {
	store := memory.New()
	store.Keys[meterID()] = liveKey("")
	svc := serviceOver(store)

	for _, c := range []struct {
		name, header string
		want         int
	}{
		{"no header at all", "", 401},
		{"a bearer with nothing after it", "Bearer ", 401},
		{"a key that was never pushed", "Bearer nope", 401},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.Identify(context.Background(), c.header)
			assertStatus(t, err, c.want)
		})
	}
}

// A store that cannot be read is a 503, never a 401: answering "unknown api key" would send the
// caller to rotate a credential that was fine.
func TestAnUnreadableStoreIsNotAnAuthFailure(t *testing.T) {
	store := memory.New()
	store.Keys[meterID()] = liveKey("")
	store.Fail["keys"] = true

	_, err := serviceOver(store).Identify(context.Background(), "Bearer "+secret)
	assertStatus(t, err, 503)
}

// A key with no group and no allow of its own reaches nothing: there is nothing to fall back
// to, and inventing a grant would be the difference between failing closed and failing open.
func TestAKeyWithNoGrantReachesNothing(t *testing.T) {
	store := memory.New()
	store.Keys[meterID()] = liveKey("")
	store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b")}

	id, err := serviceOver(store).Identify(context.Background(), "Bearer "+secret)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	assertStatus(t, serviceOver(store).Authorize(id, "qwen3-4b"), 403)
}

// Order matters: a revoked key is 401 even when it is also over budget, because the key is the
// thing that is wrong.
func TestAuthorizeChecksTheCredentialBeforeTheBudget(t *testing.T) {
	store := memory.New()
	rec := liveKey("acme")
	rec.Status, rec.Limited = "revoked", true
	store.Keys[meterID()] = rec
	store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b")}

	svc := serviceOver(store)
	id, err := svc.Identify(context.Background(), "Bearer "+secret)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	assertStatus(t, svc.Authorize(id, "qwen3-4b"), 401)
}

func TestAuthorizeGates(t *testing.T) {
	limited := liveKey("acme")
	limited.Limited = true
	denied := liveKey("acme")
	denied.Deny = domain.ModelSet("qwen3-4b")
	allowed := liveKey("acme")
	allowed.Allow = domain.ModelSet("extra")
	for _, c := range []struct {
		name  string
		rec   domain.KeyRecord
		model string
		want  int
	}{
		{"out of credit", limited, "qwen3-4b", 402},
		{"model the group does not grant", liveKey("acme"), "secret-model", 403},
		{"deny beats the group's grant", denied, "qwen3-4b", 403},
		{"granted", liveKey("acme"), "qwen3-4b", 0},
		{"the key's own allow, on top of the group", allowed, "extra", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := memory.New()
			store.Keys[meterID()] = c.rec
			store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b")}

			svc := serviceOver(store)
			id, err := svc.Identify(context.Background(), "Bearer "+secret)
			if err != nil {
				t.Fatalf("Identify: %v", err)
			}
			assertStatus(t, svc.Authorize(id, c.model), c.want)
		})
	}
}

// assertStatus checks the refusal status, with want 0 meaning "should have been admitted".
func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	if want == 0 {
		if err != nil {
			t.Fatalf("refused with %v, want admitted", err)
		}
		return
	}
	var denial domain.Denial
	if !errors.As(err, &denial) {
		t.Fatalf("err = %v, want a Denial with status %d", err, want)
	}
	if denial.Status != want {
		t.Errorf("status = %d (%s), want %d", denial.Status, denial.Reason, want)
	}
}

// Membership is a list, and the grant is the union of it. A model from either group admits, and
// the key's own Deny still beats both.
func TestIdentifyUnionsEveryGroupTheKeyIsIn(t *testing.T) {
	store := memory.New()
	rec := liveKey("acme,beta")
	rec.Deny = domain.ModelSet("secret-model")
	store.Keys[meterID()] = rec
	store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b")}
	store.Groups["beta"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-35b,secret-model")}

	svc := serviceOver(store)
	id, err := svc.Identify(context.Background(), "Bearer "+secret)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	for _, model := range []string{"qwen3-4b", "qwen3-35b"} {
		if err := svc.Authorize(id, model); err != nil {
			t.Errorf("Authorize(%q) = %v, want admitted", model, err)
		}
	}
	if err := svc.Authorize(id, "secret-model"); err == nil {
		t.Error("deny lost to a second group's grant")
	}
}

// The union must not write into the record the store handed back, or reading a group would edit it.
func TestIdentifyDoesNotMutateTheStoredGroup(t *testing.T) {
	store := memory.New()
	store.Keys[meterID()] = liveKey("acme,beta")
	store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b")}
	store.Groups["beta"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-35b")}

	if _, err := serviceOver(store).Identify(context.Background(), "Bearer "+secret); err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(store.Groups["acme"].Models) != 1 {
		t.Errorf("group acme now grants %v — the union aliased the store's map", store.Groups["acme"].Models)
	}
}

// forgetful answers every credential without its groups, as a store that split a group list
// differently from domain.ModelSet would.
type forgetful struct{ repository.Keys }

func (f forgetful) Resolve(ctx context.Context, meterID string) (repository.Holder, bool, error) {
	holder, found, err := f.Keys.Resolve(ctx, meterID)
	holder.Groups = nil
	return holder, found, err
}

// The key record says which groups; reading them beside the key only saves round trips. One the
// store did not read is read on its own, never taken for a group that grants nothing.
func TestAGroupTheStoreDidNotReadIsReadOnItsOwn(t *testing.T) {
	store := memory.New()
	store.Keys[meterID()] = liveKey("acme")
	store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b")}
	repos := store.Repositories()
	svc := New(forgetful{repos.Keys}, repos.Groups, repos.Limits)

	id, err := svc.Identify(context.Background(), "Bearer "+secret)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if err := svc.Authorize(id, "qwen3-4b"); err != nil {
		t.Errorf("Authorize = %v, want the group read on its own to admit", err)
	}
}

// An ungrouped key reaches nothing but its own Allow, so the group store is never touched — a
// failing one proves the read did not happen.
func TestIdentifySkipsTheGroupStoreWhenUngrouped(t *testing.T) {
	store := memory.New()
	rec := liveKey("")
	rec.Allow = domain.ModelSet("qwen3-4b")
	store.Keys[meterID()] = rec
	store.Fail["groups"] = true

	id, err := serviceOver(store).Identify(context.Background(), "Bearer "+secret)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if err := serviceOver(store).Authorize(id, "qwen3-4b"); err != nil {
		t.Errorf("Authorize = %v, want their own Allow to admit", err)
	}
}

// Rate limits are the key's own, counted under its prefix: a second key of the same team has its
// own window.
func TestAdmitCountsUnderTheKeysPrefix(t *testing.T) {
	store := memory.New()
	rec := liveKey("acme")
	rec.Limits = []domain.Limit{{Metric: domain.LimitRequests, Window: "1m", Value: 1}}
	store.Keys[meterID()] = rec
	svc := serviceOver(store)
	id, err := svc.Identify(context.Background(), "Bearer "+secret)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if err := svc.Admit(context.Background(), id); err != nil {
		t.Fatalf("first = %v, want admitted", err)
	}
	assertStatus(t, svc.Admit(context.Background(), id), 429)
	for counter := range store.Limits {
		if counter[:len("abc123:")] != "abc123:" {
			t.Errorf("counter %q is not under the key's prefix", counter)
		}
	}
}
