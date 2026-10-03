package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

// payloadLine runs payloadlog for an opted-in user with the given request state over a handler,
// and returns the line it wrote and what the client received.
func payloadLine(t *testing.T, fill func(*State), r *http.Request, handler http.HandlerFunc) (map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	buf := &bytes.Buffer{}
	payloadlog, err := newPayloadLog(Deps{Payload: slog.New(slog.NewJSONHandler(buf, nil))})
	if err != nil {
		t.Fatal(err)
	}
	optIn := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r, state := newState(r)
			state.Identity.User.LogPayloads = true
			fill(state)
			next.ServeHTTP(w, r)
		})
	}
	w := httptest.NewRecorder()
	optIn(payloadlog(handler)).ServeHTTP(w, r)
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("no payload line: %v\n%s", err, buf)
	}
	return line, w
}

func post() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
}

// Inline media up to mediaKeep is logged as sent; past it, a placeholder naming its type, its
// decoded size and a hash a support engineer can match. Long text is never media.
func TestBigInlineMediaIsAPlaceholder(t *testing.T) {
	small := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 100<<10))
	big := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 500<<10))
	sum := sha256.Sum256(bytes.Repeat([]byte{7}, 500<<10))
	want := fmt.Sprintf("[media image/png %d bytes sha256:%s]", 500<<10, hex.EncodeToString(sum[:8]))
	prose := strings.Repeat("a long prompt, ", 30000)

	for name, tc := range map[string]struct {
		body, keep, gone string
	}{
		"small data URI kept":    {`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + small + `"}}]}]}`, small, ""},
		"big data URI":           {`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + big + `"}}]}]}`, want, big},
		"anthropic source":       {`{"messages":[{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + big + `"}}]}]}`, want, big},
		"long text is not media": {`{"messages":[{"content":"` + prose + `","data":"` + prose + `"}]}`, prose, ""},
	} {
		line, _ := payloadLine(t, func(s *State) { s.Raw = []byte(tc.body) }, post(),
			func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
		prompt, _ := line["prompt"].(string)
		if !strings.Contains(prompt, tc.keep) {
			t.Errorf("%s: prompt lost %q", name, tc.keep[:min(len(tc.keep), 60)])
		}
		if tc.gone != "" && strings.Contains(prompt, tc.gone) {
			t.Errorf("%s: big media was kept", name)
		}
		if tc.gone == "" && prompt != tc.body {
			t.Errorf("%s: an untouched body was re-encoded", name)
		}
	}
}

// Generated media in a response is replaced the same way, in one document or per stream event.
func TestBigMediaInTheOutputIsAPlaceholder(t *testing.T) {
	big := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 500<<10))
	for name, tc := range map[string]struct{ contentType, body string }{
		"image generation": {"application/json", `{"data":[{"b64_json":"` + big + `"}]}`},
		"audio stream":     {"text/event-stream", `data: {"choices":[{"delta":{"audio":{"data":"` + big + `"}}}]}` + "\n\n"},
	} {
		line, w := payloadLine(t, func(s *State) { s.Raw = []byte(`{}`) }, post(),
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.body)
			})
		output, _ := line["output"].(string)
		if strings.Contains(output, big) || !strings.Contains(output, "[media application/octet-stream 512000 bytes sha256:") {
			t.Errorf("%s: output = %.120q", name, output)
		}
		if w.Body.String() != tc.body {
			t.Errorf("%s: the client's response was changed", name)
		}
	}
}

// A file response — speech audio — is base64 when it fits mediaKeep, a placeholder past it, and
// the recorder stops holding it there. The client always gets every byte.
func TestABinaryResponseIsBase64OrAPlaceholder(t *testing.T) {
	for name, size := range map[string]int{"small": 100 << 10, "big": 2 << 20} {
		audio := bytes.Repeat([]byte{1}, size)
		var held int
		line, w := payloadLine(t, func(s *State) { s.Raw = []byte(`{}`) }, post(),
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "audio/mpeg")
				for chunk := range slices.Chunk(audio, 32<<10) {
					_, _ = w.Write(chunk)
				}
				held = len(w.(*payloadRecorder).body)
			})
		if w.Body.Len() != size {
			t.Errorf("%s: client got %d bytes, want %d", name, w.Body.Len(), size)
		}
		if line["output_bytes"] != float64(size) {
			t.Errorf("%s: output_bytes = %v", name, line["output_bytes"])
		}
		if name == "small" {
			if line["output"] != base64.StdEncoding.EncodeToString(audio) || line["output_encoding"] != "base64" {
				t.Errorf("small: output not the base64 audio (encoding %v)", line["output_encoding"])
			}
			continue
		}
		if line["output"] != fmt.Sprintf("[media audio/mpeg %d bytes]", size) || line["output_encoding"] != nil {
			t.Errorf("big: output = %.80q, encoding %v", line["output"], line["output_encoding"])
		}
		if held > mediaKeep+(32<<10) {
			t.Errorf("big: recorder held %d bytes of audio", held)
		}
	}
}

// An upload gets a line: its form fields and a stand-in naming the file, never the file, with the
// transcript as output.
func TestAnUploadIsLoggedAsItsFormFields(t *testing.T) {
	form := map[string]string{"model": "whisper", "language": "en", "file": "[file talk.mp3 audio/mpeg]"}
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(strings.Repeat("x", 4096)))
	line, _ := payloadLine(t, func(s *State) { s.Form = form }, r,
		func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"text":"hello there"}`) })

	var prompt map[string]string
	if err := json.Unmarshal([]byte(line["prompt"].(string)), &prompt); err != nil || !maps.Equal(prompt, form) {
		t.Errorf("prompt = %v, want the form %v", line["prompt"], form)
	}
	if line["output"] != `{"text":"hello there"}` || line["prompt_bytes"] != float64(4096) {
		t.Errorf("output %v, prompt_bytes %v", line["output"], line["prompt_bytes"])
	}
}
