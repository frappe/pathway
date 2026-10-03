package respond

import (
	"context"
	"testing"

	"github.com/phot0n/pathway/internal/domain"
)

// Root is the OpenAI surface: a request nobody marked is OpenAI.
func TestDialect(t *testing.T) {
	if got := Dialect(context.Background()); got != domain.DialectOpenAI {
		t.Errorf("unmarked = %q, want openai", got)
	}
	if got := Dialect(WithDialect(context.Background(), domain.DialectAnthropic)); got != domain.DialectAnthropic {
		t.Errorf("marked = %q, want anthropic", got)
	}
}
