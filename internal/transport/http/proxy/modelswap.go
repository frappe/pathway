package proxy

import (
	"bytes"
	"encoding/json"
	"io"
)

// ModelSwap names the id the route sent the upstream and the id the client knows the serving model
// by. The zero value means the upstream was asked by the client's own name, answers under it, and
// nothing is rewritten.
type ModelSwap struct {
	Upstream string
	Client   string
}

func (s ModelSwap) active() bool {
	return s.Upstream != "" && s.Client != "" && s.Upstream != s.Client
}

// maxModelValue bounds how long a `model` value is waited for: a longer one is not a model id and
// passes through as it came.
const maxModelValue = 256

// modelKey opens the field. Its value follows, after any spaces.
var modelKey = []byte(`"model":`)

// modelSwapReader rewrites the response's `"model":"…"` to the client's id as it streams through
// — the request-side modelmap run in reverse, so a customer who asked for anthropic/claude-4-5
// never learns the vendor's own spelling from the body. Whatever the upstream wrote is replaced,
// not only the id it was asked by: a vendor answers an alias under the name behind it.
//
// Matching raw bytes is safe: inside any JSON string value the quotes arrive escaped (\"model\"),
// so an unescaped `"model":` can only be the real field. Generated text cannot forge it.
type modelSwapReader struct {
	src     io.ReadCloser
	client  []byte // the client's id as JSON string content, without the quotes
	carry   []byte // tail that could still become the field, split across reads
	out     []byte // rewritten bytes not yet handed to the caller
	scratch []byte
	err     error
}

func newModelSwapReader(src io.ReadCloser, swap ModelSwap) *modelSwapReader {
	client, _ := json.Marshal(swap.Client)
	return &modelSwapReader{src: src, client: client[1 : len(client)-1], scratch: make([]byte, 32<<10)}
}

func (r *modelSwapReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 && r.err == nil {
		r.fill()
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	if n > 0 {
		return n, nil
	}
	return 0, r.err
}

func (r *modelSwapReader) Close() error { return r.src.Close() }

// fill reads one chunk and rewrites it. Only a tail that could still be the field is held back —
// usually nothing, so a streamed frame is never delayed waiting for the next one.
func (r *modelSwapReader) fill() {
	n, err := r.src.Read(r.scratch)
	out, carry := r.rewrite(append(r.carry, r.scratch[:n]...), err != nil)
	r.out, r.carry, r.err = out, append([]byte(nil), carry...), err
}

// rewrite replaces every complete model value in buf and returns what can go out and what must
// wait for the next read. On the last read nothing waits.
func (r *modelSwapReader) rewrite(buf []byte, last bool) (out, carry []byte) {
	for {
		at := bytes.Index(buf, modelKey)
		if at < 0 {
			break
		}
		value := at + len(modelKey)
		for value < len(buf) && buf[value] == ' ' {
			value++
		}
		end, arriving := -1, value == len(buf)
		if !arriving && buf[value] == '"' {
			end = closingQuote(buf[value+1:])
			arriving = end < 0
		}
		switch {
		case end >= 0:
			out = append(append(out, buf[:value+1]...), r.client...)
			buf = buf[value+1+end:]
		case arriving && !last && len(buf)-at <= maxModelValue:
			// Hold from the field's start until its value has closed.
			return append(out, buf[:at]...), buf[at:]
		default:
			// Not a string, or one that never closes: as it came.
			out, buf = append(out, buf[:value]...), buf[value:]
		}
	}
	hold := 0
	if !last {
		hold = holdback(buf)
	}
	return append(out, buf[:len(buf)-hold]...), buf[len(buf)-hold:]
}

// closingQuote is the index of the quote that ends a JSON string whose content starts at value.
// -1 when it has not arrived.
func closingQuote(value []byte) int {
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// holdback is the length of the longest tail of buf that could still open the field.
func holdback(buf []byte) int {
	for k := min(len(modelKey)-1, len(buf)); k > 0; k-- {
		if bytes.HasPrefix(modelKey, buf[len(buf)-k:]) {
			return k
		}
	}
	return 0
}
