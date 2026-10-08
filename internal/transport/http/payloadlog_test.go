package http

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/phot0n/pathway/internal/config"
	"github.com/phot0n/pathway/internal/domain"
)

// payloadFixture is newFixture with a payload log wired in and the test key's opt-in set as
// asked. The buffer is the payload file.
func payloadFixture(t *testing.T, engineHandler http.HandlerFunc, optIn bool) (*fixture, *bytes.Buffer) {
	t.Helper()
	f := newFixture(t, engineHandler)
	f.withKey(func(k *domain.KeyRecord) { k.LogPayloads = optIn })

	buf := &bytes.Buffer{}
	f.handler = buildHandler(t, f.store, config.Config{}, 0, func(s *Services) {
		s.Payload = slog.New(slog.NewJSONHandler(buf, nil))
	})
	return f, buf
}

// jsonLine decodes a log buffer that must hold exactly one line.
func jsonLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log is not one JSON line: %v\n%s", err, buf)
	}
	return line
}

// The row a support query needs: the prompt as the customer wrote it, the output as they received
// it, and the join keys to the access line.
func TestAnOptedInKeysPromptAndOutputAreLogged(t *testing.T) {
	f, buf := payloadFixture(t, jsonEngine(`{"id":"chatcmpl-1",`+usageObject+`}`), true)
	prompt := `{"model":"qwen3-4b","messages":[{"role":"user","content":"hi"}]}`

	f.post("/v1/chat/completions", prompt)

	line := jsonLine(t, buf)
	if line["prompt"] != prompt {
		t.Errorf("prompt = %q, want the body as sent", line["prompt"])
	}
	if output, ok := line["output"].(string); !ok || !strings.Contains(output, "chatcmpl-1") {
		t.Errorf("output = %q, want what the client received", line["output"])
	}
	if line["model"] != "qwen3-4b" || line["team"] != "test-team" || line["key"] != "abc123" {
		t.Errorf("join fields off: %v", line)
	}
	if rid, ok := line["rid"].(string); !ok || rid == "" || rid == "-" {
		t.Errorf("rid = %q — the line cannot join the access log", line["rid"])
	}
	if line["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v", line["status"])
	}
}

// Default off, and never silent: a key the control plane has not flagged leaves no trace even on
// a box with the payload log configured.
func TestAKeyNotOptedInLogsNothing(t *testing.T) {
	f, buf := payloadFixture(t, jsonEngine(`{`+usageObject+`}`), false)

	f.post("/v1/chat/completions", `{"model":"qwen3-4b","messages":[]}`)

	if buf.Len() != 0 {
		t.Errorf("payload was logged without the opt-in: %s", buf)
	}
}

// The capture rides the response writer read-only: the stream must still reach the client in the
// bytes and frames the engine wrote.
func TestAStreamIsCapturedWithoutBeingAltered(t *testing.T) {
	frames := "data: {\"choices\":[{\"delta\":{\"content\":\"He\"}}]}\n\n" +
		"data: {\"choices\":[]," + usageObject + "}\n\ndata: [DONE]\n\n"
	f, buf := payloadFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frames)
	}, true)

	resp := f.post("/v1/chat/completions", `{"model":"qwen3-4b","stream":true}`)

	if resp.Body.String() != frames {
		t.Errorf("stream altered:\n got: %q\nwant: %q", resp.Body.String(), frames)
	}
	line := jsonLine(t, buf)
	if line["output"] != frames {
		t.Errorf("captured output = %q", line["output"])
	}
	if f.store.Usage["abc123"]["total_tokens"] != 120 {
		t.Errorf("metering lost under the capture: %v", f.store.Usage["abc123"])
	}
}
