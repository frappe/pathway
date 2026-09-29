package http

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/phot0n/pathway/internal/config"
	"github.com/phot0n/pathway/internal/domain"
)

// providerFixture points one model at a third-party route. The fake engine stands in for the
// vendor: what matters is the request as it leaves us, not who answers it.
func providerFixture(t *testing.T) *fixture {
	t.Helper()
	return providerFixtureAnswering(t, jsonEngine(`{`+usageObject+`}`))
}

func providerFixtureAnswering(t *testing.T, engineHandler http.HandlerFunc) *fixture {
	t.Helper()
	f := newFixture(t, engineHandler)
	f.store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b,anthropic/claude-4-5")}
	f.store.Routes["anthropic/claude-4-5"] = []domain.Route{{
		EngineURL: f.engine.URL, InternalKey: "vendor-key", Healthy: true,
		Deployment: "anthropic", Server: "anthropic", Kind: "provider", Dialect: "anthropic",
		UpstreamModel: "claude-sonnet-4-5-20250929", APIVersion: "2023-06-01",
	}}
	return f
}

// A vendor authenticates its own way. Leaving our Bearer in place would hand it a credential it
// has no business seeing, on top of failing to authenticate.
func TestAProviderRouteGetsTheVendorsOwnCredential(t *testing.T) {
	f := providerFixture(t)
	f.post("/anthropic/v1/messages", `{"model":"anthropic/claude-4-5","max_tokens":16}`)

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
	f.post("/anthropic/v1/messages", `{"model":"anthropic/claude-4-5","max_tokens":16}`)

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
	f.post("/anthropic/v1/messages", `{"model":"anthropic/claude-4-5"}`)

	usage := f.store.Usage["abc123"]
	if usage["m:total_tokens:anthropic/claude-4-5"] == 0 {
		t.Errorf("no usage against the Grove id; usage = %v", usage)
	}
	if usage["m:total_tokens:claude-sonnet-4-5-20250929"] != 0 {
		t.Errorf("usage was recorded against the vendor's id; usage = %v", usage)
	}
}

// modelmap in reverse: the vendor answers under its own spelling, and the client gets back the id
// it asked for — the rewrite must not be visible from outside.
func TestAProviderResponseSpeaksTheGroveModelID(t *testing.T) {
	f := providerFixtureAnswering(t, jsonEngine(`{"id":"msg_1","model":"claude-sonnet-4-5-20250929",`+usageObject+`}`))
	resp := f.post("/anthropic/v1/messages", `{"model":"anthropic/claude-4-5","max_tokens":16}`)

	if !strings.Contains(resp.Body.String(), `"model":"anthropic/claude-4-5"`) ||
		strings.Contains(resp.Body.String(), "claude-sonnet") {
		t.Errorf("vendor spelling reached the client: %s", resp.Body)
	}
}

// The same swap on a stream, frame by frame, with everything else byte-identical and the usage
// frame still captured off the raw bytes underneath the rewrite.
func TestAProviderStreamSpeaksTheGroveModelIDInEveryFrame(t *testing.T) {
	frames := []string{
		`data: {"type":"message_start","message":{"model":"claude-sonnet-4-5-20250929"}}`,
		`data: {"type":"content_block_delta","delta":{"text":"Hi"}}`,
		`data: {"type":"message_delta","model":"claude-sonnet-4-5-20250929",` + usageObject + `}`,
	}
	f := providerFixtureAnswering(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame+"\n\n")
			w.(http.Flusher).Flush()
		}
	})
	resp := f.post("/anthropic/v1/messages", `{"model":"anthropic/claude-4-5","stream":true}`)

	want := strings.ReplaceAll(strings.Join(frames, "\n\n")+"\n\n",
		"claude-sonnet-4-5-20250929", "anthropic/claude-4-5")
	if resp.Body.String() != want {
		t.Errorf("stream altered beyond the model swap.\n got: %q\nwant: %q", resp.Body.String(), want)
	}
	if f.store.Usage["abc123"]["m:total_tokens:anthropic/claude-4-5"] == 0 {
		t.Errorf("usage lost under the swap: %v", f.store.Usage["abc123"])
	}
}

