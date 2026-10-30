package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The model the upstream answered under is read off the usage line, on either shape and whether
// it streamed. A body with no usage line names nothing; an escaped `"model"` inside a string is
// not the field.
func TestTheUpstreamsModelIsReadOffTheUsageLine(t *testing.T) {
	for name, tc := range map[string]struct{ contentType, body, want string }{
		"openai unary":  {"application/json", `{"model":"gpt-x-2026-01-01","choices":[],"usage":{"prompt_tokens":1}}`, "gpt-x-2026-01-01"},
		"openai stream": {"text/event-stream", "data: {\"model\":\"gpt-x-2026-01-01\",\"choices\":[],\"usage\":null}\n\ndata: {\"model\":\"gpt-x-2026-01-01\",\"usage\":{\"prompt_tokens\":1}}\n\ndata: [DONE]\n\n", "gpt-x-2026-01-01"},
		"anthropic stream": {"text/event-stream", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"claude-x-2026\",\"usage\":{\"input_tokens\":1}}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n", "claude-x-2026"},
		"no usage line": {"application/json", `{"model":"gpt-x","choices":[]}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			if got := forward(New(Options{}, quiet()), server.URL, false).ServedModel; got != tc.want {
				t.Errorf("served model = %q, want %q", got, tc.want)
			}
		})
	}
	if got := findModel([]byte(`{"content":"say \"model\":\"x\"","model":"real"}`)); got != "real" {
		t.Errorf("findModel = %q, want the field, not the text that escapes it", got)
	}
}
