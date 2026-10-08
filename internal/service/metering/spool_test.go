package metering

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository/memory"
)

func spoolFixture(t *testing.T, maxBytes int64) (*memory.Store, *Service, string) {
	t.Helper()
	store := memory.New()
	store.Keys["k1"] = domain.KeyRecord{Prepaid: true, Budget: 1_000}
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	svc := New(store.Repositories().Usage, store.Repositories().Limits, store.Repositories().Health, quiet())
	svc.Spool = NewSpool(store.Repositories().Usage, quiet(), func() string { return path }, func() int64 { return maxBytes })
	return store, svc, path
}

func record(t *testing.T, svc *Service, id string) {
	svc.Record(context.Background(), Report{
		RequestID: id, Prefix: "K-1", Model: "m", MeterID: "k1", Prepaid: true, Budget: 1_000,
		Pricing: priced(t, "mp1", map[string]int64{"request_count": 7}),
	})
}

func lines(t *testing.T, path string) int {
	t.Helper()
	got, err := readLines(path)
	if err != nil {
		t.Fatal(err)
	}
	return len(got)
}

// A request that finishes while the store is down waits on disk, and lands exactly once when it
// answers — even when a pass dies after landing it and the next pass sees it again.
func TestAFailedAccrualIsSpooledThenReplayedOnce(t *testing.T) {
	store, svc, path := spoolFixture(t, 1<<20)
	store.Fail["usage"] = true
	record(t, svc, "rid-1")
	if lines(t, path) != 1 || store.Usage["K-1"] != nil {
		t.Fatalf("spooled %d lines, usage %v: want one line and nothing landed", lines(t, path), store.Usage["K-1"])
	}
	saved, _ := os.ReadFile(path)

	delete(store.Fail, "usage")
	svc.Spool.Replay(context.Background())
	if lines(t, path) != 0 || store.Usage["K-1"]["p:mp1:cost"] != 7 || store.Keys["k1"].Spent != 7 {
		t.Fatalf("after replay: %d lines, usage %v, spent %d", lines(t, path), store.Usage["K-1"], store.Keys["k1"].Spent)
	}
	// A pass that died before rewriting the file: the line is back, the marker makes it a no-op.
	if err := os.WriteFile(path, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	svc.Spool.Replay(context.Background())
	if store.Usage["K-1"]["p:mp1:cost"] != 7 || lines(t, path) != 0 || svc.Spool.Stats().Replayed != 1 {
		t.Errorf("replayed twice: usage %v, stats %+v", store.Usage["K-1"], svc.Spool.Stats())
	}
}

func TestAStoreStillDownKeepsTheLineWithoutATry(t *testing.T) {
	store, svc, path := spoolFixture(t, 1<<20)
	store.Fail["usage"], store.Fail["ping"] = true, true
	record(t, svc, "rid-1")
	for range maxTries + 1 {
		svc.Spool.Replay(context.Background())
	}
	if lines(t, path) != 1 || len(svc.Spool.Dead()) != 0 {
		t.Errorf("a down store must not count tries: %d lines, dead %v", lines(t, path), svc.Spool.Dead())
	}
}

// A line the answering store keeps refusing is set aside, for the pull to hand over, and an ack
// forgets it.
func TestALineRefusedFiveTimesIsDeadUntilAcked(t *testing.T) {
	store, svc, path := spoolFixture(t, 1<<20)
	store.Fail["usage"] = true
	record(t, svc, "rid-1")
	for i := range maxTries {
		if len(svc.Spool.Dead()) != 0 {
			t.Fatalf("dead after %d tries, want %d", i, maxTries)
		}
		svc.Spool.Replay(context.Background())
	}
	dead := svc.Spool.Dead()
	if len(dead) != 1 || dead[0].ID != "rid-1" || dead[0].Line == "" || lines(t, path) != 0 || svc.Spool.Stats().Dead != 1 {
		t.Fatalf("dead = %+v, spool %d lines, stats %+v", dead, lines(t, path), svc.Spool.Stats())
	}
	if n, err := svc.Spool.Forget([]string{"rid-1", "unknown"}); err != nil || n != 1 || len(svc.Spool.Dead()) != 0 {
		t.Errorf("forget = %d, %v; dead now %v", n, err, svc.Spool.Dead())
	}
}

func TestAnUnreadableLineIsDeadAtOnce(t *testing.T) {
	_, svc, path := spoolFixture(t, 1<<20)
	if err := appendLine(path, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	svc.Spool.Replay(context.Background())
	if dead := svc.Spool.Dead(); len(dead) != 1 || dead[0].Line != "{not json" || lines(t, path) != 0 {
		t.Errorf("dead = %+v", dead)
	}
}

func TestPastTheCapUsageIsDropped(t *testing.T) {
	store, svc, path := spoolFixture(t, 10)
	store.Fail["usage"] = true
	if err := appendLine(path, []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	record(t, svc, "rid-1")
	if lines(t, path) != 1 || svc.Spool.Stats().Dropped != 1 {
		t.Errorf("%d lines, stats %+v: want the new line dropped", lines(t, path), svc.Spool.Stats())
	}
}

// The file is the spool: a fresh process over the same path replays what the last one kept.
func TestARestartReplaysFromDisk(t *testing.T) {
	store, svc, path := spoolFixture(t, 1<<20)
	store.Fail["usage"] = true
	record(t, svc, "rid-1")
	delete(store.Fail, "usage")
	fresh := NewSpool(store.Repositories().Usage, quiet(), func() string { return path }, func() int64 { return 1 << 20 })
	if fresh.Stats().Depth != 1 {
		t.Fatalf("depth = %d, want the line the last process spooled", fresh.Stats().Depth)
	}
	fresh.Replay(context.Background())
	if store.Usage["K-1"]["request_count"] != 1 || fresh.Stats().Depth != 0 {
		t.Errorf("usage %v, depth %d", store.Usage["K-1"], fresh.Stats().Depth)
	}
}
