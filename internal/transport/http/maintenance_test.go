package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phot0n/pathway/internal/config"
	"github.com/phot0n/pathway/internal/observability"
	"github.com/phot0n/pathway/internal/repository/memory"
	"github.com/phot0n/pathway/internal/service/admission"
	"github.com/phot0n/pathway/internal/service/catalog"
	"github.com/phot0n/pathway/internal/service/metering"
	"github.com/phot0n/pathway/internal/service/provisioning"
	"github.com/phot0n/pathway/internal/service/routing"
	"github.com/phot0n/pathway/internal/service/transform"
	"github.com/phot0n/pathway/internal/transport/http/middleware"
	"github.com/phot0n/pathway/internal/transport/http/proxy"
)

// Maintenance: new requests are refused, running ones finish, and the control plane can watch the
// count reach zero. The flag is a reloadable tunable; here an atomic stands in for the file.

type maintained struct {
	on     *atomic.Bool
	front  *httptest.Server
	admin  http.Handler
	health http.HandlerFunc
}

func newMaintained(t *testing.T, store *memory.Store) *maintained {
	t.Helper()
	logs := observability.Discard()
	repos := store.Repositories()
	transforms, _ := transform.NewChain(transform.Default)
	on := new(atomic.Bool)
	server := New(config.Config{AdminToken: "admin-token"}, Services{
		Admission:    admission.New(repos.Keys, repos.Groups, repos.Limits),
		Routing:      routing.New(repos, logs.Process, routing.Options{}),
		Metering:     metering.New(repos.Usage, repos.Limits, repos.Health, logs.Process),
		Catalog:      catalog.New(repos.Routes),
		Provisioning: provisioning.New(repos, logs.Process),
		Transform:    transforms,
		Proxy:        proxy.New(proxy.Options{}, logs.Process),
		Access:       logs.Access,
		Maintenance:  on.Load,
	}, logs.Process)
	data, err := server.DataHandler(middleware.GatewayChain)
	if err != nil {
		t.Fatalf("DataHandler: %v", err)
	}
	front := httptest.NewServer(data)
	t.Cleanup(front.Close)
	return &maintained{on: on, front: front, admin: server.AdminHandler(), health: server.Health}
}

func (m *maintained) inFlight(t *testing.T) (maintenance bool, count int64) {
	t.Helper()
	w := adminCall(t, m.admin, http.MethodGet, "/grove-admin/in-flight", "")
	if w.Code != http.StatusOK {
		t.Fatalf("in-flight = %d: %s", w.Code, w.Body)
	}
	var body struct {
		Maintenance bool  `json:"maintenance"`
		InFlight    int64 `json:"in_flight"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Maintenance, body.InFlight
}

func (m *maintained) waitFor(t *testing.T, want int64) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if _, got := m.inFlight(t); got == want {
			return
		}
	}
	_, got := m.inFlight(t)
	t.Fatalf("in_flight = %d, want %d", got, want)
}

func (m *maintained) request(ctx context.Context) (*http.Response, error) {
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, m.front.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3-4b"}`))
	r.Header.Set("Authorization", "Bearer "+secret)
	return http.DefaultClient.Do(r)
}

// A held engine, so a request can be kept in flight for as long as a test needs.
func heldEngine(release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}
}

func TestMaintenanceRefusesNewRequestsAndFailsHealth(t *testing.T) {
	f := newFixture(t, jsonEngine(`{}`))
	m := newMaintained(t, f.store)
	m.on.Store(true)

	resp, err := m.request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "30" {
		t.Errorf("status = %d, Retry-After = %q, want 503 / 30", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if !strings.Contains(string(body), `"type":"maintenance"`) {
		t.Errorf("body = %s, want type maintenance", body)
	}
	if f.seen.path != "" {
		t.Error("a request reached the engine during maintenance")
	}

	w := httptest.NewRecorder()
	m.health(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "maintenance") {
		t.Errorf("healthz = %d %s, want 503 maintenance", w.Code, w.Body)
	}
	// The routes the mux answers itself refuse too — the whole customer surface, not just /v1/.
	for _, path := range []string{"/", "/v1/models", "/anthropic/v1/models"} {
		resp, err := http.Get(m.front.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d during maintenance, want 503", path, resp.StatusCode)
		}
	}
	// Under /anthropic the refusal speaks Anthropic, so Claude Code shows the message.
	for _, path := range []string{"/anthropic/v1/models", "/anthropic/v1/messages"} {
		r, _ := http.NewRequest(http.MethodPost, m.front.URL+path, strings.NewReader(`{"model":"qwen3-4b"}`))
		if strings.HasSuffix(path, "models") {
			r.Method = http.MethodGet
		}
		r.Header.Set("x-api-key", secret)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), `"type":"error"`) ||
			!strings.Contains(string(body), `"type":"maintenance"`) {
			t.Errorf("%s = %d %s, want an Anthropic-shaped maintenance 503", path, resp.StatusCode, body)
		}
	}
	if on, count := m.inFlight(t); !on || count != 0 {
		t.Errorf("in-flight = %t/%d, want true/0 — a refusal is not in flight", on, count)
	}

	m.on.Store(false)
	resp, err = m.request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("after maintenance ended: status = %d, want 200", resp.StatusCode)
	}
	if resp, err = http.Get(m.front.URL + "/"); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("after maintenance ended: GET / = %d, want 200", resp.StatusCode)
	}
}

// The point of maintenance over a restart: what is already running is left to finish.
func TestARunningRequestFinishesWhileNewOnesAreRefused(t *testing.T) {
	release := make(chan struct{})
	f := newFixture(t, heldEngine(release))
	m := newMaintained(t, f.store)

	done := make(chan int, 1)
	go func() {
		resp, err := m.request(context.Background())
		if err != nil {
			done <- 0
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	m.waitFor(t, 1)

	m.on.Store(true)
	resp, err := m.request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("new request = %d, want 503", resp.StatusCode)
	}
	if _, count := m.inFlight(t); count != 1 {
		t.Errorf("in_flight = %d, want the running request only", count)
	}

	close(release)
	if status := <-done; status != http.StatusOK {
		t.Errorf("the running request ended %d, want 200", status)
	}
	m.waitFor(t, 0)
}

// A client that hangs up is no longer in flight, or the wait for zero never ends.
func TestAHangupLeavesTheCount(t *testing.T) {
	f := newFixture(t, heldEngine(make(chan struct{})))
	m := newMaintained(t, f.store)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = m.request(ctx) }()
	m.waitFor(t, 1)
	cancel()
	m.waitFor(t, 0)
}

// A realtime session is in flight until it closes, which is when its usage is metered.
func TestARealtimeSessionIsInFlightUntilItCloses(t *testing.T) {
	engine := echoEngine(t)
	m := newMaintained(t, realtimeStore(t, engine.URL, "transcription"))

	resp, _, conn := upgrade(t, m.front, "/v1/realtime?model=nemotron-asr")
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	m.waitFor(t, 1)
	conn.Close()
	m.waitFor(t, 0)
}

func TestInFlightNeedsTheAdminToken(t *testing.T) {
	m := newMaintained(t, memory.New())
	r := httptest.NewRequest(http.MethodGet, "/grove-admin/in-flight", nil)
	w := httptest.NewRecorder()
	m.admin.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}
