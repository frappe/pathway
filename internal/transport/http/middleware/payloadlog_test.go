package middleware

import (
	"bytes"
	"net/http/httptest"
	"testing"
)

// The row stores the complete body — no truncation (user call, 2026-08-31). total still counts
// independently so the two can never silently drift.
func TestPayloadRecorderKeepsEveryByte(t *testing.T) {
	recorder := &payloadRecorder{ResponseWriter: httptest.NewRecorder()}
	chunk := bytes.Repeat([]byte("x"), 10<<10)
	const chunks = 10
	for range chunks {
		if _, err := recorder.Write(chunk); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if len(recorder.body) != chunks*len(chunk) {
		t.Errorf("kept %d bytes, want all %d", len(recorder.body), chunks*len(chunk))
	}
	if recorder.total != int64(len(recorder.body)) {
		t.Errorf("total = %d, body = %d — the count must match what is stored", recorder.total, len(recorder.body))
	}
}
