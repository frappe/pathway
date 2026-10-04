package admission

import (
	"context"
	"errors"
	"testing"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository/memory"
)

const secret = "gr_test"

func meterID() string { return domain.SHA256Hex(secret) }

func serviceOver(store *memory.Store) *Service {
	repos := store.Repositories()
	return New(repos.Keys, repos.Users, repos.Groups)
}

// The happy path is three hops — key → user → group — and the whole reason the records are split.
func TestIdentifyFollowsKeyToUserToGroup(t *testing.T) {
	store := memory.New()
	store.Keys[meterID()] = domain.KeyRecord{Status: "active", User: "test-user", KeyPrefix: "abc123"}
	store.Users["test-user"] = domain.UserRecord{Groups: domain.ModelSet("acme"), Email: "r@example.com"}
	store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b")}

	id, err := serviceOver(store).Identify(context.Background(), "Bearer "+secret)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if id.Prefix() != "abc123" {
		t.Errorf("prefix = %q, want abc123 — usage would accrue to the wrong bucket", id.Prefix())
	}
}

func TestIdentifyRefusals(t *testing.T) {
	store := memory.New()
	store.Keys[meterID()] = domain.KeyRecord{Status: "active", User: "test-user"}
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
	store.Keys[meterID()] = domain.KeyRecord{Status: "active", User: "test-user"}
	store.Fail["keys"] = true

	_, err := serviceOver(store).Identify(context.Background(), "Bearer "+secret)
	assertStatus(t, err, 503)
}

// A key whose user record has not landed yet falls back to what the key itself carries. This is
// what makes the control plane and the agent deployable in either order.
func TestAPreSplitKeyFallsBackToItsOwnProjection(t *testing.T) {
	store := memory.New()
	store.Keys[meterID()] = domain.KeyRecord{
		Status: "active", User: "test-user",
		Legacy: domain.LegacyKey{Models: domain.ModelSet("qwen3-4b")},
	}
	// No user:test-user record.

	id, err := serviceOver(store).Identify(context.Background(), "Bearer "+secret)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if err := serviceOver(store).Authorize(id, "qwen3-4b"); err != nil {
		t.Errorf("a pre-split key lost the access its own record granted: %v", err)
	}
}

// A CURRENT key with no user record must reach nothing. It carries no projection to fall back to,
// and inventing one would be the difference between failing closed and failing open.
func TestACurrentKeyWithNoUserRecordReachesNothing(t *testing.T) {
	store := memory.New()
	store.Keys[meterID()] = domain.KeyRecord{Status: "active", User: "test-user"}
	store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b")}

	id, err := serviceOver(store).Identify(context.Background(), "Bearer "+secret)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	assertStatus(t, serviceOver(store).Authorize(id, "qwen3-4b"), 403)
}

// Order matters: a revoked key is 401 even for a holder who is also over budget, because the key
// is the thing that is wrong.
func TestAuthorizeChecksTheCredentialBeforeTheBudget(t *testing.T) {
	store := memory.New()
	store.Keys[meterID()] = domain.KeyRecord{Status: "revoked", User: "test-user"}
	store.Users["test-user"] = domain.UserRecord{Groups: domain.ModelSet("acme"), Limited: true}
	store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b")}

	svc := serviceOver(store)
	id, err := svc.Identify(context.Background(), "Bearer "+secret)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	assertStatus(t, svc.Authorize(id, "qwen3-4b"), 401)
}

func TestAuthorizeGates(t *testing.T) {
	for _, c := range []struct {
		name  string
		user  domain.UserRecord
		model string
		want  int
	}{
		{"out of credit", domain.UserRecord{Groups: domain.ModelSet("acme"), Limited: true}, "qwen3-4b", 402},
		{"model the group does not grant", domain.UserRecord{Groups: domain.ModelSet("acme")}, "secret-model", 403},
		{"deny beats the group's grant",
			domain.UserRecord{Groups: domain.ModelSet("acme"), Deny: domain.ModelSet("qwen3-4b")}, "qwen3-4b", 403},
		{"granted", domain.UserRecord{Groups: domain.ModelSet("acme")}, "qwen3-4b", 0},
		{"the user's own allow, on top of the group",
			domain.UserRecord{Groups: domain.ModelSet("acme"), Allow: domain.ModelSet("extra")}, "extra", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := memory.New()
			store.Keys[meterID()] = domain.KeyRecord{Status: "active", User: "test-user"}
			store.Users["test-user"] = c.user
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
// the user's own Deny still beats both.
func TestIdentifyUnionsEveryGroupTheUserIsIn(t *testing.T) {
	store := memory.New()
	store.Keys[meterID()] = domain.KeyRecord{Status: "active", User: "test-user"}
	store.Users["test-user"] = domain.UserRecord{
		Groups: domain.ModelSet("acme,beta"),
		Deny:   domain.ModelSet("secret-model"),
	}
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
	store.Keys[meterID()] = domain.KeyRecord{Status: "active", User: "test-user"}
	store.Users["test-user"] = domain.UserRecord{Groups: domain.ModelSet("acme,beta")}
	store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b")}
	store.Groups["beta"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-35b")}

	if _, err := serviceOver(store).Identify(context.Background(), "Bearer "+secret); err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(store.Groups["acme"].Models) != 1 {
		t.Errorf("group acme now grants %v — the union aliased the store's map", store.Groups["acme"].Models)
	}
}

// An ungrouped user reaches nothing but their own Allow, so the group store is never touched — a
// failing one proves the read did not happen.
func TestIdentifySkipsTheGroupStoreWhenUngrouped(t *testing.T) {
	store := memory.New()
	store.Keys[meterID()] = domain.KeyRecord{Status: "active", User: "test-user"}
	store.Users["test-user"] = domain.UserRecord{Allow: domain.ModelSet("qwen3-4b")}
	store.Fail["groups"] = true

	id, err := serviceOver(store).Identify(context.Background(), "Bearer "+secret)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if err := serviceOver(store).Authorize(id, "qwen3-4b"); err != nil {
		t.Errorf("Authorize = %v, want their own Allow to admit", err)
	}
}
