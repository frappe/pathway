package proxy

import (
	"crypto/tls"
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
	// The body as it was read: how many reads brought bytes, when the last did, and the longest
	// silence between two of them — where a stream that stalled without being cut spent its time.
	chunks int
	last   time.Time
	gapMax time.Duration
	// reads is when each of those reads arrived, from the first: the stream's timeline, kept only
	// when timed — the stream_timeline tunable is on — for the one line it goes to.
	timed bool
	reads []time.Duration
}

const (
	hopConnectStart = iota
	hopConnectDone
	hopTLSStart
	hopTLSDone
	hopGotConn
	hopWroteRequest
	hopFirstByte // the upstream's headers
	hopFirstBody // the first byte of its body read, which a vendor may send well after its headers
	hopDone
)

func (h *Hop) mark(event int) {
	h.mu.Lock()
	h.at[event] = time.Now()
	h.mu.Unlock()
}

// read is one body read that brought bytes: the first stamps hopFirstBody, every later one
// measures its silence since the one before.
func (h *Hop) read() {
	now := time.Now()
	h.mu.Lock()
	if h.at[hopFirstBody].IsZero() {
		h.at[hopFirstBody] = now
	} else {
		h.gapMax = max(h.gapMax, now.Sub(h.last))
	}
	if h.timed {
		h.reads = append(h.reads, now.Sub(h.at[hopFirstBody]))
	}
	h.last = now
	h.chunks++
	h.mu.Unlock()
}

// Timeline is when each read that brought bytes arrived, ms from the first: the stream as it came,
// for the `stream timeline` line. Nil unless the hop was timed.
func (h *Hop) Timeline() []float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reads == nil {
		return nil
	}
	at := make([]float64, len(h.reads))
	for i, d := range h.reads {
		at[i] = float64(d.Microseconds()) / 1000
	}
	return at
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

// Legs is the hop as the access line reads it: each leg as a duration, zero when it did not
// happen — a reused connection has no connect and no tls — and Total, the whole of it, from the
// first event on the socket to the hop's end. Wait runs to the first byte of the body, not the
// headers: a vendor may send its headers at once and think before its first token, and that is
// its time. First is when that byte arrived (the headers, for a response with no body), zero when
// nothing did; the gateway's own `respond` is measured from it. Chunks is how many body reads
// brought bytes — one per upstream flush, about one per event on a stream — and GapMax the longest
// silence between two of them: a stream that stalled shows it near Stream, with few Chunks.
type Legs struct {
	Reused                           bool
	Connect, TLS, Send, Wait, Stream time.Duration
	Total                            time.Duration
	First                            time.Time
	Chunks                           int
	GapMax                           time.Duration
}

func (h *Hop) Legs() Legs {
	h.mu.Lock()
	defer h.mu.Unlock()
	leg := func(from, to time.Time) time.Duration {
		if from.IsZero() || to.IsZero() {
			return 0
		}
		return to.Sub(from)
	}
	start := h.at[hopConnectStart]
	if start.IsZero() {
		start = h.at[hopGotConn]
	}
	first := h.at[hopFirstBody]
	if first.IsZero() {
		first = h.at[hopFirstByte]
	}
	return Legs{
		Reused:  h.reused,
		Connect: leg(h.at[hopConnectStart], h.at[hopConnectDone]),
		TLS:     leg(h.at[hopTLSStart], h.at[hopTLSDone]),
		Send:    leg(h.at[hopGotConn], h.at[hopWroteRequest]),
		Wait:    leg(h.at[hopWroteRequest], first),
		Stream:  leg(first, h.at[hopDone]),
		Total:   leg(start, h.at[hopDone]),
		First:   first,
		Chunks:  h.chunks,
		GapMax:  h.gapMax,
	}
}
