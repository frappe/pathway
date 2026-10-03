package domain

import (
	"bytes"
	"encoding/json"
	"math"
)

// Usage is a token count normalized across the OpenAI and Anthropic response shapes, so both mean
// the same thing: Prompt is the FULL input including cache, Total = Prompt + Completion, and
// Cached ⊆ Prompt. Budget is Total - Cached on either.
type Usage struct {
	Prompt     int
	Completion int
	Total      int
	// Cached = prompt tokens served from the prefix cache, credited. Already inside prompt_tokens
	// on the OpenAI shape; separate from input_tokens on the Anthropic one, so it is folded back in
	// here. cache_creation is a write — billed, so it lands in Prompt but never in Cached.
	Cached int
	// CacheWrite is every prompt token written to the cache, CacheWrite1h the part of it written
	// with the hour TTL (Anthropic prices the two apart). Both ⊆ Prompt, CacheWrite1h ⊆ CacheWrite.
	// Already inside prompt_tokens on the OpenAI shape, like Cached.
	CacheWrite   int
	CacheWrite1h int
	// Audio = prompt tokens that were audio, priced apart from text. ⊆ Prompt, OpenAI shape only.
	Audio int
	// CompletionAudio = completion tokens that were audio, priced apart from text. ⊆ Completion,
	// OpenAI shape only.
	CompletionAudio int
	// Seconds of audio processed — all a duration-shaped transcription reports. Display only.
	Seconds int
}

// ParseUsage reads a token count from a full response, a bare usage object, or the tail of a
// response the tee truncated, in either key dialect. ok=false when no token fields are present — a
// streaming chunk before the final frame.
func ParseUsage(raw []byte) (Usage, bool) {
	raw = TrimSSE(raw)
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		// Unwrap a nested "usage" object if present.
		if uraw, ok := m["usage"]; ok {
			var um map[string]json.RawMessage
			if json.Unmarshal(uraw, &um) == nil {
				m = um
			}
		}
		if u, ok := usageFrom(m); ok {
			return u, true
		}
	}
	return lastUsage(raw)
}

// MergeUsage folds two usage reports of one request into one: the larger of each field, because
// counts only grow within a request, then Total raised to Prompt + Completion. An Anthropic stream
// reports the prompt on its first event and the output on its last.
func MergeUsage(a, b Usage) Usage {
	u := Usage{
		Prompt:          max(a.Prompt, b.Prompt),
		Completion:      max(a.Completion, b.Completion),
		Total:           max(a.Total, b.Total),
		Cached:          max(a.Cached, b.Cached),
		CacheWrite:      max(a.CacheWrite, b.CacheWrite),
		CacheWrite1h:    max(a.CacheWrite1h, b.CacheWrite1h),
		Audio:           max(a.Audio, b.Audio),
		CompletionAudio: max(a.CompletionAudio, b.CompletionAudio),
		Seconds:         max(a.Seconds, b.Seconds),
	}
	u.Total = max(u.Total, u.Prompt+u.Completion)
	return u
}

// lastUsage recovers usage from a line the tee cut at the front — no longer a document, but the
// usage object at its tail is whole — or one that nests it deeper. Backwards, because the real one
// is last. The ':' skips a token whose TEXT is "usage": a bare value is followed by ',' or '}', and
// inside a string the quotes arrive escaped and never match.
func lastUsage(raw []byte) (Usage, bool) {
	key := []byte(`"usage"`)
	for end := len(raw); ; {
		at := bytes.LastIndex(raw[:end], key)
		if at < 0 {
			return Usage{}, false
		}
		end = at
		value, isKey := bytes.CutPrefix(bytes.TrimLeft(raw[at+len(key):], " \t\r\n"), []byte(":"))
		if !isKey {
			continue
		}
		// A decoder reads ONE value and stops, so what follows the object need not parse.
		var object map[string]json.RawMessage
		if json.NewDecoder(bytes.NewReader(value)).Decode(&object) != nil {
			continue
		}
		if u, ok := usageFrom(object); ok {
			return u, true
		}
	}
}

// usageFrom normalizes one usage object, in either key dialect.
func usageFrom(m map[string]json.RawMessage) (Usage, bool) {
	geti := func(keys ...string) (int, bool) {
		for _, k := range keys {
			if r, ok := m[k]; ok {
				var n int
				if json.Unmarshal(r, &n) == nil {
					return n, true
				}
			}
		}
		return 0, false
	}
	// A transcription's usage is a duration, not tokens: {"type":"duration","seconds":N} as the
	// usage object, or — on a verbose_json body, which carries no usage at all — a top-level float.
	if seconds, ok := geti("seconds"); ok && string(m["type"]) == `"duration"` {
		return Usage{Seconds: seconds}, true
	}
	p, pok := geti("prompt_tokens", "input_tokens")
	c, cok := geti("completion_tokens", "output_tokens")
	t, tok := geti("total_tokens")
	// Anthropic cache buckets (separate from input_tokens; only present on that shape).
	cacheRead, crok := geti("cache_read_input_tokens")
	cacheCreate, ccok := geti("cache_creation_input_tokens")
	if !pok && !cok && !tok && !crok && !ccok {
		var duration float64
		if raw, ok := m["duration"]; ok && json.Unmarshal(raw, &duration) == nil {
			return Usage{Seconds: int(math.Ceil(duration))}, true
		}
		return Usage{}, false
	}

	u := Usage{Completion: c}
	if crok || ccok {
		// Anthropic shape: input_tokens EXCLUDES cache; fold the cache buckets back
		// in so Prompt/Total are the full processed input (matches OpenAI shape).
		// Credit reads only — cache_creation is a write, billed.
		u.Prompt = p + cacheRead + cacheCreate
		u.Cached = cacheRead
		u.CacheWrite = cacheCreate
		u.CacheWrite1h = min(nestedInt(m, "cache_creation", "ephemeral_1h_input_tokens"), cacheCreate)
		u.Total = u.Prompt + c
	} else {
		// OpenAI/vLLM shape: prompt_tokens already INCLUDES cached; the cached subset
		// is under prompt_tokens_details.cached_tokens (nested — parse out of band).
		u.Prompt = p
		u.Cached = nestedInt(m, "prompt_tokens_details", "cached_tokens")
		u.CacheWrite = nestedInt(m, "prompt_tokens_details", "cache_write_tokens")
		// A chat names the details prompt_tokens_details, a transcription input_token_details.
		u.Audio = nestedInt(m, "prompt_tokens_details", "audio_tokens") +
			nestedInt(m, "input_token_details", "audio_tokens")
		u.CompletionAudio = nestedInt(m, "completion_tokens_details", "audio_tokens")
		if tok {
			u.Total = t
		} else {
			u.Total = p + c
		}
	}
	return u, true
}

// nestedInt reads object[key], 0 when the object is absent, null, or not carrying the key.
func nestedInt(m map[string]json.RawMessage, object, key string) int {
	var inner map[string]json.RawMessage
	if json.Unmarshal(m[object], &inner) != nil {
		return 0
	}
	var n int
	_ = json.Unmarshal(inner[key], &n)
	return n
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
