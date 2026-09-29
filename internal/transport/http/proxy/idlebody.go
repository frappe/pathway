package proxy

import (
	"io"
	"time"
)

// idleBody bounds the silence of an upstream that has already sent its headers. Every read that
// brings bytes restarts the wait; a wait that runs out calls cut, which cancels the hop and so
// fails the read ReverseProxy is blocked in. A slow stream is never cut, only a silent one.
type idleBody struct {
	io.ReadCloser
	limit time.Duration
	timer *time.Timer
}

func newIdleBody(body io.ReadCloser, limit time.Duration, cut func()) *idleBody {
	return &idleBody{ReadCloser: body, limit: limit, timer: time.AfterFunc(limit, cut)}
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(b.limit)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.ReadCloser.Close()
}
