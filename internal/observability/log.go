// Package observability sets up the two loggers this process writes: diagnostics, and the durable
// per-request record.
package observability

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Two loggers on purpose. Process is diagnostics on stdout for journald, mirrored at Warn and above
// into error.log so a failure is greppable without journalctl. Access is one line per request in its
// own file: a RECORD, not a diagnostic, so it must not move when the log level does.
type Loggers struct {
	Process *slog.Logger
	Access  *slog.Logger
	// Payload carries opted-in users' prompts and outputs — customer content, so unlike the two
	// above it has no stdout fallback: nil when no path is configured, and the stage that would
	// write it stays off. Content must never land somewhere retention was not decided for.
	Payload *slog.Logger
}

type Options struct {
	// Level is a LevelVar rather than a Level so a reload can move it under a running process:
	// slog reads it on every record, which is the whole reason it exists.
	Level *slog.LevelVar
	// AccessLogPath is the file the per-request line goes to. Blank sends it to stdout alongside
	// the diagnostics, which is what a local run wants and what a box gets before the directory
	// exists.
	AccessLogPath string
	// ErrorLogPath receives Warn and above, in addition to stdout. Blank keeps stdout alone.
	ErrorLogPath string
	// PayloadLogPath is the file opted-in users' prompts and outputs go to. Blank disables
	// payload logging for the whole box, whatever the pushed per-user flags say.
	PayloadLogPath string
}

// New builds both loggers and returns a close for the files it opened.
func New(opts Options) (Loggers, func(), error) {
	level := opts.Level
	if level == nil {
		level = new(slog.LevelVar)
	}
	var open []*reopener
	closeAll := func() {
		for _, f := range open {
			_ = f.Close()
		}
	}

	handlers := []slog.Handler{slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})}
	if opts.ErrorLogPath != "" {
		file, err := openLog(opts.ErrorLogPath)
		if err != nil {
			closeAll()
			return Loggers{}, func() {}, err
		}
		open = append(open, file)
		// Pinned at Warn: the point of a separate file is that turning the process log down to Error
		// or up to Debug does not change what a failure hunt finds in it.
		handlers = append(handlers, slog.NewJSONHandler(file, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	process := slog.New(fanout{handlers: handlers})

	loggers := Loggers{Process: process, Access: process}
	if opts.AccessLogPath != "" {
		file, err := openLog(opts.AccessLogPath)
		if err != nil {
			closeAll()
			return Loggers{}, func() {}, err
		}
		open = append(open, file)
		// Always at Info: the access line is the record, and a level change is about diagnostics.
		loggers.Access = slog.New(slog.NewJSONHandler(file, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	if opts.PayloadLogPath != "" {
		file, err := openLog(opts.PayloadLogPath)
		if err != nil {
			closeAll()
			return Loggers{}, func() {}, err
		}
		open = append(open, file)
		loggers.Payload = slog.New(slog.NewJSONHandler(file, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	return loggers, closeAll, nil
}

func openLog(path string) (*reopener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	return &reopener{path: path, file: file, checked: time.Now()}, nil
}

func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
}

// reopener follows logrotate's `create`: when the path no longer names the file it holds — the
// old one was renamed away — it reopens before writing. This is what lets rotation move the file
// instead of copytruncating it, which loses whatever lands during the copy and yanks the tail
// out from under anything following the file by inode. The stat is throttled to one a second;
// the lines written in between land on the renamed file, which a follower still holds open.
type reopener struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	checked time.Time
}

func (w *reopener) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if now := time.Now(); now.Sub(w.checked) >= time.Second {
		w.checked = now
		w.reopenIfMoved()
	}
	return w.file.Write(p)
}

func (w *reopener) reopenIfMoved() {
	held, err := w.file.Stat()
	if err == nil {
		if current, err := os.Stat(w.path); err == nil && os.SameFile(held, current) {
			return
		}
	}
	fresh, err := openAppend(w.path)
	if err != nil {
		// The held descriptor still works; a record on the rotated file beats a lost one.
		return
	}
	_ = w.file.Close()
	w.file = fresh
}

func (w *reopener) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

// Discard is for tests, which want neither file.
func Discard() Loggers {
	quiet := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return Loggers{Process: quiet, Access: quiet}
}
