package proxy

import (
	"crypto/tls"
	"log/slog"
	"net/http/httptrace"
	"sync"
	"time"
)

// Hop is when each event of the upstream hop happened, off net/http's own trace, for the access
// line. Locked: the transport fires ConnectDone and WroteRequest from goroutines of its own, and
// either can land after Forward returned — a dial outlives the request that started it, and a
// body is still being written when a server answers early. A pointer on Outcome, so Outcome stays
// a plain value.
type Hop struct {
	mu     sync.Mutex
	at     [hopDone + 1]time.Time
	reused bool
}

const (
	hopConnectStart = iota
	hopConnectDone
	hopTLSStart
	hopTLSDone
	hopGotConn
	hopWroteRequest
	hopFirstByte
	hopDone
)

func (h *Hop) mark(event int) {
	h.mu.Lock()
	h.at[event] = time.Now()
	h.mu.Unlock()
}

// trace is the hop's events as the transport reports them. Last wins: the transport retries a
// request whose connection died under it, and the attempt that answered is the one timed.
func (h *Hop) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		ConnectStart:      func(string, string) { h.mark(hopConnectStart) },
		ConnectDone:       func(string, string, error) { h.mark(hopConnectDone) },
		TLSHandshakeStart: func() { h.mark(hopTLSStart) },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { h.mark(hopTLSDone) },
		GotConn: func(info httptrace.GotConnInfo) {
			h.mu.Lock()
			h.at[hopGotConn], h.reused = time.Now(), info.Reused
			h.mu.Unlock()
		},
		WroteRequest:         func(httptrace.WroteRequestInfo) { h.mark(hopWroteRequest) },
		GotFirstResponseByte: func() { h.mark(hopFirstByte) },
	}
}

// Attrs is the hop for the access line: `reused`, then each leg in seconds, and only the legs
// that happened — a reused connection has no connect and no tls. `respond` is the gateway's own
// handling between the upstream's first byte and the client's; a blank clientFirstByte leaves it
// off, for an attempt the client never received.
func (h *Hop) Attrs(clientFirstByte time.Time) slog.Attr {
	h.mu.Lock()
	defer h.mu.Unlock()
	attrs := []any{slog.Bool("reused", h.reused)}
	leg := func(name string, from, to time.Time) {
		if !from.IsZero() && !to.IsZero() {
			attrs = append(attrs, slog.Float64(name, to.Sub(from).Seconds()))
		}
	}
	leg("connect", h.at[hopConnectStart], h.at[hopConnectDone])
	leg("tls", h.at[hopTLSStart], h.at[hopTLSDone])
	leg("send", h.at[hopGotConn], h.at[hopWroteRequest])
	leg("wait", h.at[hopWroteRequest], h.at[hopFirstByte])
	leg("respond", h.at[hopFirstByte], clientFirstByte)
	leg("stream", h.at[hopFirstByte], h.at[hopDone])
	return slog.Group("hop", attrs...)
}
