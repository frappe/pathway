package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository/memory"
)

// withKey rewrites the fixture's key with the credit fields given, keeping its grant.
func (f *fixture) withKey(edit func(*domain.KeyRecord)) {
	id := domain.SHA256Hex(secret)
	rec := f.store.Keys[id]
	edit(&rec)
	f.store.Keys[id] = rec
}

// The prepaid gate through the chain: refused at quota, before the body is read, in the shape
// of the surface asked; the control plane's flag refuses alike.
func TestThePrepaidGate(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(*domain.KeyRecord)
		status int
		reason string
	}{
		{"funded admits", func(k *domain.KeyRecord) { k.Prepaid, k.Budget, k.Spent = true, 1000, 999 }, 200, ""},
		{"exhausted", func(k *domain.KeyRecord) { k.Prepaid, k.Budget, k.Spent = true, 1000, 1000 }, 402, "credit balance exhausted"},
		{"not prepaid ignores the cap", func(k *domain.KeyRecord) { k.Budget, k.Spent = 1000, 5000 }, 200, ""},
		{"limited refuses a funded key", func(k *domain.KeyRecord) { k.Limited, k.Prepaid, k.Budget = true, true, 1000 }, 402, "credit balance exhausted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, jsonEngine(`{`+usageObject+`}`))
			f.withKey(tc.edit)

			resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","messages":[]}`)
			if resp.Code != tc.status {
				t.Fatalf("status = %d, body = %s", resp.Code, resp.Body)
			}
			if tc.status == 200 {
				return
			}
			var body struct {
				Error struct{ Message, Type string } `json:"error"`
			}
			_ = json.Unmarshal(resp.Body.Bytes(), &body)
			if body.Error.Message != tc.reason || body.Error.Type != domain.AnthropicErrorType(tc.status) {
				t.Errorf("error = %+v", body.Error)
			}
			if f.seen.path != "" {
				t.Error("a refused request reached the engine")
			}
			if len(f.store.Usage) != 0 {
				t.Errorf("a refused request was metered: %v", f.store.Usage)
			}
		})
	}
}

// Under /anthropic the refusal takes the Anthropic envelope, like every other denial.
func TestThePrepaidGateSpeaksAnthropic(t *testing.T) {
	f := providerFixtureAnswering(t, jsonEngine(anthropicMessage))
	f.withKey(func(k *domain.KeyRecord) { k.Prepaid = true })

	resp := f.post("/anthropic/v1/messages", `{"model":"anthropic/claude-4-5","max_tokens":16,"messages":[]}`)
	if resp.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d", resp.Code)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct{ Type, Message string }
	}
	_ = json.Unmarshal(resp.Body.Bytes(), &body)
	if body.Type != "error" || body.Error.Type != "billing_error" || body.Error.Message != "credit balance exhausted" {
		t.Errorf("body = %s", resp.Body)
	}
}

// counters is the control plane's table as the catalog ships it, the way every pushed pricing
// carries it.
func counters(t *testing.T) domain.CounterTable {
	t.Helper()
	raw, err := os.ReadFile("../../domain/testdata/counters.json")
	if err != nil {
		t.Fatal(err)
	}
	var table domain.CounterTable
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	return table
}

