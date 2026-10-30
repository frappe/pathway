package http

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/frappe/pathway/internal/config"
	"github.com/frappe/pathway/internal/observability"
	"github.com/frappe/pathway/internal/repository/memory"
	"github.com/frappe/pathway/internal/service/provisioning"
)

// /healthz says how long since the control plane last spoke to this box, and nothing until it has:
// a call with the wrong token is not the control plane. The status is never that header's to change.
func TestHealthSaysHowLongSinceTheControlPlaneSpoke(t *testing.T) {
	logs := observability.Discard()
	server := New(config.Config{AdminToken: "admin-token"}, Services{
		Provisioning: provisioning.New(memory.New().Repositories(), logs.Process),
	}, logs.Process)
	health := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		server.Health(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return w
	}

	if w := health(); w.Code != http.StatusOK || w.Header().Get("X-Grove-Contact-Age") != "" {
		t.Errorf("fresh healthz = %d, age = %q; want 200 and no age before any contact", w.Code, w.Header().Get("X-Grove-Contact-Age"))
	}

	r := httptest.NewRequest(http.MethodGet, "/grove-admin/state-hash", nil)
	r.Header.Set("X-Grove-Admin-Token", "wrong")
	refused := httptest.NewRecorder()
	server.AdminHandler().ServeHTTP(refused, r)
	if refused.Code != http.StatusForbidden {
		t.Fatalf("wrong token = %d, want 403", refused.Code)
	}
	if age := health().Header().Get("X-Grove-Contact-Age"); age != "" {
		t.Errorf("age = %q after a refused call; only the control plane counts", age)
	}

	if w := adminCall(t, server.AdminHandler(), http.MethodGet, "/grove-admin/state-hash", ""); w.Code != http.StatusOK {
		t.Fatalf("state-hash = %d", w.Code)
	}
	w := health()
	if age, err := strconv.Atoi(w.Header().Get("X-Grove-Contact-Age")); w.Code != http.StatusOK || err != nil || age < 0 || age > 60 {
		t.Errorf("healthz = %d, age = %q (%v); want 200 and seconds since the call", w.Code, w.Header().Get("X-Grove-Contact-Age"), err)
	}
}
