package middleware

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

func init() {
	Register("payloadlog", newPayloadLog)
}

// payloadlog records the prompt and the output — the one place the gateway retains customer
// CONTENT rather than metadata, which is why it runs nowhere unless BOTH the box has a payload log
// configured AND the control plane flagged this key in. Never silently: default off, per key.
//
// What it logs is the customer's own view: the request as the client sent it (state.Raw, above the
// transforms) and the response as the client received it (below the proxy's model swap). One line
// per request; rid joins it to the access line. Inline media over mediaKeep is a placeholder; an
// upload is its form fields with a stand-in per file; an upgrade (realtime) is not logged at all.
func newPayloadLog(deps Deps) (Middleware, error) {
	if deps.Payload == nil {
		return passthrough, nil
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)
			if !state.Identity.Key.LogPayloads || (state.Raw == nil && state.Form == nil) {
				next.ServeHTTP(w, r)
				return
			}
			recorder := &payloadRecorder{ResponseWriter: w, status: http.StatusOK}
			// Deferred for the same reason the access line is: a client that hangs up mid-stream
			// unwinds this stage through http.ErrAbortHandler, and the frames already generated
			// are exactly the ones a support query about an abandoned request asks after.
			defer func() {
				prompt, promptBytes := state.Raw, int64(len(state.Raw))
				if state.Raw == nil {
					// An upload: what the form said, never the file. Its size is the whole form's,
					// -1 when the client sent it chunked.
					prompt, _ = json.Marshal(state.Form)
					promptBytes = r.ContentLength
				}
				attrs := []slog.Attr{
					slog.String("rid", state.RequestID),
					slog.String("key", or(state.Identity.Prefix(), "-")),
					slog.String("team", state.Identity.Key.Team),
					slog.String("model", or(state.Model, "-")),
					slog.String("fallback", or(state.Fallback, "-")),
					slog.String("path", r.URL.Path),
					slog.Int("status", recorder.status),
					slog.String("prompt", string(scrubPayload(prompt))),
				}
				output, encoding := recorder.output()
				attrs = append(attrs, slog.String("output", output))
				if encoding != "" {
					attrs = append(attrs, slog.String("output_encoding", encoding))
				}
				attrs = append(attrs, slog.Int64("prompt_bytes", promptBytes), slog.Int64("output_bytes", recorder.total))
				deps.Payload.LogAttrs(r.Context(), slog.LevelInfo, "payload", attrs...)
			}()
			next.ServeHTTP(recorder, r)
		})
	}, nil
}

// payloadRecorder passes every byte through and keeps a copy: all of a text response, and a binary
// one only up to mediaKeep, past which it just counts. Write-through first would read nicer, but
// the copy must happen before the caller's buffer is reused.
// ponytail: a text response is held whole until the line is written (no truncation, user call
// 2026-08-31); a cap is the fix if long streams ever hurt.
type payloadRecorder struct {
	http.ResponseWriter
	status int
	body   []byte
	total  int64
	// binary is decided on the first write, when the headers are final.
	binary  bool
	decided bool
}

func (p *payloadRecorder) WriteHeader(status int) {
	p.status = status
	p.ResponseWriter.WriteHeader(status)
}

func (p *payloadRecorder) Write(b []byte) (int, error) {
	if !p.decided {
		p.decided = true
		p.binary = binaryMediaType(p.Header().Get("Content-Type"))
	}
	if !p.binary || len(p.body) <= mediaKeep {
		p.body = append(p.body, b...)
	}
	p.total += int64(len(b))
	return p.ResponseWriter.Write(b)
}

// output is what the line records: text as received with big media and secrets replaced; a file
// as base64 when it fits mediaKeep, a placeholder when it does not — never scanned for secrets.
func (p *payloadRecorder) output() (text, encoding string) {
	contentType := p.Header().Get("Content-Type")
	switch {
	case p.binary && p.total <= mediaKeep:
		return base64.StdEncoding.EncodeToString(p.body), "base64"
	case p.binary:
		mediaType, _, _ := strings.Cut(contentType, ";")
		return fmt.Sprintf("[media %s %d bytes]", strings.TrimSpace(mediaType), p.total), ""
	case strings.HasPrefix(contentType, "text/event-stream"):
		return string(scrubStream(p.body)), ""
	}
	return string(scrubPayload(p.body)), ""
}

// Unwrap lets net/http find the underlying writer for Flush — without it a stream would buffer.
func (p *payloadRecorder) Unwrap() http.ResponseWriter { return p.ResponseWriter }
