package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/phot0n/pathway/internal/domain"
)

// streamEnd closes an event stream the upstream abandoned with an error event in the surface's own
// shape, the one its SDK raises on: `event: error` on the Anthropic surface, a `data:` chunk
// carrying `error` on the OpenAI one. Without it the status has long gone out as a 200 and the
// client sees a dropped connection with no reason. It sits outside the tee, so usage and the cut
// are read off the raw upstream exactly as before.
type streamEnd struct {
	body      io.ReadCloser
	client    context.Context
	hop       context.Context
	anthropic bool
	// tail is the last two bytes sent, which say how much it takes to close the event in progress.
	tail    []byte
	pending []byte
	ended   bool
}

func newStreamEnd(body io.ReadCloser, client, hop context.Context, anthropic bool) *streamEnd {
	return &streamEnd{body: body, client: client, hop: hop, anthropic: anthropic}
}

func (s *streamEnd) Read(p []byte) (int, error) {
	if s.ended {
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		if len(s.pending) == 0 {
			return n, io.EOF
		}
		return n, nil
	}
	n, err := s.body.Read(p)
	if n > 0 {
		s.tail = append(s.tail, p[:n]...)
		s.tail = s.tail[max(0, len(s.tail)-2):]
	}
	// A client that left has nobody to tell; its own abort is the right ending.
	if err == nil || err == io.EOF || s.client.Err() != nil {
		return n, err
	}
	s.ended = true
	s.pending = s.closing()
	return n, nil
}

func (s *streamEnd) Close() error { return s.body.Close() }

// closing is the newlines that finish whatever event was in progress, then the error event. An
// event cut mid-line still reaches the client broken; the error follows it.
func (s *streamEnd) closing() []byte {
	var out []byte
	switch {
	case len(s.tail) == 0 || string(s.tail) == "\n\n":
	case s.tail[len(s.tail)-1] == '\n':
		out = append(out, '\n')
	default:
		out = append(out, "\n\n"...)
	}
	status, message := http.StatusBadGateway, "upstream broke off the stream"
	if context.Cause(s.hop) == errUpstreamIdle {
		status, message = http.StatusGatewayTimeout, "upstream went silent"
	}
	if s.anthropic {
		out = append(out, "event: error\ndata: "...)
		out = append(out, domain.AnthropicError(status, []byte(message))...)
	} else {
		event, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": "api_error"}})
		out = append(out, "data: "...)
		out = append(out, event...)
	}
	return append(out, "\n\n"...)
}
