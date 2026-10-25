package proxy

import (
	"io"
	"strings"
	"testing"

	"github.com/frappe/pathway/internal/domain"
)

// The per-token cost of reading usage off a stream: a thousand OpenAI chunks, each decoded because
// each names "usage" (null until the last).
func BenchmarkUsageReaderOnAStream(b *testing.B) {
	chunk := `data: {"id":"c","choices":[{"delta":{"content":"word"}}],"usage":null}` + "\n\n"
	last := `data: {"id":"c","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":1000,"total_tokens":1007}}` + "\n\n"
	stream := strings.Repeat(chunk, 1000) + last
	adapter := domain.UsageAdapterFor(domain.DialectOpenAI)
	b.SetBytes(int64(len(stream)))
	for b.Loop() {
		reader := newUsageReader(io.NopCloser(strings.NewReader(stream)), adapter, true)
		if _, err := io.Copy(io.Discard, reader); err != nil {
			b.Fatal(err)
		}
		if u, ok := reader.Usage(); !ok || u.Total != 1007 {
			b.Fatalf("got %+v ok=%v", u, ok)
		}
	}
}
