package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phot0n/pathway/internal/domain"
)

// openaiVendorFixture points deepseek/chat at an OpenAI-only vendor, next to the Anthropic one.
func openaiVendorFixture(t *testing.T) *fixture {
	t.Helper()
	f := providerFixtureAnswering(t, jsonEngine(anthropicMessage))
	f.store.Groups["acme"] = domainGroup("qwen3-4b,anthropic/claude-4-5,deepseek/chat")
	f.store.Routes["deepseek/chat"] = openaiProviderRoute(f.engine.URL)
	return f
}

func domainGroup(models string) domain.GroupRecord {
	return domain.GroupRecord{Models: domain.ModelSet(models)}
}

func openaiProviderRoute(url string) []domain.Route {
	return []domain.Route{{
		EngineURL: url, InternalKey: "ds-key", Healthy: true,
		Deployment: "deepseek", Server: "deepseek", Kind: "provider", Dialect: "openai",
	}}
}

// The Anthropic front, end to end: the /anthropic base-path convention, the SDK's own
// credential spelling, and the dialect-filtered model list. Dialect is end-to-end — an
// Anthropic surface reaches an Anthropic-speaking upstream or 404s; nothing translates.

const anthropicMessage = `{"id":"msg_vendor1","type":"message","role":"assistant",` +
	`"content":[{"type":"text","text":"Hello!"}],"stop_reason":"end_turn",` +
	`"usage":{"input_tokens":20,"output_tokens":10,` +
	`"cache_read_input_tokens":80,"cache_creation_input_tokens":0}}`

// A native call passes byte-for-byte — the guarantee the no-translation call preserves
// unconditionally.
func TestANativeDialectResponseIsUntouched(t *testing.T) {
	f := providerFixtureAnswering(t, jsonEngine(anthropicMessage))
	resp := f.post("/anthropic/v1/messages", `{"model":"anthropic/claude-4-5","max_tokens":16,"messages":[]}`)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
	if resp.Body.String() != anthropicMessage {
		t.Errorf("a native response was rewritten:\n got %s\nwant %s", resp.Body, anthropicMessage)
	}
}

// Anthropic-dialect usage still meters correctly off the vendor's own frames.
func TestAnAnthropicResponseIsMetered(t *testing.T) {
	f := providerFixtureAnswering(t, jsonEngine(anthropicMessage))
	f.post("/anthropic/v1/messages", `{"model":"anthropic/claude-4-5","max_tokens":16,"messages":[]}`)

	usage := f.store.Usage["abc123"]
	for field, want := range map[string]int64{
		"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110, "cached_tokens": 80,
		"m:total_tokens:anthropic/claude-4-5": 110,
	} {
		if usage[field] != want {
			t.Errorf("usage[%s] = %d, want %d", field, usage[field], want)
		}
	}
}

// The provider-convention alias: ANTHROPIC_BASE_URL=<base>/anthropic plus the SDK's own
// x-api-key spelling, end to end.
func TestTheAnthropicAliasServesTheSDKConvention(t *testing.T) {
	f := providerFixtureAnswering(t, jsonEngine(anthropicMessage))
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(`{"model":"anthropic/claude-4-5","max_tokens":16,"messages":[]}`))
	r.Header.Set("x-api-key", secret)
	r.Header.Set("anthropic-version", "2023-06-01")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if f.seen.path != "/v1/messages" {
		t.Errorf("vendor path = %q — the prefix must be stripped, not forwarded", f.seen.path)
	}
	if f.seen.apiKey != "vendor-key" {
		t.Errorf("x-api-key = %q — the caller's key must be swapped, not forwarded", f.seen.apiKey)
	}
}

// Fail-closed under the alias: an OpenAI path under an Anthropic-declared base would answer in
// the wrong shape, so it does not exist there.
func TestTheAliasRefusesNonAnthropicSurfaces(t *testing.T) {
	for _, path := range []string{"/anthropic/v1/chat/completions", "/anthropic/v1/embeddings", "/anthropic/"} {
		f := providerFixtureAnswering(t, jsonEngine(anthropicMessage))
		resp := f.post(path, `{"model":"anthropic/claude-4-5"}`)
		if resp.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, resp.Code)
		}
		if f.seen.path != "" {
			t.Errorf("%s was dialled upstream", path)
		}
	}
}

