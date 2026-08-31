package http

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func metricsFixture(t *testing.T) (*metricsProxy, *string) {
	t.Helper()
	var seenPath string
	exporter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(exporter.Close)

	hash, err := bcrypt.GenerateFromPassword([]byte("scrape-pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	htpasswd := filepath.Join(t.TempDir(), "metrics.htpasswd")
	if err := os.WriteFile(htpasswd, []byte("grove:"+string(hash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy, err := newMetricsProxy(htpasswd, exporter.URL+"/metrics")
	if err != nil {
		t.Fatalf("newMetricsProxy: %v", err)
	}
	return proxy, &seenPath
}

// The scrape is mounted at /metrics/node, but that path is the gateway's own — the exporter
// serves exactly the endpoint the target URL names, and forwarding our mount path 404s it.
func TestTheScrapeReachesTheExportersOwnPath(t *testing.T) {
	proxy, seenPath := metricsFixture(t)

	r := httptest.NewRequest(http.MethodGet, "/metrics/node", nil)
	r.SetBasicAuth("grove", "scrape-pw")
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, exporter saw path %q", w.Code, *seenPath)
	}
	if *seenPath != "/metrics" {
		t.Errorf("exporter saw %q, want /metrics", *seenPath)
	}
}

func TestAWrongScrapeCredentialIs401(t *testing.T) {
	proxy, seenPath := metricsFixture(t)

	for name, set := range map[string]func(*http.Request){
		"wrong password": func(r *http.Request) { r.SetBasicAuth("grove", "nope") },
		"wrong user":     func(r *http.Request) { r.SetBasicAuth("prometheus", "scrape-pw") },
		"no auth":        func(r *http.Request) {},
	} {
		r := httptest.NewRequest(http.MethodGet, "/metrics/node", nil)
		set(r)
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, w.Code)
		}
	}
	if *seenPath != "" {
		t.Errorf("exporter was reached without a valid credential (path %q)", *seenPath)
	}
}
