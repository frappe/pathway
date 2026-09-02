package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
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

// A client that hangs up mid-stream unwinds both logging stages through http.ErrAbortHandler,
// which recover deliberately re-panics rather than swallowing. The upstream still generated —
// and billed — the frames it had sent by then, so neither line may be lost to the unwind: an
// access log missing its abandoned streams cannot be reconciled against the bill, and the
// payload of an abandoned request is exactly what a support query asks after.
func TestBothLogLinesSurviveAClientHangup(t *testing.T) {
	access, payload := &bytes.Buffer{}, &bytes.Buffer{}
	deps := Deps{
		Access:  slog.New(slog.NewJSONHandler(access, nil)),
		Payload: slog.New(slog.NewJSONHandler(payload, nil)),
	}
	accesslog, err := newAccessLog(deps)
	if err != nil {
		t.Fatalf("newAccessLog: %v", err)
	}
	payloadlog, err := newPayloadLog(deps)
	if err != nil {
		t.Fatalf("newPayloadLog: %v", err)
	}
	// The stages auth and body would have filled in, between the two under test.
	optIn := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)
			state.Identity.User.LogPayloads = true
			state.Raw = []byte(`{"model":"qwen3-4b","stream":true}`)
			next.ServeHTTP(w, r)
		})
	}
	const frame = "data: {\"choices\":[{\"delta\":{\"content\":\"He\"}}]}\n\n"
	aborting := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(frame))
		panic(http.ErrAbortHandler)
	})

	handler := accesslog(optIn(payloadlog(aborting)))
	func() {
		defer func() {
			if p := recover(); p == nil {
				t.Fatal("the hangup was swallowed — recover must still see it to drop the connection")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	}()

	var line map[string]any
	if err := json.Unmarshal(payload.Bytes(), &line); err != nil {
		t.Fatalf("no payload line survived the hangup: %v\n%s", err, payload)
	}
	if line["output"] != frame {
		t.Errorf("output = %q, want the frames sent before the hangup", line["output"])
	}
	if err := json.Unmarshal(access.Bytes(), &line); err != nil {
		t.Fatalf("no access line survived the hangup: %v\n%s", err, access)
	}
	if line["path"] != "/v1/chat/completions" || line["bytes"] != float64(len(frame)) {
		t.Errorf("access line = %v, want the partial response it actually wrote", line)
	}
}