// The point of gating locally: a vendor's surfaces are a closed set, so dialling anything
// outside its dialect buys a 404 we could have given ourselves, plus a round trip on a paid link.
func TestAPathTheVendorDoesNotServeNeverLeavesTheGateway(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/embeddings", "/v1/rerank", "/v1/completions"} {
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

// One provider record, two fronts: each surface's requests dial the front that speaks it, and a
// path neither front serves still never leaves the gateway.
func TestADualFrontVendorRoutesEachSurfaceToItsOwnFront(t *testing.T) {
	f := newFixture(t, jsonEngine(`{`+usageObject+`}`))
	f.store.Groups["acme"] = domain.GroupRecord{Models: domain.ModelSet("qwen3-4b,kimi/k2")}
	f.store.Routes["kimi/k2"] = []domain.Route{
		{EngineURL: f.engine.URL + "/root", InternalKey: "kimi-key", Healthy: true,
			Deployment: "kimi", Server: "kimi", Kind: "provider", Dialect: "openai", UpstreamModel: "k2"},
		{EngineURL: f.engine.URL + "/anthropic", InternalKey: "kimi-key", Healthy: true,
			Deployment: "kimi", Server: "kimi", Kind: "provider", Dialect: "anthropic", UpstreamModel: "k2"},
	}

	if resp := f.post("/anthropic/v1/messages", `{"model":"kimi/k2"}`); resp.Code != http.StatusOK {
		t.Fatalf("anthropic surface: status = %d, body = %s", resp.Code, resp.Body)
	}
	if f.seen.path != "/anthropic/v1/messages" {
		t.Errorf("anthropic surface dialled %q, want the anthropic front", f.seen.path)
	}

	if resp := f.post("/v1/chat/completions", `{"model":"kimi/k2"}`); resp.Code != http.StatusOK {
		t.Fatalf("openai surface: status = %d, body = %s", resp.Code, resp.Body)
	}
	if f.seen.path != "/root/v1/chat/completions" {
		t.Errorf("openai surface dialled %q, want the root front", f.seen.path)
	}

	*f.seen = engineRecord{}
	if resp := f.post("/v1/embeddings", `{"model":"kimi/k2"}`); resp.Code != http.StatusNotFound {
		t.Errorf("embeddings: status = %d, want 404", resp.Code)
	}
	if f.seen.path != "" {
		t.Errorf("a path no front serves was dialled anyway: %s", f.seen.path)
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

// The request id is ours on both sides of a vendor hop. Ours never leaves our network — a vendor
// ignores it and mints its own — and the vendor's never reaches the client: ReverseProxy would add
// it as a second X-Request-Id, or hand an Anthropic SDK the vendor's request-id in place of ours.
// It is kept on the access line instead, as the key a ticket to the vendor quotes.
func TestAProviderRequestIDStaysOurs(t *testing.T) {
	f := providerFixtureAnswering(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req_vendor")
		w.Header().Set("Request-Id", "req_vendor")
		jsonEngine(`{`+usageObject+`}`)(w, r)
	})
	access := &bytes.Buffer{}
	f.handler = buildHandler(t, f.store, config.Config{}, 0, func(s *Services) {
		s.Access = slog.New(slog.NewJSONHandler(access, nil))
	})
	resp := f.post("/anthropic/v1/messages", `{"model":"anthropic/claude-4-5","max_tokens":16}`)

	if f.seen.requestID != "" {
		t.Errorf("our request id reached the vendor: %q", f.seen.requestID)
	}
	ours := resp.Header().Values("X-Request-Id")
	if len(ours) != 1 || uuid.Validate(ours[0]) != nil || ours[0] == "req_vendor" {
		t.Errorf("X-Request-Id = %v, want exactly our one id", ours)
	}
	if got := resp.Header().Values("Request-Id"); len(got) != 1 || got[0] != ours[0] {
		t.Errorf("Request-Id = %v, want ours (%v) for the Anthropic SDK", got, ours)
	}
	line := jsonLine(t, access)
	if line["rid"] != ours[0] || line["upstream_rid"] != "req_vendor" {
		t.Errorf("access line rid=%v upstream_rid=%v, want ours + the vendor's", line["rid"], line["upstream_rid"])
	}
}
