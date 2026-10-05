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

// The prepaid gate through the chain: refused at quota, before the body is read, in the shape
// of the surface asked; the control plane's flag refuses alike.
func TestThePrepaidGate(t *testing.T) {
	cases := []struct {
		name   string
		user   domain.UserRecord
		status int
		reason string
	}{
		{"funded admits", domain.UserRecord{Prepaid: true, Budget: 1000, Spent: 999}, 200, ""},
		{"exhausted", domain.UserRecord{Prepaid: true, Budget: 1000, Spent: 1000}, 402, "credit balance exhausted"},
		{"not prepaid ignores the budget", domain.UserRecord{Budget: 1000, Spent: 5000}, 200, ""},
		{"limited refuses a funded holder", domain.UserRecord{Limited: true, Prepaid: true, Budget: 1000}, 402, "credit balance exhausted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, jsonEngine(`{`+usageObject+`}`))
			tc.user.Groups = domain.ModelSet("acme")
			f.store.Users["test-user"] = tc.user

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
	f.store.Users["test-user"] = domain.UserRecord{Groups: domain.ModelSet("acme"), Prepaid: true}

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

// A served request is priced off its route's pricing and moves the holder's spend, and the next
// request sees the new balance — the gate is exact within one store.
func TestAServedRequestSpendsTheBalance(t *testing.T) {
	f := newFixture(t, jsonEngine(`{`+usageObject+`}`))
	f.store.Users["test-user"] = domain.UserRecord{Groups: domain.ModelSet("acme"), Prepaid: true, Budget: 400_000}
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
		"cost": cost, "p:mp1:cost": cost, "user_spent": cost, "user_balance": 400_000 - cost,
		"prompt_tokens": 100, "cached_tokens": 80, "completion_tokens": 20,
	} {
		if usage[field] != want {
			t.Errorf("usage[%s] = %d, want %d", field, usage[field], want)
		}
	}
	if f.store.Users["test-user"].Spent != cost {
		t.Errorf("spent = %d, want %d", f.store.Users["test-user"].Spent, cost)
	}
	// 384,000 spent of 400,000: one more request of the same size crosses the ceiling.
	if resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","messages":[]}`); resp.Code != http.StatusOK {
		t.Fatalf("second status = %d", resp.Code)
	}
	if resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","messages":[]}`); resp.Code != http.StatusPaymentRequired {
		t.Fatalf("third status = %d, want 402 once the balance is spent", resp.Code)
	}
}

// The users section of a state push, as the control plane sends it: prepaid and budget land,
// spent — this box's own counter, never on the wire — does not move.
func TestAUsersPushCarriesTheCeilingAndLeavesSpentAlone(t *testing.T) {
	store := memory.New()
	store.Users["GU-1"] = domain.UserRecord{Spent: 700}
	handler := adminFixture(t, store)
	body := fmt.Sprintf(`{"users": {"buckets": {"%s": {"hash": "uh", "records": [
		{"name": "GU-1", "email": "a@b", "group": "acme", "prepaid": true, "budget": 5000000}]}}}}`,
		domain.BucketOf("GU-1"))
	if w := adminCall(t, handler, http.MethodPost, "/grove-admin/state", body); w.Code != http.StatusOK {
		t.Fatalf("POST state = %d: %s", w.Code, w.Body)
	}
	usr := store.Users["GU-1"]
	if !usr.Prepaid || usr.Budget != 5_000_000 || usr.Spent != 700 || !usr.Groups["acme"] {
		t.Errorf("user = %+v, want prepaid, budget 5000000, spent 700", usr)
	}
}

// GET /v1/credits is the holder's balance on this store, in dollars, for a key allowed to read
// it — overspent or not, since that is when it is asked — and a refusal for any other key.
func TestCreditsAreReadByAnAllowedKey(t *testing.T) {
	funded := domain.UserRecord{Prepaid: true, Budget: 15_750_000_000, Spent: 3_250_000_000}
	cases := []struct {
		name      string
		keyStatus string
		allowed   bool
		user      domain.UserRecord
		status    int
		want      string
	}{
		{"prepaid", "active", true, funded, 200, `{"balance":12.5,"spent":3.25,"is_free_user":false}`},
		{"overspent still reads", "active", true,
			domain.UserRecord{Prepaid: true, Budget: 1_000_000_000, Spent: 1_500_000_000},
			200, `{"balance":-0.5,"spent":1.5,"is_free_user":false}`},
		{"free", "active", true, domain.UserRecord{}, 200, `{"balance":0,"spent":0,"is_free_user":true}`},
		{"key not allowed", "active", false, funded, 403, "this key cannot read the balance"},
		{"revoked key", "revoked", true, funded, 401, "unknown or revoked api key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, jsonEngine(`{}`))
			f.store.Keys[domain.SHA256Hex(secret)] = domain.KeyRecord{
				Status: tc.keyStatus, User: "test-user", KeyPrefix: "abc123", CanReadBalance: tc.allowed,
			}
			f.store.Users["test-user"] = tc.user
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
