package http

import (
	"bytes"
	"log/slog"
	"net/http"
	"testing"

	"github.com/phot0n/pathway/internal/config"
)

// A caller's tags land on its access line as one `meta` object and stop at the gateway, along with
// its session: neither is the upstream's to read.
func TestRequestMetadataIsLoggedAndNotRelayed(t *testing.T) {
	f := newFixture(t, jsonEngine(`{`+usageObject+`}`))
	access := &bytes.Buffer{}
	f.handler = buildHandler(t, f.store, config.Config{}, 0,
		func(s *Services) { s.Access = slog.New(slog.NewJSONHandler(access, nil)) })

	resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b"}`, "Authorization", "Bearer "+secret,
		"X-Grove-Metadata", "app=hrms, trace=7f3a-91", "X-Grove-Session", "acme-bot")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d %s", resp.Code, resp.Body)
	}
	meta, _ := jsonLine(t, access)["meta"].(map[string]any)
	if meta["app"] != "hrms" || meta["trace"] != "7f3a-91" {
		t.Errorf("meta = %v, want app and trace", meta)
	}
	for _, name := range []string{"X-Grove-Metadata", "X-Grove-Session"} {
		if got := f.seen.headers.Get(name); got != "" {
			t.Errorf("upstream saw %s: %q", name, got)
		}
	}
}

// A malformed header is refused before anything runs, and the refusal still has its line.
func TestMalformedRequestMetadataIsRefused(t *testing.T) {
	f := newFixture(t, jsonEngine(`{`+usageObject+`}`))
	access := &bytes.Buffer{}
	f.handler = buildHandler(t, f.store, config.Config{}, 0,
		func(s *Services) { s.Access = slog.New(slog.NewJSONHandler(access, nil)) })

	resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b"}`, "Authorization", "Bearer "+secret,
		"X-Grove-Metadata", "App=hrms")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.Code)
	}
	if line := jsonLine(t, access); line["status"] != float64(400) {
		t.Errorf("access line status = %v, want 400", line["status"])
	}
	if f.seen.path != "" {
		t.Error("a refused request reached the engine")
	}
}