// The same allow-list as /v1/models, rendered in the dialect the base path declares — and
// narrowed to what that surface can actually reach: an openai-only vendor model is left off
// the list it would 404 on.
func TestTheAliasServesADialectFilteredAnthropicModelList(t *testing.T) {
	f := providerFixtureAnswering(t, jsonEngine(anthropicMessage))
	f.store.Groups["acme"] = domainGroup("qwen3-4b,anthropic/claude-4-5,deepseek/chat")
	f.store.Routes["deepseek/chat"] = openaiProviderRoute(f.engine.URL)

	r := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
	r.Header.Set("x-api-key", secret)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var list struct {
		Data []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"data"`
		HasMore *bool `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || list.HasMore == nil {
		t.Fatalf("not an Anthropic list: %s", w.Body)
	}
	ids := map[string]bool{}
	for _, m := range list.Data {
		if m.Type != "model" {
			t.Errorf("entry type = %q", m.Type)
		}
		ids[m.ID] = true
	}
	if !ids["qwen3-4b"] || !ids["anthropic/claude-4-5"] {
		t.Errorf("engine and anthropic-vendor models must both list: %v", ids)
	}
	if ids["deepseek/chat"] {
		t.Error("an openai-only vendor model was advertised on the surface it 404s on")
	}
}

// Root is the OpenAI surface: an Anthropic path there is refused before any stage runs, pointing
// the client at /anthropic, and nothing is dialled.
func TestRootRefusesTheAnthropicSurface(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		f := providerFixtureAnswering(t, jsonEngine(anthropicMessage))
		resp := f.post(path, `{"model":"anthropic/claude-4-5","max_tokens":16,"messages":[]}`)
		if resp.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, resp.Code)
		}
		if !strings.Contains(resp.Body.String(), "/anthropic"+path) {
			t.Errorf("%s: the refusal must name where the client belongs: %s", path, resp.Body)
		}
		if f.seen.path != "" {
			t.Errorf("%s was dialled upstream", path)
		}
	}
}

// Nothing translates: a surface reaches only rows of its own dialect, and count_tokens has no
// analogue on the other side to fall back to.
func TestNothingCrossesTheDialect(t *testing.T) {
	f := openaiVendorFixture(t)
	for _, path := range []string{"/anthropic/v1/messages", "/anthropic/v1/messages/count_tokens"} {
		*f.seen = engineRecord{}
		if resp := f.post(path, `{"model":"deepseek/chat","max_tokens":1,"messages":[]}`); resp.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, resp.Code)
		}
		if f.seen.path != "" {
			t.Errorf("%s reached an openai-only vendor: %s", path, f.seen.path)
		}
	}
}

// A refusal the GATEWAY makes on an Anthropic surface speaks Anthropic too — Claude Code shows
// the parsed message, not a shape error.
func TestAGatewayDenialSpeaksTheSurfacesDialect(t *testing.T) {
	f := openaiVendorFixture(t)

	resp := f.post("/anthropic/v1/messages", `{"model":"nobody/nothing","max_tokens":1,"messages":[]}`)
	if resp.Code != http.StatusForbidden && resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), `"type":"error"`) {
		t.Errorf("anthropic surface got the OpenAI envelope: %s", resp.Body)
	}

	resp = f.post("/v1/chat/completions", `{"model":"nobody/nothing","messages":[]}`)
	if strings.Contains(resp.Body.String(), `"type":"error"`) ||
		!strings.Contains(resp.Body.String(), `"error":{`) {
		t.Errorf("openai surface must keep its own envelope: %s", resp.Body)
	}
}

// The root list is the OpenAI surface's: an anthropic-only vendor model would 404 there, so it is
// left off, while engines (both dialects) and openai vendors list.
func TestTheRootModelListOmitsAnthropicOnlyModels(t *testing.T) {
	f := openaiVendorFixture(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || list.Object != "list" {
		t.Fatalf("not an OpenAI list: %s", w.Body)
	}
	ids := map[string]bool{}
	for _, m := range list.Data {
		ids[m.ID] = true
	}
	if !ids["qwen3-4b"] || !ids["deepseek/chat"] {
		t.Errorf("engine and openai-vendor models must both list: %v", ids)
	}
	if ids["anthropic/claude-4-5"] {
		t.Error("an anthropic-only vendor model was advertised on the surface it 404s on")
	}
}

// The Anthropic model list refuses in Anthropic's shape, like every other answer under the prefix.
func TestAnAnthropicModelListRefusalSpeaksAnthropic(t *testing.T) {
	f := providerFixtureAnswering(t, jsonEngine(anthropicMessage))
	r := httptest.NewRequest(http.MethodGet, "/anthropic/v1/models", nil)
	r.Header.Set("x-api-key", "not-a-key")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"type":"error"`) {
		t.Errorf("refusal under /anthropic got the OpenAI envelope: %s", w.Body)
	}
}
