package proxy

import (
	"bytes"
	"io"
)

// carryLimit bounds the partial line held across reads — a single huge non-streaming body. Past it
// the tail is kept, because the usage object is at the end of it and domain.ParseUsage reads it out
// of a line cut at the front. scan re-copies the carry on every Read, so raise this only after that.
const carryLimit = 1 << 20

// usageTee keeps the last newline-delimited line containing "usage" — the final frame of a stream,
// or a whole non-streaming body — and the first, when there is more than one: an Anthropic stream
// reports the prompt on its first event and the output on its last. It reads what it is already
// copying and writes nothing back, so the stream reaches the client byte-for-byte and on time.
// That property is the contract.
type usageTee struct {
	body  io.ReadCloser
	carry []byte
	first []byte
	line  []byte
}

func newUsageTee(body io.ReadCloser) *usageTee { return &usageTee{body: body} }

func (t *usageTee) Read(p []byte) (int, error) {
	n, err := t.body.Read(p)
	if n > 0 {
		t.scan(p[:n])
	}
	if err == io.EOF {
		t.flush()
	}
	return n, err
}

func (t *usageTee) Close() error {
	// A client that hung up mid-stream still leaves a partial line worth reading: it may be the
	// usage frame of a response the engine finished generating.
	t.flush()
	return t.body.Close()
}

// Usage is the captured line, or empty if the response never carried one.
func (t *usageTee) Usage() string { return string(t.line) }

// UsageStart is the first usage-bearing line, empty when Usage is the only one.
func (t *usageTee) UsageStart() string { return string(t.first) }

func (t *usageTee) scan(chunk []byte) {
	data := chunk
	if len(t.carry) > 0 {
		data = append(t.carry, chunk...)
		t.carry = nil
	}
	for {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			break
		}
		t.keep(data[:newline])
		data = data[newline+1:]
	}
	if len(data) > carryLimit {
		data = data[len(data)-carryLimit:]
	}
	// Copied rather than aliased: `p` belongs to the caller and is reused on the next Read.
	t.carry = append([]byte(nil), data...)
}

func (t *usageTee) flush() {
	t.keep(t.carry)
	t.carry = nil
}

// ponytail: first and last only. Per-event merge if a vendor ever splits its counts three ways.
func (t *usageTee) keep(line []byte) {
	if !bytes.Contains(line, []byte(`"usage"`)) {
		return
	}
	if t.first == nil {
		t.first = t.line
	}
	t.line = append([]byte(nil), line...)
}
