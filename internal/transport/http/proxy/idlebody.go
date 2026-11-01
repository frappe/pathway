package proxy

import (
	"io"
	"time"
)

// idleBody bounds the silence of an upstream that has already sent its headers. Every read that
// brings bytes restarts the wait; a wait that runs out calls cut, which cancels the hop and so
// fails the read ReverseProxy is blocked in. A slow stream is never cut, only a silent one. Every
// read that brings bytes is also told to the hop: when the body began, and how it came, for the
// access line.
type idleBody struct {
	io.ReadCloser
	limit time.Duration
	timer *time.Timer
	hop   *Hop
}

func newIdleBody(body io.ReadCloser, limit time.Duration, cut func(), hop *Hop) *idleBody {
	return &idleBody{ReadCloser: body, limit: limit, timer: time.AfterFunc(limit, cut), hop: hop}
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.hop.read()
		b.timer.Reset(b.limit)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.ReadCloser.Close()
}
