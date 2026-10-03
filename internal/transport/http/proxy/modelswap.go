package proxy

import (
	"bytes"
	"encoding/json"
	"io"
)

// ModelSwap names the id the upstream answers with and the id the client asked us for. The zero
// value means the response already speaks the client's name and nothing is rewritten.
type ModelSwap struct {
	Upstream string
	Client   string
}

func (s ModelSwap) active() bool {
	return s.Upstream != "" && s.Client != "" && s.Upstream != s.Client
}

// modelSwapReader rewrites `"model":"<upstream>"` back to the client's id as the response streams
// through — the request-side modelmap run in reverse, so a customer who asked for
// anthropic/claude-4-5 never learns the vendor's own spelling from the body.
//
// Matching raw bytes is safe: inside any JSON string value the quotes arrive escaped (\"model\"),
// so an unescaped `"model":"…"` can only be the real field. Generated text cannot forge it.
type modelSwapReader struct {
	src      io.ReadCloser
	old, new [][]byte // parallel: compact and single-spaced spellings of the field
	carry    []byte   // tail that could still become a pattern split across reads
	out      []byte   // rewritten bytes not yet handed to the caller
	scratch  []byte
	err      error
}

func newModelSwapReader(src io.ReadCloser, swap ModelSwap) *modelSwapReader {
	upstream, _ := json.Marshal(swap.Upstream)
	client, _ := json.Marshal(swap.Client)
	r := &modelSwapReader{src: src, scratch: make([]byte, 32<<10)}
	for _, space := range []string{"", " "} {
		r.old = append(r.old, []byte(`"model":`+space+string(upstream)))
		r.new = append(r.new, []byte(`"model":`+space+string(client)))
	}
	return r
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

// fill reads one chunk, rewrites it, and holds back only a tail that could still open a pattern —
// usually nothing, so a streamed frame is never delayed waiting for the next one.
func (r *modelSwapReader) fill() {
	n, err := r.src.Read(r.scratch)
	buf := append(r.carry, r.scratch[:n]...)
	for i := range r.old {
		buf = bytes.ReplaceAll(buf, r.old[i], r.new[i])
	}
	if err != nil {
		// The carry cannot hold a whole pattern, so on EOF it flushes as-is.
		r.out, r.carry, r.err = buf, nil, err
		return
	}
	hold := r.holdback(buf)
	r.out = buf[:len(buf)-hold]
	r.carry = append([]byte(nil), buf[len(buf)-hold:]...)
}

// holdback is the length of the longest tail of buf that is a proper prefix of a pattern.
func (r *modelSwapReader) holdback(buf []byte) int {
	longest := 0
	for _, pattern := range r.old {
		limit := min(len(pattern)-1, len(buf))
		for k := limit; k > longest; k-- {
			if bytes.HasPrefix(pattern, buf[len(buf)-k:]) {
				longest = k
				break
			}
		}
	}
	return longest
}
