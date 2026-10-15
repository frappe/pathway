package http

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/frappe/pathway/internal/domain"
	"github.com/frappe/pathway/internal/repository"
	"github.com/frappe/pathway/internal/repository/memory"
)

// The pull as the control plane runs it: every unacknowledged bucket comes back grouped by drain
// id, an ack names the buckets it recorded and keeps them for the retention window, an unacked one
// keeps coming back, and ?keys= names prefixes, which match their buckets.
func TestTheDrainAckCycle(t *testing.T) {
	store := memory.New()
	day := domain.DayOf(time.Now())
	k1 := repository.UsageBucket("K-1", time.Now())
	store.Usage[k1] = map[string]int64{"request_count": 3}
	store.Usage["K-2"] = map[string]int64{"request_count": 1} // from before buckets carried a day
	handler := adminFixture(t, store)

	get := func(path string) repository.Drains {
		w := adminCall(t, handler, http.MethodGet, path, "")
		var p struct {
			Drains repository.Drains `json:"drains"`
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
	if first[id]["K-1"][day]["request_count"] != "3" || first[id]["K-2"][""]["request_count"] != "1" {
		t.Fatalf("first = %+v", first)
	}
	w := adminCall(t, handler, http.MethodPost, "/grove-admin/usage/ack", `{"acks": {"`+id+`": {"K-1": ["`+day+`"]}}}`)
	var ack struct {
		OK    bool `json:"ok"`
		Count int  `json:"count"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &ack) != nil || !ack.OK || ack.Count != 1 {
		t.Fatalf("ack = %d: %s", w.Code, w.Body)
	}
	if store.Retained[id+":"+k1] != 3*24*time.Hour {
		t.Errorf("retained %v, want the default three days", store.Retained[id+":"+k1])
	}
	if after := get("/grove-admin/usage"); len(after) != 1 || after[id]["K-2"] == nil || after[id]["K-1"] != nil {
		t.Errorf("after ack = %+v, want only the unacked K-2 again", after)
	}
	if mine := get("/grove-admin/usage?keys=K-1"); len(mine) != 0 {
		t.Errorf("K-1 alone = %+v, want nothing: it was acked and nothing new was metered", mine)
	}
	// A retried ack is harmless.
	if w := adminCall(t, handler, http.MethodPost, "/grove-admin/usage/ack", `{"acks": {"`+id+`": {"K-1": ["`+day+`"]}}}`); w.Code != http.StatusOK {
		t.Errorf("retried ack = %d", w.Code)
	}
}

// A peek is the pull's answer without the pull: the control plane reads what the box holds for a
// check it makes now, and nothing is set aside, so the next pull finds it all.
func TestAPeekMovesNothing(t *testing.T) {
	store := memory.New()
	day := domain.DayOf(time.Now())
	store.Usage[repository.UsageBucket("K-1", time.Now())] = map[string]int64{"request_count": 3, "cost": 300}
	store.Usage[repository.UsageBucket("K-2", time.Now())] = map[string]int64{"request_count": 1}
	handler := adminFixture(t, store)
	get := func(path string) repository.Drains {
		w := adminCall(t, handler, http.MethodGet, path, "")
		var p struct {
			Drains repository.Drains `json:"drains"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body)
		}
		return p.Drains
	}
	// K-2 set aside once and never acked: a peek shows it under its drain id, K-1 live under "".
	drained := get("/grove-admin/usage?keys=K-2")
	var id string
	for id = range drained {
	}
	peek := get("/grove-admin/usage?keys=K-1,K-2&peek=1")
	if peek[""]["K-1"][day]["cost"] != "300" || peek[id]["K-2"][day]["request_count"] != "1" || len(peek) != 2 {
		t.Fatalf("peek = %+v, want K-1 live and K-2 under %s", peek, id)
	}
	if alone := get("/grove-admin/usage?keys=K-1&peek=1"); len(alone) != 1 || alone[""]["K-1"] == nil {
		t.Errorf("K-1 alone = %+v", alone)
	}
	if len(store.Usage) != 1 || len(store.Unacked) != 1 {
		t.Errorf("after the peeks: live %v, unacked %v — a peek moves nothing", store.Usage, store.Unacked)
	}
	if pull := get("/grove-admin/usage"); pull[id]["K-2"] == nil || len(pull) != 2 {
		t.Errorf("the pull after a peek = %+v, want K-2 still waiting and K-1 set aside now", pull)
	}
}

// A spend correction applies once per id, and names a key this store must already have.
func TestSpendAdjust(t *testing.T) {
	store := memory.New()
	store.Keys["aa"] = domain.KeyRecord{Spent: 3_000}
	handler := adminFixture(t, store)
	body := `{"key": "aa", "delta": -1000, "id": "CD-1"}`

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
	if w := adminCall(t, handler, http.MethodPost, "/grove-admin/spend-adjust", `{"key": "ghost", "delta": 5, "id": "CD-2"}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown key = %d, want 404", w.Code)
	}
	if w := adminCall(t, handler, http.MethodPost, "/grove-admin/spend-adjust", `{"key": "aa", "delta": 5}`); w.Code != http.StatusBadRequest {
		t.Errorf("no id = %d, want 400", w.Code)
	}
}
