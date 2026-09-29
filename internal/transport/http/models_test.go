package http

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phot0n/pathway/internal/config"
	"github.com/phot0n/pathway/internal/observability"
	"github.com/phot0n/pathway/internal/service/admission"
	"github.com/phot0n/pathway/internal/service/catalog"
	"github.com/phot0n/pathway/internal/service/metering"
	"github.com/phot0n/pathway/internal/service/provisioning"
	"github.com/phot0n/pathway/internal/service/routing"
	"github.com/phot0n/pathway/internal/service/transform"
	"github.com/phot0n/pathway/internal/transport/http/middleware"
	"github.com/phot0n/pathway/internal/transport/http/proxy"
)

// accessFixture rebuilds the fixture's handler with the access log going to a buffer, so a test
// can assert on the lines the server writes.
func accessFixture(t *testing.T, f *fixture) *bytes.Buffer {
	t.Helper()
	access := &bytes.Buffer{}
	logs := observability.Discard()
	repos := f.store.Repositories()
	transforms, err := transform.NewChain(transform.Default)
	if err != nil {
		t.Fatalf("transform chain: %v", err)
	}
	server := New(config.Config{AdminToken: "admin-token"}, Services{
		Admission:    admission.New(repos.Keys, repos.Users, repos.Groups),
		Routing:      routing.New(repos, logs.Process, routing.Options{}),
		Metering:     metering.New(repos.Usage, repos.Health, logs.Process),
		Catalog:      catalog.New(repos.Routes),
		Provisioning: provisioning.New(repos, logs.Process),
		Transform:    transforms,
		Proxy:        proxy.New(proxy.Options{}, logs.Process),
		Access:       slog.New(slog.NewJSONHandler(access, nil)),
		MaxBodyBytes: func() int64 { return 0 },
	}, logs.Process)
	handler, err := server.DataHandler(middleware.GatewayChain)
	if err != nil {
		t.Fatalf("DataHandler: %v", err)
	}
	f.handler = handler
	return access
}

func accessLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("unparseable access line %q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

// The model list is answered outside the data chain, which is exactly how it could vanish from
// the access log — this pins that it does not, and that the line names the caller's key.
func TestListingModelsLeavesAnAccessLine(t *testing.T) {
	f := newFixture(t, jsonEngine(`{}`))
	access := accessFixture(t, f)

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}

	lines := accessLines(t, access)
	if len(lines) != 1 {
		t.Fatalf("got %d access lines, want exactly 1: %v", len(lines), lines)
	}
	line := lines[0]
	if line["path"] != "/v1/models" || line["status"] != float64(200) {
		t.Errorf("access line = %v", line)
	}
	if line["key"] != "abc123" {
		t.Errorf("key = %v — the handler authenticates inline, so it must attribute the line itself", line["key"])
	}
	if _, present := line["ttft"]; present {
		t.Error("ttft on a gateway-answered line — it is an upstream measurement and would only repeat rt")
	}
}

// No key is a 401, as on the inference path: a list answered anyway hides a client that forgot its key.
func TestListingModelsWithoutAKeyIsRefused(t *testing.T) {
	f := newFixture(t, jsonEngine(`{}`))

	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", w.Code, w.Body)
	}
}

// Nothing the server answers goes unlogged — a request for a path no route claims still leaves
// its line, the way an nginx access log records every request that reached it.
func TestAnUnknownPathLeavesA404AccessLine(t *testing.T) {
	f := newFixture(t, jsonEngine(`{}`))
	access := accessFixture(t, f)

	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nowhere", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}

	lines := accessLines(t, access)
	if len(lines) != 1 || lines[0]["path"] != "/nowhere" || lines[0]["status"] != float64(404) {
		t.Errorf("lines = %v, want one 404 line for /nowhere", lines)
	}
}

// The listener wrap and the data chain both carry an accesslog stage; the inner one must step
// aside or every inference request would be billed two log lines.
func TestAChainedRequestLogsExactlyOnce(t *testing.T) {
	f := newFixture(t, jsonEngine(`{`+usageObject+`}`))
	access := accessFixture(t, f)

	resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","messages":[]}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body)
	}

	lines := accessLines(t, access)
	if len(lines) != 1 {
		t.Fatalf("got %d access lines, want exactly 1: %v", len(lines), lines)
	}
	if lines[0]["key"] != "abc123" || lines[0]["model"] != "qwen3-4b" {
		t.Errorf("line = %v — the outer accesslog must still see what the chain resolved", lines[0])
	}
	if _, present := lines[0]["ttft"]; !present {
		t.Error("no ttft on a proxied line — the one place it measures something rt does not")
	}
}

func TestOwnedByIsTheProviderPrefix(t *testing.T) {
	for id, want := range map[string]string{
		"anthropic/claude-opus-5": "anthropic",
		"frappe/qwen3-4b":         "frappe",
		"qwen3-4b":                "frappe",
		"a/b/c":                   "a",
	} {
		if got := ownerOf(id); got != want {
			t.Errorf("ownerOf(%q) = %q, want %q", id, got, want)
		}
	}
}
