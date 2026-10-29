package middleware

import (
	"bytes"
	"io"
	"math/bits"
	"net/http"
	"sync"

	"github.com/frappe/pathway/internal/domain"
)

// A request body is read once from the client and written once or more to an upstream, and a
// 1M-token one is 4MB. Without a pool each is a fresh allocation, reached through io.ReadAll's
// doubling chain of copies. Buffers come in power-of-two classes from 64 KiB to 8 MiB; a body past
// the largest class is allocated as before and never pooled.
//
// A buffer goes back only at the end of its request, after every reader over it is closed.
// ReverseProxy closes the body it forwarded before it returns, but net/http warns that a Read on
// the transport's goroutine can still be in flight after that, so bodyReader holds a lock across
// each Read and refuses once closed. What another request could otherwise read is this one's
// prompt.

const (
	smallestClass = 16 // 64 KiB
	largestClass  = 23 // 8 MiB
)

var bufferClasses [largestClass + 1]sync.Pool

// takeBuffer is an empty slice with room for size bytes: pooled when size fits a class, a plain
// allocation past the largest.
func takeBuffer(size int) []byte {
	class := bits.Len(uint(max(size, 1) - 1))
	if class < smallestClass {
		class = smallestClass
	}
	if class > largestClass {
		return make([]byte, 0, size)
	}
	if pooled, ok := bufferClasses[class].Get().(*[]byte); ok {
		return (*pooled)[:0]
	}
	return make([]byte, 0, 1<<class)
}

// giveBuffer returns b to the class its capacity fills, and drops one too small or too large.
func giveBuffer(b []byte) {
	class := bits.Len(uint(cap(b))) - 1
	if class < smallestClass || class > largestClass {
		return
	}
	b = b[:0]
	bufferClasses[class].Put(&b)
}

// readBody reads the whole body into a pooled buffer the request owns. hint is the Content-Length,
// which a JSON client sends, capped at the body limit so a lying one cannot size the buffer; a
// chunked body starts at the smallest class and grows.
func readBody(body io.Reader, hint, limit int64, state *State) ([]byte, error) {
	size := 64 << 10
	if hint > 0 {
		// +1: a body exactly hint long reads its EOF without growing.
		size = int(min(hint, limit)) + 1
	}
	buf := takeBuffer(size)
	for {
		if len(buf) == cap(buf) {
			grown := takeBuffer(2 * cap(buf))
			grown = append(grown, buf...)
			giveBuffer(buf)
			buf = grown
		}
		n, err := body.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			break
		}
		if err != nil {
			giveBuffer(buf)
			return nil, err
		}
	}
	state.own(buf)
	return buf, nil
}

// encodeBody is state.Body as the upstream's body, in a pooled buffer the request owns. Sized from
// the body as sent: a rewrite moves a few fields, not megabytes.
func encodeBody(state *State) ([]byte, error) {
	buf := takeBuffer(len(state.Raw) + 4096)
	if err := domain.EncodeJSON(&buf, state.Body); err != nil {
		giveBuffer(buf)
		return nil, err
	}
	state.own(buf)
	return buf, nil
}

// bodyReader is a request body over a buffer the request owns. Once closed it refuses to read, and
// a Read holds the lock, so the buffer can go back to the pool the moment Close returns.
type bodyReader struct {
	mu     sync.Mutex
	reader *bytes.Reader
	closed bool
}

func (b *bodyReader) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, http.ErrBodyReadAfterClose
	}
	return b.reader.Read(p)
}

func (b *bodyReader) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return nil
}
