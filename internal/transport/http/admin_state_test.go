package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phot0n/pathway/internal/config"
	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/observability"
	"github.com/phot0n/pathway/internal/repository/memory"
	"github.com/phot0n/pathway/internal/service/provisioning"
)

// The /grove-admin/state surface end to end: the exact JSON the control plane sends, decoded,
// applied, and readable back through /grove-admin/state-hash. plan_agent_state_sync.md is the
// contract; these bodies are copied from its example shapes.

func adminFixture(t *testing.T, store *memory.Store) http.Handler {
	t.Helper()
	logs := observability.Discard()
	repos := store.Repositories()
	server := New(config.Config{AdminToken: "admin-token"}, Services{
		Provisioning: provisioning.New(repos, logs.Process),
		ProviderKeys: repos.ProviderKeys,
	}, logs.Process)
	return server.AdminHandler()
}

func adminCall(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-Grove-Admin-Token", "admin-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// A field this binary does not know is refused by name, never stored without. The row the
// control plane keeps then says which field, and the hash is never written — so the next tick
// pushes again instead of reading "in sync" off a lossy copy.
func TestAPushWithAFieldTheBinaryDoesNotKnowIsRefused(t *testing.T) {
	store := memory.New()
	handler := adminFixture(t, store)

	w := adminCall(t, handler, http.MethodPost, "/grove-admin/routes",
		`{"routes":{"m":[{"engine_url":"http://e","healthy":true,"keyring_v2":[]}]}}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `unknown field "keyring_v2"`) {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if len(store.Routes) != 0 {
		t.Errorf("a refused push was stored: %v", store.Routes)
	}
	w = adminCall(t, handler, http.MethodPost, "/grove-admin/state",
		`{"groups":{"hash":"h","records":[{"name":"g","models":"m","expires_at":"never"}]}}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `unknown field "expires_at"`) {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if hashes, _ := store.Repositories().State.Hashes(context.Background()); len(hashes) != 0 {
		t.Errorf("a refused push stored its hash: %v", hashes)
	}
}

func TestAStatePushLandsAndItsHashesReadBack(t *testing.T) {
	store := memory.New()
	store.Groups["stale"] = domain.GroupRecord{}
	handler := adminFixture(t, store)
	bucket := domain.BucketOf("aa")

	body := fmt.Sprintf(`{
		"groups": {"hash": "gh", "records": [{"name": "acme", "models": "m1"}]},
		"keys": {"buckets": {"%s": {"hash": "kh", "records": [
			{"key_hash": "aa", "prefix": "K-1", "team": "T-1", "status": "active", "group": "acme"}]}}},
		"routes": {"hash": "rh", "table": {"m1": [
			{"engine_url": "https://box/e/md1", "internal_key": "ek", "healthy": true,
			 "capacity": 8, "deployment": "MD-1", "server": "INF-1", "kind": "direct"}]}}
	}`, bucket)

	w := adminCall(t, handler, http.MethodPost, "/grove-admin/state", body)
	if w.Code != http.StatusOK {
		t.Fatalf("POST state = %d: %s", w.Code, w.Body)
	}
	var reply struct {
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if reply.Counts["groups"] != 1 || reply.Counts["keys"] != 1 || reply.Counts["routes"] != 1 {
		t.Errorf("counts = %v", reply.Counts)
	}

	if _, ok := store.Groups["stale"]; ok {
		t.Error("unnamed group survived")
	}
	if rec := store.Keys["aa"]; rec.KeyPrefix != "K-1" || rec.Team != "T-1" || !rec.Groups["acme"] {
		t.Errorf("key record = %+v", store.Keys["aa"])
	}
	if len(store.Routes["m1"]) != 1 || store.Routes["m1"][0].EngineURL != "https://box/e/md1" {
		t.Errorf("routes = %+v", store.Routes["m1"])
	}

	w = adminCall(t, handler, http.MethodGet, "/grove-admin/state-hash", "")
	var hashes struct {
		Hashes map[string]string `json:"hashes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &hashes); err != nil {
		t.Fatalf("state-hash: %v", err)
	}
	want := map[string]string{"groups": "gh", "routes": "rh", "keys:" + bucket: "kh"}
	for field, hash := range want {
		if hashes.Hashes[field] != hash {
			t.Errorf("hashes[%s] = %q, want %q", field, hashes.Hashes[field], hash)
		}
	}
}

func TestAFreshStoreAnswersAnEmptyHashMap(t *testing.T) {
	w := adminCall(t, adminFixture(t, memory.New()), http.MethodGet, "/grove-admin/state-hash", "")
	if w.Code != http.StatusOK {
		t.Fatalf("state-hash = %d", w.Code)
	}
	// {} not null — the control plane reads this as "the box holds nothing, push everything".
	if !strings.Contains(w.Body.String(), `"hashes":{}`) {
		t.Errorf("body = %s, want an empty object", w.Body)
	}
}

func TestAStoreErrorIsA500NotASilentOK(t *testing.T) {
	store := memory.New()
	store.Fail["state"] = true

	w := adminCall(t, adminFixture(t, store), http.MethodPost, "/grove-admin/state",
		`{"groups": {"hash": "h", "records": []}}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("POST state on a broken store = %d, want 500 — an acknowledged push that did "+
			"not land is divergence no retry heals", w.Code)
	}
}

func TestTheStateSurfaceIsTokenGated(t *testing.T) {
	handler := adminFixture(t, memory.New())
	for _, path := range []string{"/grove-admin/state", "/grove-admin/state-hash"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s without token = %d, want 403", path, w.Code)
		}
	}
}

func TestAnIngressServesTheStateSurfaceToo(t *testing.T) {
	store := memory.New()
	store.Routes["stale"] = []domain.Route{{EngineURL: "https://old/e/x", Healthy: true}}
	logs := observability.Discard()
	server := New(config.Config{AdminToken: "admin-token", IngressID: "ing-1"}, Services{
		Provisioning: provisioning.New(store.Repositories(), logs.Process),
	}, logs.Process)

	w := adminCall(t, server.AdminHandler(), http.MethodPost, "/grove-admin/state",
		`{"routes": {"hash": "rh", "table": {}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("ingress POST state = %d: %s", w.Code, w.Body)
	}
	if len(store.Routes) != 0 {
		t.Errorf("routes = %v, want the whole table pruned", store.Routes)
	}
}

// The pin rides the key record; a push without it decodes unpinned.
func TestAStatePushCarriesTheKeysGeography(t *testing.T) {
	store := memory.New()
	handler := adminFixture(t, store)
	body := fmt.Sprintf(`{"keys": {"buckets": {"%s": {"hash": "kh", "records": [
		{"key_hash": "aa", "prefix": "K-1", "team": "T-1", "status": "active", "group": "acme", "geography": "eu"}]},
		"%s": {"hash": "kh2", "records": [{"key_hash": "bb", "prefix": "K-2", "team": "T-1", "status": "active", "group": "acme"}]}}}}`,
		domain.BucketOf("aa"), domain.BucketOf("bb"))

	if w := adminCall(t, handler, http.MethodPost, "/grove-admin/state", body); w.Code != http.StatusOK {
		t.Fatalf("POST state = %d: %s", w.Code, w.Body)
	}
	if got := store.Keys["aa"].Geography; got != "eu" {
		t.Errorf("aa geography = %q, want eu", got)
	}
	if got := store.Keys["bb"].Geography; got != "" {
		t.Errorf("bb geography = %q, want unpinned", got)
	}
}

// A users section is no longer a thing this binary knows: a control plane still sending one is
// refused by name, so the mismatch is seen rather than half-applied.
func TestAUsersSectionIsRefused(t *testing.T) {
	store := memory.New()
	w := adminCall(t, adminFixture(t, store), http.MethodPost, "/grove-admin/state",
		`{"users": {"buckets": {}}}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `unknown field "users"`) {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
}
