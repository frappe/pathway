package metering

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phot0n/pathway/internal/repository"
)

// maxTries is how many replays a line gets while the store answers before it is set aside as dead.
const maxTries = 5

// Spool keeps what the store refused while it was down. A failed Accrue is appended to a local
// file and fsynced; Run replays it once the store answers, through Replay, which lands each
// request id once — so a pass that dies half way lands nothing twice. A line the healthy store
// keeps refusing moves to <spool>.dead, which the usage pull hands to the control plane.
//
// ponytail: the whole file is read and rewritten per pass. Fine while it only fills during an
// outage; a segmented log if outages ever leave gigabytes behind.
type Spool struct {
	usage    repository.Usage
	log      *slog.Logger
	path     func() string
	maxBytes func() int64

	mu    sync.Mutex // one writer: an append never interleaves with a pass's rewrite
	stats struct{ replayed, dropped, dead atomic.Int64 }
}

// entry is one spooled accrual as it sits on disk.
type entry struct {
	repository.Accrual
	At    time.Time `json:"at"`
	Tries int       `json:"tries,omitempty"`
}

// Dead is one line the store kept refusing, as the usage pull reports it.
type Dead struct {
	ID    string `json:"id"`
	Line  string `json:"line"`
	Error string `json:"error"`
}

// Stats is what the spool has done since the process started, plus what it holds now.
type Stats struct {
	Depth    int   `json:"depth"`
	Replayed int64 `json:"replayed"`
	Dropped  int64 `json:"dropped"`
	Dead     int64 `json:"dead"`
}

func NewSpool(usage repository.Usage, log *slog.Logger, path func() string, maxBytes func() int64) *Spool {
	return &Spool{usage: usage, log: log, path: path, maxBytes: maxBytes}
}

// Add appends one accrual. Past the size cap it is logged and dropped: a full disk must not
// become a failed request, and the cap is what keeps an outage from filling it.
func (s *Spool) Add(a repository.Accrual) {
	if a.ID == "" {
		// The replay marker is keyed by it: a blank one would make every such line one request.
		a.ID = "spool-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	line, err := json.Marshal(entry{Accrual: a, At: time.Now().UTC()})
	if err != nil {
		s.drop(a, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path()
	if info, err := os.Stat(path); err == nil && info.Size()+int64(len(line))+1 > s.maxBytes() {
		s.drop(a, errors.New("spool full"))
		return
	}
	if err := appendLine(path, line); err != nil {
		s.drop(a, err)
	}
}

func (s *Spool) drop(a repository.Accrual, err error) {
	s.stats.dropped.Add(1)
	s.log.Error("usage dropped", "id", a.ID, "prefix", a.Prefix, "err", err)
}

// Run replays at start and then every interval until ctx ends.
func (s *Spool) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.Replay(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Replay is one pass: nothing while the store is down, else every line through Replay, keeping
// the ones that failed. A store that drops mid-pass ends the pass without counting a try.
func (s *Spool) Replay(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines, err := readLines(s.path())
	if err != nil || len(lines) == 0 || s.usage.Ping(ctx) != nil {
		return
	}
	var keep, dead [][]byte
	for i, line := range lines {
		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			dead = append(dead, deadLine("", line, err))
			continue
		}
		landed, err := s.usage.Replay(ctx, e.Accrual)
		switch {
		case err == nil:
			if landed {
				s.stats.replayed.Add(1)
			}
		case s.usage.Ping(ctx) != nil:
			s.finish(append(keep, lines[i:]...), dead)
			return
		case e.Tries+1 >= maxTries:
			dead = append(dead, deadLine(e.ID, line, err))
		default:
			e.Tries++
			retry, _ := json.Marshal(e)
			keep = append(keep, retry)
		}
	}
	s.finish(keep, dead)
}

// finish writes the dead lines, then the lines still waiting. Dead first: a crash between the two
// leaves a line in both, and its replay marker keeps that from counting twice.
func (s *Spool) finish(keep, dead [][]byte) {
	path := s.path()
	for _, line := range dead {
		if err := appendLine(path+".dead", line); err != nil {
			s.log.Error("dead usage line not written", "err", err)
			keep = append(keep, line)
			continue
		}
		s.stats.dead.Add(1)
		s.log.Error("usage line set aside as dead", "line", string(line))
	}
	if err := rewrite(path, keep); err != nil {
		s.log.Error("usage spool not rewritten", "err", err)
	}
}

// Dead is every line set aside, for the usage pull.
func (s *Spool) Dead() []Dead {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines, _ := readLines(s.path() + ".dead")
	out := make([]Dead, 0, len(lines))
	for _, line := range lines {
		var d Dead
		if json.Unmarshal(line, &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// Forget drops the dead lines the control plane has recorded. → how many.
func (s *Spool) Forget(ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	gone := map[string]bool{}
	for _, id := range ids {
		gone[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path() + ".dead"
	lines, err := readLines(path)
	if err != nil {
		return 0, err
	}
	var keep [][]byte
	for _, line := range lines {
		var d Dead
		if json.Unmarshal(line, &d) == nil && gone[d.ID] {
			continue
		}
		keep = append(keep, line)
	}
	return len(lines) - len(keep), rewrite(path, keep)
}

// Stats is the counters, plus how many lines wait in the spool right now.
func (s *Spool) Stats() Stats {
	s.mu.Lock()
	lines, _ := readLines(s.path())
	s.mu.Unlock()
	return Stats{
		Depth: len(lines), Replayed: s.stats.replayed.Load(),
		Dropped: s.stats.dropped.Load(), Dead: s.stats.dead.Load(),
	}
}

func deadLine(id string, line []byte, err error) []byte {
	out, _ := json.Marshal(Dead{ID: id, Line: string(line), Error: err.Error()})
	return out
}

func appendLine(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// readLines is every non-empty line; a missing file is no lines.
func readLines(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines [][]byte
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		if line := bytes.TrimSpace(scanner.Bytes()); len(line) > 0 {
			lines = append(lines, append([]byte(nil), line...))
		}
	}
	return lines, scanner.Err()
}

// rewrite replaces path with lines atomically (tmp + fsync + rename); none left removes it.
func rewrite(path string, lines [][]byte) error {
	if len(lines) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(bytes.Join(lines, []byte("\n")), '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
