package domain

import (
	"bytes"
	"encoding/json"
)

// UsageAdapter reads the usage one dialect reports, off bytes the client also receives. It decodes
// only what usage needs and never changes a byte: the response still reaches the client as sent.
type UsageAdapter interface {
	// Event reads the usage one SSE payload carries, false when it carries none — a content delta,
	// an OpenAI chunk whose usage is null, half a frame.
	Event(payload []byte) (Usage, bool)
	// Document reads the usage a non-streaming body carries, false when it carries none. A body
	// cut at the front, past what the reader holds, is read from the usage object at its tail.
	Document(body []byte) (Usage, bool)
}

// UsageAdapterFor is the adapter for a dialect: Anthropic's for its own, OpenAI's for everything
// else, since every other path the gateway serves answers in that shape.
func UsageAdapterFor(dialect string) UsageAdapter {
	if dialect == DialectAnthropic {
		return anthropicUsage{}
	}
	return openaiUsage{}
}

// tailUsageObject is the value of the last `"usage":` key in a document whose front is gone —
// no longer JSON, but the usage object at its tail is whole. Backwards, because the real one is
// last. The ':' skips a token whose TEXT is "usage": a bare value is followed by ',' or '}', and
// inside a string the quotes arrive escaped and never match. Nil when there is none.
func tailUsageObject(raw []byte) json.RawMessage {
	key := []byte(`"usage"`)
	for end := len(raw); ; {
		at := bytes.LastIndex(raw[:end], key)
		if at < 0 {
			return nil
		}
		end = at
		value, isKey := bytes.CutPrefix(bytes.TrimLeft(raw[at+len(key):], " \t\r\n"), []byte(":"))
		if !isKey {
			continue
		}
		// A decoder reads ONE value and stops, so what follows the object need not parse.
		var object json.RawMessage
		if json.NewDecoder(bytes.NewReader(value)).Decode(&object) != nil || len(object) == 0 || object[0] != '{' {
			continue
		}
		return object
	}
}

// TrimSSE strips the "data:" framing off a streaming line. The final frame of an OpenAI stream is
// the only place a streaming request's usage appears, and it arrives wrapped. Handled here rather
// than at each call site so both the streaming and non-streaming paths parse the same way.
func TrimSSE(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if rest, ok := bytes.CutPrefix(trimmed, []byte("data:")); ok {
		return bytes.TrimSpace(rest)
	}
	return trimmed
}
