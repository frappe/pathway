package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/phot0n/pathway/internal/domain"
)

// The ring walks every key once on a 429, opens once more, and never again; a rejected key stays
// out through the re-opening.
func TestKeyRingSpendsRetiresAndReopensOnce(t *testing.T) {
	a, b := domain.Credential{ID: "a"}, domain.Credential{ID: "b"}
	ring := newKeyRing([]domain.Credential{a, b})

	if next := ring.next(a, 429); next.ID != "b" {
		t.Fatalf("after a's 429 got %q, want b", next.ID)
	}
	if next := ring.next(b, 401); next.ID != "a" {
		t.Fatalf("after b's 401 got %q, want a once more (the ring re-opened)", next.ID)
	}
	if next := ring.next(a, 429); next.ID != "" {
		t.Fatalf("after a's second 429 got %q, want nothing: b is dead and the ring opened already", next.ID)
	}
}

// The first final status decides; a held attempt leaks nothing, a forwarded one everything.
func TestAttemptWriterHoldsOrForwards(t *testing.T) {
	for _, held := range []bool{true, false} {
		real := httptest.NewRecorder()
		w := &attemptWriter{ResponseWriter: real, again: func(status int) bool { return held && status == 429 }}
		w.WriteHeader(http.StatusTooManyRequests)
		w.WriteHeader(http.StatusOK) // a second final status is ignored, as net/http does
		_, _ = w.Write([]byte("body"))
		w.Flush()
		if w.held.Load() != held {
			t.Errorf("held = %v, want %v", w.held.Load(), held)
		}
		if held && (real.Code != http.StatusOK || real.Body.Len() != 0) {
			// The recorder's default 200 means WriteHeader was never called on it.
			t.Errorf("held attempt leaked: code=%d body=%q", real.Code, real.Body)
		}
		if !held && (real.Code != http.StatusTooManyRequests || real.Body.String() != "body") {
			t.Errorf("forwarded attempt: code=%d body=%q", real.Code, real.Body)
		}
	}
}

// A 1xx is the upstream's interim word, passed straight through and deciding nothing.
func TestAttemptWriterPassesInterimStatusesThrough(t *testing.T) {
	real := httptest.NewRecorder()
	w := &attemptWriter{ResponseWriter: real, again: func(int) bool { t.Fatal("a 1xx asked the ring"); return false }}
	w.WriteHeader(http.StatusContinue)
	if real.Code != http.StatusContinue || w.wrote || w.held.Load() {
		t.Errorf("code=%d wrote=%v held=%v", real.Code, w.wrote, w.held.Load())
	}
}
