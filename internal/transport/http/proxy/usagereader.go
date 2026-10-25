package proxy

import (
	"bytes"
	"io"

	"github.com/frappe/pathway/internal/domain"
)

// carryLimit bounds what is held across reads — a partial line of a stream, or a body that is one
// document. Past it the tail is kept, because the usage object is at the end of it and the adapter
// reads it out of a document cut at the front. scan re-copies the carry on every Read, so raise
// this only after that.
const carryLimit = 1 << 20

// usageNeedle is in every usage-bearing payload of either dialect. A line without it is passed on
// without a decode: a content delta, an event name, [DONE].
var usageNeedle = []byte(`"usage"`)

// usageReader keeps what a response says about usage, read through the dialect's adapter. An event
// stream is read by the line, and every usage a line carries is merged: an Anthropic stream reports
// the prompt on its first event and the output on its last, an engine of ours the running count on
// every chunk. Any other body is one document, read whole at its end — a vendor may print it over
// many lines. It reads what it is already copying and writes nothing back, so the response reaches
// the client byte-for-byte and on time. That property is the contract.
type usageReader struct {
	body    io.ReadCloser
	adapter domain.UsageAdapter
	stream  bool
	failed  bool // a read ended in an error: the body broke off before its end
	ended   bool // the body was read to its end
	carry   []byte
	usage   domain.Usage
	found   bool
}

func newUsageReader(body io.ReadCloser, adapter domain.UsageAdapter, stream bool) *usageReader {
	return &usageReader{body: body, adapter: adapter, stream: stream}
}

func (r *usageReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.scan(p[:n])
	}
	if err == io.EOF {
		r.ended = true
		r.flush()
	} else if err != nil {
		r.failed = true
	}
	return n, err
}

// Close reads a document to its end first when the client hung up on it: the upstream generated
// and billed the whole answer before it sent a byte, so the usage at its tail is owed whether or
// not anyone was there to receive it. A stream is not read on — it is billed by what arrived.
// ponytail: bounded by the idle timeout per read only; a cap on bytes if a vendor ever drips.
func (r *usageReader) Close() error {
	if !r.stream && !r.ended && !r.failed {
		_, _ = io.Copy(io.Discard, r)
	}
	// A stream cut mid-line still leaves a partial line worth reading: it may be the usage frame
	// of a response the engine finished generating.
	r.flush()
	return r.body.Close()
}

// Usage is what the response reported, merged, and whether it reported anything at all.
func (r *usageReader) Usage() (domain.Usage, bool) { return r.usage, r.found }

func (r *usageReader) scan(chunk []byte) {
	data := chunk
	if len(r.carry) > 0 {
		data = append(r.carry, chunk...)
		r.carry = nil
	}
	for r.stream {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			break
		}
		r.line(data[:newline])
		data = data[newline+1:]
	}
	if len(data) > carryLimit {
		data = data[len(data)-carryLimit:]
	}
	// Copied rather than aliased: `p` belongs to the caller and is reused on the next Read.
	r.carry = append([]byte(nil), data...)
}

// flush reads what was left without a newline: a whole document, or a stream's last line. Half a
// frame decodes as nothing, so a stream cut inside a line keeps the last frame that was whole.
func (r *usageReader) flush() {
	if r.stream {
		r.line(r.carry)
	} else if u, ok := r.adapter.Document(r.carry); ok {
		r.merge(u)
	}
	r.carry = nil
}

func (r *usageReader) line(line []byte) {
	if !bytes.Contains(line, usageNeedle) {
		return
	}
	if u, ok := r.adapter.Event(domain.TrimSSE(line)); ok {
		r.merge(u)
	}
}

func (r *usageReader) merge(u domain.Usage) {
	r.usage, r.found = domain.MergeUsage(r.usage, u), true
}