// A served request is priced off its route's pricing and moves the key's spend, and the next
// request sees the new balance — the gate is exact within one store.
func TestAServedRequestSpendsTheCap(t *testing.T) {
	f := newFixture(t, jsonEngine(`{`+usageObject+`}`))
	f.withKey(func(k *domain.KeyRecord) { k.Prepaid, k.Budget = true, 400_000 })
	f.store.Routes["qwen3-4b"][0].Pricing = &domain.Pricing{
		ID: "mp1", Rates: map[string]int64{"prompt_tokens": 3e9, "cached_tokens": 3e8, "completion_tokens": 15e9},
		Counters: counters(t),
	}

	if resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","messages":[]}`); resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body)
	}
	cost := int64(20*3000 + 80*300 + 20*15000)
	usage := f.store.Usage["abc123"]
	for field, want := range map[string]int64{
		"cost": cost, "p:mp1:cost": cost, "key_spent": cost, "key_balance": 400_000 - cost,
		"prompt_tokens": 100, "cached_tokens": 80, "completion_tokens": 20,
	} {
		if usage[field] != want {
			t.Errorf("usage[%s] = %d, want %d", field, usage[field], want)
		}
	}
	if spent := f.store.Keys[domain.SHA256Hex(secret)].Spent; spent != cost {
		t.Errorf("spent = %d, want %d", spent, cost)
	}
	// 384,000 spent of 400,000: one more request of the same size crosses the ceiling.
	if resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","messages":[]}`); resp.Code != http.StatusOK {
		t.Fatalf("second status = %d", resp.Code)
	}
	if resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","messages":[]}`); resp.Code != http.StatusPaymentRequired {
		t.Fatalf("third status = %d, want 402 once the cap is spent", resp.Code)
	}
}

// The keys section of a state push, as the control plane sends it: prepaid and the cap land,
// spent — this box's own counter, never on the wire — does not move.
func TestAKeysPushCarriesTheCapAndLeavesSpentAlone(t *testing.T) {
	store := memory.New()
	store.Keys["aa"] = domain.KeyRecord{Spent: 700}
	handler := adminFixture(t, store)
	body := fmt.Sprintf(`{"keys": {"buckets": {"%s": {"hash": "kh", "records": [
		{"key_hash": "aa", "prefix": "K-1", "team": "T-1", "status": "active", "group": "acme",
		 "prepaid": true, "budget": 5000000}]}}}}`,
		domain.BucketOf("aa"))
	if w := adminCall(t, handler, http.MethodPost, "/grove-admin/state", body); w.Code != http.StatusOK {
		t.Fatalf("POST state = %d: %s", w.Code, w.Body)
	}
	rec := store.Keys["aa"]
	if !rec.Prepaid || rec.Budget != 5_000_000 || rec.Spent != 700 || !rec.Groups["acme"] || rec.Team != "T-1" {
		t.Errorf("key = %+v, want prepaid, budget 5000000, spent 700, in acme, team T-1", rec)
	}
}

// GET /v1/credits is the key's own balance on this store, in dollars — its cap less what it
// spent, nothing of the team's — overspent or not, since that is when it is asked.
func TestCreditsAreTheKeysOwn(t *testing.T) {
	cases := []struct {
		name      string
		keyStatus string
		edit      func(*domain.KeyRecord)
		status    int
		want      string
	}{
		{"prepaid", "active", func(k *domain.KeyRecord) { k.Prepaid, k.Budget, k.Spent = true, 15_750_000_000, 3_250_000_000 },
			200, `{"balance":12.5,"spent":3.25,"is_free_user":false}`},
		{"overspent still reads", "active", func(k *domain.KeyRecord) { k.Prepaid, k.Budget, k.Spent = true, 1_000_000_000, 1_500_000_000 },
			200, `{"balance":-0.5,"spent":1.5,"is_free_user":false}`},
		{"free", "active", func(*domain.KeyRecord) {}, 200, `{"balance":0,"spent":0,"is_free_user":true}`},
		{"revoked key", "revoked", func(k *domain.KeyRecord) { k.Prepaid, k.Budget = true, 1_000_000_000 }, 401, "unknown or revoked api key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, jsonEngine(`{}`))
			f.withKey(func(k *domain.KeyRecord) { tc.edit(k); k.Status = tc.keyStatus })
			r := httptest.NewRequest(http.MethodGet, "/v1/credits", nil)
			r.Header.Set("Authorization", "Bearer "+secret)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)

			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("status = %d, body = %s; want %d with %s", w.Code, w.Body, tc.status, tc.want)
			}
			if f.seen.path != "" {
				t.Error("/v1/credits was forwarded to an engine")
			}
		})
	}
}
