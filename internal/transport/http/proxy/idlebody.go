package proxy

import (
	"context"
	"io"
	"sync/atomic"
	"time"
)

// idleBody bounds the silence on a body whose headers have come, and says whose it was. The wait
// starts over when a read is asked for and again when it returns: the first is the upstream's to
// answer, the second the client's to take what was read. A wait that runs out cuts the hop with
// the silent side as the cause, which lets the upstream go either way and fails the read
// ReverseProxy is in or comes to next. A slow stream is never cut, only a silent side. Every read
// that brings bytes is also told to the hop: when the body began, and how it came, for the access
// line.
type idleBody struct {
	io.ReadCloser
	limit   time.Duration
	timer   *time.Timer
	hop     *Hop
	reading atomic.Bool
}

func newIdleBody(body io.ReadCloser, limit time.Duration, cut context.CancelCauseFunc, hop *Hop) *idleBody {
	b := &idleBody{ReadCloser: body, limit: limit, hop: hop}
	b.timer = time.AfterFunc(limit, func() {
		if b.reading.Load() {
			cut(errUpstreamIdle)
			return
		}
		cut(errClientIdle)
	})
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.limit)
	b.reading.Store(true)
	n, err := b.ReadCloser.Read(p)
	b.reading.Store(false)
	if err != nil {
		// The body is over, whoever ended it: nobody is left to wait on.
		b.timer.Stop()
	} else {
		b.timer.Reset(b.limit)
	}
	if n > 0 {
		b.hop.read()
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.ReadCloser.Close()
}
