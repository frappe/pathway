package http

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/phot0n/pathway/internal/config"
	"github.com/phot0n/pathway/internal/observability"
	"github.com/phot0n/pathway/internal/repository/memory"
	"github.com/phot0n/pathway/internal/service/metering"
	"github.com/phot0n/pathway/internal/service/provisioning"
)

// The pull carries the spool's dead lines and its counters; an ack naming a dead line forgets it.
func TestThePullHandsOverDeadLinesAndAnAckForgetsThem(t *testing.T) {
	store := memory.New()
	logs := observability.Discard()
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	meter := metering.New(store.Repositories().Usage, store.Repositories().Limits, store.Repositories().Health, logs.Process)
	meter.Spool = metering.NewSpool(store.Repositories().Usage, logs.Process, func() string { return path }, func() int64 { return 1 << 20 })
	store.Fail["usage"] = true
	meter.Record(context.Background(), metering.Report{RequestID: "rid-1", Prefix: "K-1", Model: "m"})
	for range 5 {
		meter.Spool.Replay(context.Background())
	}
	delete(store.Fail, "usage")
	handler := New(config.Config{AdminToken: "admin-token"}, Services{
		Provisioning: provisioning.New(store.Repositories(), logs.Process), Metering: meter,
	}, logs.Process).AdminHandler()

	w := adminCall(t, handler, http.MethodGet, "/grove-admin/usage", "")
	var pull struct {
		Dead  []metering.Dead `json:"dead"`
		Spool metering.Stats  `json:"spool"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &pull) != nil || len(pull.Dead) != 1 || pull.Dead[0].ID != "rid-1" || pull.Spool.Dead != 1 {
		t.Fatalf("pull = %d: %s", w.Code, w.Body)
	}
	w = adminCall(t, handler, http.MethodPost, "/grove-admin/usage/ack", `{"acks": {}, "dead": ["rid-1"]}`)
	var ack struct {
		Count int `json:"count"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &ack) != nil || ack.Count != 1 || len(meter.Spool.Dead()) != 0 {
		t.Errorf("ack = %d: %s, dead left %v", w.Code, w.Body, meter.Spool.Dead())
	}
}
