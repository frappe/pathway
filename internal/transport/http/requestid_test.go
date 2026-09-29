package http

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/phot0n/pathway/internal/config"
)

// ridHeaders returns the id a response carries, failing unless both names carry the same one.
func ridHeaders(t *testing.T, h http.Header) string {
	t.Helper()
	x, r := h.Values("X-Request-Id"), h.Values("Request-Id")
	if len(x) != 1 || len(r) != 1 || x[0] != r[0] || uuid.Validate(x[0]) != nil {
		t.Fatalf("X-Request-Id=%v Request-Id=%v, want one uuid under both names", x, r)
	}
	return x[0]
}

// Every response carries an id — a refusal, a health probe and the model list included — because it
// is minted where the access line is, before any stage can say no.
func TestEveryResponseCarriesARequestID(t *testing.T) {
	f := newFixture(t, jsonEngine(`{`+usageObject+`}`))

	denied := f.post("/v1/chat/completions", `{"model":"qwen3-4b"}`, "Authorization", "Bearer wrong")
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("bad key = %d, want 401", denied.Code)
	}
	ridHeaders(t, denied.Header())

	models := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	f.handler.ServeHTTP(models, r)
	if models.Code != http.StatusOK {
		t.Fatalf("models = %d %s", models.Code, models.Body)
	}
	ridHeaders(t, models.Header())

	listener, err := newServer(t, f.store, config.Config{}, 0).Handlers(config.Config{}, nil)
	if err != nil {
		t.Fatalf("Handlers: %v", err)
	}
	health := httptest.NewRecorder()
	listener.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("healthz = %d %s", health.Code, health.Body)
	}
	ridHeaders(t, health.Header())
}

// An ingress carries the gateway's id rather than minting its own: its access line, its answer and
// the engine all see the one id the gateway logged. An engine's echo of it is not a vendor's id.
func TestAnIngressAdoptsTheGatewaysRequestID(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", r.Header.Get("X-Request-Id"))
		jsonEngine(`{`+usageObject+`}`)(w, r)
	})
	access := &bytes.Buffer{}
	f.handler = buildHandler(t, f.store, config.Config{IngressID: "ing-test", IngressToken: "ing-token"}, 0,
		func(s *Services) { s.Access = slog.New(slog.NewJSONHandler(access, nil)) })

	resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b"}`,
		"Authorization", "Bearer ing-token", "X-Grove-Model", "qwen3-4b", "X-Request-Id", "01999999-0000-7000-8000-000000000001")
	if resp.Code != http.StatusOK {
		t.Fatalf("ingress = %d %s", resp.Code, resp.Body)
	}
	if f.seen.requestID != "01999999-0000-7000-8000-000000000001" {
		t.Errorf("engine saw %q, want the gateway's id", f.seen.requestID)
	}
	if got := ridHeaders(t, resp.Header()); got != "01999999-0000-7000-8000-000000000001" {
		t.Errorf("ingress answered with %q, want the gateway's id", got)
	}
	line := jsonLine(t, access)
	if line["rid"] != "01999999-0000-7000-8000-000000000001" || line["upstream_rid"] != "-" {
		t.Errorf("access line rid=%v upstream_rid=%v, want the gateway's id and no vendor id", line["rid"], line["upstream_rid"])
	}
}
