package middleware

import (
	"log/slog"
	"net/http"
)

func init() {
	Register("payloadlog", newPayloadLog)
}

// payloadlog records the prompt and the output — the one place the gateway retains customer
// CONTENT rather than metadata, which is why it runs nowhere unless BOTH the box has a payload log
// configured AND the control plane flagged this user in. Never silently: default off, per user.
//
// What it logs is the customer's own view: the request as the client sent it (state.Raw, above the
// transforms) and the response as the client received it (below the proxy's model swap). One line
// per request; rid joins it to the access line. Bodyless paths — upgrade, multipart — are not
// logged at all.
func newPayloadLog(deps Deps) (Middleware, error) {
	if deps.Payload == nil {
		return passthrough, nil
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)
			if !state.Identity.User.LogPayloads || state.Raw == nil {
				next.ServeHTTP(w, r)
				return
			}
			recorder := &payloadRecorder{ResponseWriter: w, status: http.StatusOK}
			// Deferred for the same reason the access line is: a client that hangs up mid-stream
			// unwinds this stage through http.ErrAbortHandler, and the frames already generated
			// are exactly the ones a support query about an abandoned request asks after.
			defer func() {
				deps.Payload.LogAttrs(r.Context(), slog.LevelInfo, "payload",
					slog.String("rid", state.RequestID),
					slog.String("key", or(state.Identity.Prefix(), "-")),
					slog.String("user", state.Identity.Key.User),
					slog.String("model", or(state.Model, "-")),
					slog.String("path", r.URL.Path),
					slog.Int("status", recorder.status),
					slog.String("prompt", string(state.Raw)),
					slog.String("output", string(recorder.body)),
					slog.Int("prompt_bytes", len(state.Raw)),
					slog.Int64("output_bytes", recorder.total),
				)
			}()
			next.ServeHTTP(recorder, r)
		})
	}, nil
}

// payloadRecorder passes every byte through and keeps a copy of all of them. Write-through
// first would read nicer, but the copy must happen before the caller's buffer is reused.
// ponytail: unbounded per-request memory — the whole response is held until the line is written;
// bring back a cap if long streams ever hurt.
type payloadRecorder struct {
	http.ResponseWriter
	status int
	body   []byte
	total  int64
}

func (p *payloadRecorder) WriteHeader(status int) {
	p.status = status
	p.ResponseWriter.WriteHeader(status)
}

func (p *payloadRecorder) Write(b []byte) (int, error) {
	p.body = append(p.body, b...)
	p.total += int64(len(b))
	return p.ResponseWriter.Write(b)
}

// Unwrap lets net/http find the underlying writer for Flush — without it a stream would buffer.
func (p *payloadRecorder) Unwrap() http.ResponseWriter { return p.ResponseWriter }
