package http

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/phot0n/pathway/internal/domain"
)

// providerFixture points one model at a third-party route. The fake engine stands in for the
// vendor: what matters is the request as it leaves us, not who answers it.
func providerFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, jsonEngine(`{`+usageObject+`}`))
	f.store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b,anthropic/claude-4-5")}
	f.store.Routes["anthropic/claude-4-5"] = []domain.Route{{
		EngineURL: f.engine.URL, InternalKey: "vendor-key", Healthy: true,
		Deployment: "anthropic", Server: "anthropic", Kind: "provider",
		UpstreamModel: "claude-sonnet-4-5-20250929", APIVersion: "2023-06-01",
	}}
	return f
}

// A vendor authenticates its own way. Leaving our Bearer in place would hand it a credential it
// has no business seeing, on top of failing to authenticate.
func TestAProviderRouteGetsTheVendorsOwnCredential(t *testing.T) {
	f := providerFixture(t)
	f.post("/v1/messages", `{"model":"anthropic/claude-4-5","max_tokens":16}`)

	if f.seen.apiKey != "vendor-key" {
		t.Errorf("x-api-key = %q, want the route's own key", f.seen.apiKey)
	}
	if f.seen.authorization != "" {
		t.Errorf("Authorization = %q — no Bearer may reach a vendor", f.seen.authorization)
	}
	if f.seen.apiVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q", f.seen.apiVersion)
	}
	if f.seen.groveModel != "" || f.seen.groveSession != "" {
		t.Errorf("our own headers leaked to a vendor: model=%q session=%q", f.seen.groveModel, f.seen.groveSession)
	}
}

// The customer's id is ours; the vendor answers to a dated snapshot it has never heard us call
// anything else.
func TestAProviderRouteSendsTheUpstreamsOwnModelID(t *testing.T) {
	f := providerFixture(t)
	f.post("/v1/messages", `{"model":"anthropic/claude-4-5","max_tokens":16}`)

	var body map[string]any
	if err := json.Unmarshal(f.seen.body, &body); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if body["model"] != "claude-sonnet-4-5-20250929" {
		t.Errorf("upstream model = %v; body = %s", body["model"], f.seen.body)
	}
	if body["max_tokens"] != float64(16) {
		t.Errorf("the rest of the body did not survive the rewrite: %s", f.seen.body)
	}
}

// Metering runs above the rewrite, so the tokens land against the id the customer was granted and
// is billed on — not the vendor's spelling, which no Model row would match.
func TestProviderUsageIsRecordedAgainstTheGroveID(t *testing.T) {
	f := providerFixture(t)
	f.post("/v1/messages", `{"model":"anthropic/claude-4-5"}`)

	usage := f.store.Usage["abc123"]
	if usage["m:total_tokens:anthropic/claude-4-5"] == 0 {
		t.Errorf("no usage against the Grove id; usage = %v", usage)
	}
	if usage["m:total_tokens:claude-sonnet-4-5-20250929"] != 0 {
		t.Errorf("usage was recorded against the vendor's id; usage = %v", usage)
	}
}

// The point of gating locally: Anthropic has no /v1/embeddings and no /v1/rerank, so dialling
// either buys a 404 we could have given ourselves — plus a round trip on a paid link.
func TestAPathTheVendorDoesNotServeNeverLeavesTheGateway(t *testing.T) {
	for _, path := range []string{"/v1/embeddings", "/v1/rerank", "/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			f := providerFixture(t)
			resp := f.post(path, `{"model":"anthropic/claude-4-5"}`)

			if resp.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", resp.Code)
			}
			if f.seen.path != "" {
				t.Errorf("the vendor was dialled anyway: %s", f.seen.path)
			}
		})
	}
}

// An engine is the opposite case: it serves more than the OpenAI core, and the vendor allowlist
// must not have taken those away.
func TestAnEngineStillServesItsOwnExtraEndpoints(t *testing.T) {
	f := newFixture(t, jsonEngine(`{`+usageObject+`}`))
	if resp := f.post("/v1/rerank", `{"model":"qwen3-4b"}`); resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}
	if f.seen.path != "/e/md1/v1/rerank" {
		t.Errorf("engine path = %q", f.seen.path)
	}
}
