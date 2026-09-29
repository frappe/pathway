package http

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository/memory"
)

// The pull as the control plane runs it: every unacknowledged key comes back grouped by drain id,
// an ack keeps the named keys for the retention window, and an unacked key keeps coming back.
func TestTheDrainAckCycle(t *testing.T) {
	store := memory.New()
	store.Usage["K-1"] = map[string]int64{"request_count": 3}
	store.Usage["K-2"] = map[string]int64{"request_count": 1}
	handler := adminFixture(t, store)

	get := func(path string) map[string]map[string]map[string]string {
		w := adminCall(t, handler, http.MethodGet, path, "")
		var p struct {
			Drains map[string]map[string]map[string]string `json:"drains"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Drains == nil {
			t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body)
		}
		return p.Drains
	}
	first := get("/grove-admin/usage")
	if len(first) != 1 {
		t.Fatalf("first = %+v, want one drain", first)
	}
	var id string
	for id = range first {
	}
	if first[id]["K-1"]["request_count"] != "3" || first[id]["K-2"]["request_count"] != "1" {
		t.Fatalf("first = %+v", first)
	}
	w := adminCall(t, handler, http.MethodPost, "/grove-admin/usage/ack", `{"acks": {"`+id+`": ["K-1"]}}`)
	var ack struct {
		OK    bool `json:"ok"`
		Count int  `json:"count"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &ack) != nil || !ack.OK || ack.Count != 1 {
		t.Fatalf("ack = %d: %s", w.Code, w.Body)
	}
	if store.Retained[id+":K-1"] != 7*24*time.Hour {
		t.Errorf("retained %v, want the default week", store.Retained[id+":K-1"])
	}
	if after := get("/grove-admin/usage"); len(after) != 1 || after[id]["K-2"] == nil || after[id]["K-1"] != nil {
		t.Errorf("after ack = %+v, want only the unacked K-2 again", after)
	}
	if mine := get("/grove-admin/usage?keys=K-1"); len(mine) != 0 {
		t.Errorf("K-1 alone = %+v, want nothing: it was acked and nothing new was metered", mine)
	}
	// A retried ack is harmless.
	if w := adminCall(t, handler, http.MethodPost, "/grove-admin/usage/ack", `{"acks": {"`+id+`": ["K-1"]}}`); w.Code != http.StatusOK {
		t.Errorf("retried ack = %d", w.Code)
	}
}

// A spend correction applies once per id, and names a holder this store must already have.
func TestSpendAdjust(t *testing.T) {
	store := memory.New()
	store.Users["GU-1"] = domain.UserRecord{Spent: 3_000}
	handler := adminFixture(t, store)
	body := `{"user": "GU-1", "delta": -1000, "id": "CD-1"}`

	for i, want := range []struct {
		spent   int64
		applied bool
	}{{2_000, true}, {2_000, false}} {
		w := adminCall(t, handler, http.MethodPost, "/grove-admin/spend-adjust", body)
		var reply struct {
			Spent   int64 `json:"spent"`
			Applied bool  `json:"applied"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &reply) != nil {
			t.Fatalf("call %d = %d: %s", i, w.Code, w.Body)
		}
		if reply.Spent != want.spent || reply.Applied != want.applied {
			t.Errorf("call %d = %+v, want %+v", i, reply, want)
		}
	}
	if w := adminCall(t, handler, http.MethodPost, "/grove-admin/spend-adjust", `{"user": "GU-ghost", "delta": 5, "id": "CD-2"}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown holder = %d, want 404", w.Code)
	}
	if w := adminCall(t, handler, http.MethodPost, "/grove-admin/spend-adjust", `{"user": "GU-1", "delta": 5}`); w.Code != http.StatusBadRequest {
		t.Errorf("no id = %d, want 400", w.Code)
	}
}
