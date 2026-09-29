package domain

import "testing"

func TestParseUsageOpenAI(t *testing.T) {
	raw := []byte(`{"id":"x","usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}}`)
	u, ok := ParseUsage(raw)
	if !ok || u.Prompt != 12 || u.Completion != 8 || u.Total != 20 || u.Cached != 0 {
		t.Fatalf("got %+v ok=%v (Cached must default 0 when no details)", u, ok)
	}
}

func TestParseUsageCachedTokens(t *testing.T) {
	// Live vLLM shape (--enable-prompt-tokens-details): cached_tokens is a subset of
	// prompt_tokens and already inside total_tokens.
	raw := []byte(`{"usage":{"prompt_tokens":617,"completion_tokens":5,"total_tokens":622,"prompt_tokens_details":{"cached_tokens":608,"multimodal_tokens":null}}}`)
	u, ok := ParseUsage(raw)
	if !ok || u.Prompt != 617 || u.Total != 622 || u.Cached != 608 {
		t.Fatalf("got %+v ok=%v (want Cached=608 ⊆ Prompt=617, in Total=622)", u, ok)
	}
}

func TestParseUsageCachedNullDetails(t *testing.T) {
	// Flag off (or field absent) → prompt_tokens_details null → Cached stays 0.
	raw := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":null}}`)
	u, ok := ParseUsage(raw)
	if !ok || u.Cached != 0 {
		t.Fatalf("got %+v ok=%v (want Cached=0)", u, ok)
	}
}

func TestParseUsageAnthropic(t *testing.T) {
	// Anthropic non-streaming: input_tokens/output_tokens, no total.
	raw := []byte(`{"type":"message","usage":{"input_tokens":30,"output_tokens":5}}`)
	u, ok := ParseUsage(raw)
	if !ok || u.Prompt != 30 || u.Completion != 5 || u.Total != 35 {
		t.Fatalf("got %+v ok=%v (total should derive as 35)", u, ok)
	}
}

func TestParseUsageAnthropicCache(t *testing.T) {
	// Live vLLM native /v1/messages shape (gateway → vLLM direct, no LiteLLM):
	// input_tokens EXCLUDES cache; cache_read is separate; no total_tokens. We fold
	// cache back in so Prompt=617, Total=622, Cached=608 — same as the OpenAI shape.
	raw := []byte(`{"usage":{"input_tokens":9,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":608}}`)
	u, ok := ParseUsage(raw)
	if !ok || u.Prompt != 617 || u.Completion != 5 || u.Total != 622 || u.Cached != 608 {
		t.Fatalf("got %+v ok=%v (want Prompt=617 Total=622 Cached=608)", u, ok)
	}
	// billable = Total - Cached must equal the non-cached input + output.
	if u.Total-u.Cached != 14 {
		t.Fatalf("billable %d, want 14", u.Total-u.Cached)
	}
}

func TestParseUsageAnthropicCacheCreation(t *testing.T) {
	// cache_creation is a WRITE: counts in Prompt/Total (billed) but NOT credited.
	raw := []byte(`{"usage":{"input_tokens":10,"output_tokens":2,"cache_creation_input_tokens":100,"cache_read_input_tokens":0}}`)
	u, ok := ParseUsage(raw)
	if !ok || u.Prompt != 110 || u.Total != 112 || u.Cached != 0 {
		t.Fatalf("got %+v ok=%v (want Prompt=110 Total=112 Cached=0)", u, ok)
	}
}

func TestParseUsageBareObject(t *testing.T) {
	// The usage object itself, not wrapped.
	raw := []byte(`{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}`)
	u, ok := ParseUsage(raw)
	if !ok || u.Total != 3 {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
}

func TestParseUsageNoUsage(t *testing.T) {
	// A streaming chunk before the final usage frame.
	raw := []byte(`{"choices":[{"delta":{"content":"hi"}}]}`)
	if _, ok := ParseUsage(raw); ok {
		t.Fatal("expected ok=false when no token fields present")
	}
}

func TestParseUsageTruncatedBody(t *testing.T) {
	// What the tee hands over for a body past its carry limit: the front is gone, so this is no
	// longer a document, but the usage object at the tail is whole.
	raw := []byte(`lo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":617,"completion_tokens":5,"total_tokens":622,"prompt_tokens_details":{"cached_tokens":608}},"prompt_logprobs":null}`)
	u, ok := ParseUsage(raw)
	if !ok || u.Prompt != 617 || u.Completion != 5 || u.Total != 622 || u.Cached != 608 {
		t.Fatalf("got %+v ok=%v (a truncated body must not bill as zero)", u, ok)
	}
}

func TestParseUsageTruncatedAnthropicBody(t *testing.T) {
	raw := []byte(`"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":608}}`)
	u, ok := ParseUsage(raw)
	if !ok || u.Prompt != 617 || u.Total != 622 || u.Cached != 608 {
		t.Fatalf("got %+v ok=%v (want the Anthropic fold: Prompt=617 Total=622 Cached=608)", u, ok)
	}
}

func TestParseUsageSkipsAUsageThatIsNotTheKey(t *testing.T) {
	// A token whose text is "usage" is a string VALUE after the real key, and an escaped one inside
	// a string never matches at all. Neither may shadow the object.
	raw := []byte(`t":"say \"usage\": now"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7},"prompt_logprobs":[{"token":"usage"}]}`)
	u, ok := ParseUsage(raw)
	if !ok || u.Total != 7 {
		t.Fatalf("got %+v ok=%v (want Total=7)", u, ok)
	}
}

func TestParseUsageNested(t *testing.T) {
	// A whole document whose usage sits a level down is read the same way as a truncated one.
	raw := []byte(`data: {"type":"response.completed","response":{"output":[],"usage":{"input_tokens":30,"output_tokens":5,"total_tokens":35}}}`)
	u, ok := ParseUsage(raw)
	if !ok || u.Prompt != 30 || u.Completion != 5 || u.Total != 35 {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
}

func TestParseUsageTruncatedWithoutUsage(t *testing.T) {
	for _, raw := range []string{
		`lo"},"finish_reason":"stop"}],"usage":null}`,
		`lo"},"finish_reason":"stop"}]}`,
		`"}],"usage":{"prompt_tok`,
	} {
		if u, ok := ParseUsage([]byte(raw)); ok {
			t.Errorf("%s: got %+v, want ok=false", raw, u)
		}
	}
}

func TestParseUsageGarbage(t *testing.T) {
	if _, ok := ParseUsage([]byte("not json")); ok {
		t.Fatal("expected ok=false on invalid json")
	}
}

// The cache writes, split by TTL, come off the Anthropic shape with or without the nested
// breakdown; the hour bucket can never exceed the writes it is part of.
func TestParseUsageAnthropicCacheWrites(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		write, hour  int
		prompt, read int
	}{
		{"nested breakdown", `{"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":30,` +
			`"cache_creation_input_tokens":20,"cache_creation":{"ephemeral_5m_input_tokens":15,"ephemeral_1h_input_tokens":5}}}`,
			20, 5, 60, 30},
		{"no breakdown", `{"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":30,"cache_creation_input_tokens":20}}`,
			20, 0, 60, 30},
		{"hour bucket clamped to the writes", `{"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":0,` +
			`"cache_creation_input_tokens":20,"cache_creation":{"ephemeral_1h_input_tokens":99}}}`,
			20, 20, 30, 0},
		// A creation-only object used to fall into the OpenAI branch and lose the fold.
		{"creation without read still folds", `{"usage":{"input_tokens":10,"output_tokens":2,"cache_creation_input_tokens":20}}`,
			20, 0, 30, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, ok := ParseUsage([]byte(tc.raw))
			if !ok || u.CacheWrite != tc.write || u.CacheWrite1h != tc.hour || u.Prompt != tc.prompt || u.Cached != tc.read {
				t.Fatalf("got %+v ok=%v, want write=%d hour=%d prompt=%d cached=%d", u, ok, tc.write, tc.hour, tc.prompt, tc.read)
			}
			if u.Completion != 2 || u.Total != tc.prompt+2 {
				t.Fatalf("got %+v, want completion 2 and total %d", u, tc.prompt+2)
			}
		})
	}
}

// A transcription's usage is a duration: the seconds land and no token field is invented, off
// the usage object vLLM and whisper-1 send or the top-level float a verbose_json body carries.
func TestParseUsageDuration(t *testing.T) {
	for name, raw := range map[string]string{
		"usage object":    `{"text":"hi","usage":{"type":"duration","seconds":12}}`,
		"bare object":     `{"type":"duration","seconds":12}`,
		"verbose_json":    `{"task":"transcribe","duration":11.2,"text":"hi","segments":[]}`,
		"streamed object": `data: {"type":"transcript.text.done","text":"hi","usage":{"type":"duration","seconds":12}}`,
	} {
		t.Run(name, func(t *testing.T) {
			u, ok := ParseUsage([]byte(raw))
			if !ok || u.Seconds != 12 {
				t.Fatalf("got %+v ok=%v, want Seconds=12", u, ok)
			}
			if u.Prompt != 0 || u.Completion != 0 || u.Total != 0 {
				t.Fatalf("got %+v, want no token fields", u)
			}
		})
	}
}

// Token-shaped transcription usage prices as tokens, and a body with neither is still no usage.
func TestParseUsageDurationDoesNotShadowTokens(t *testing.T) {
	u, ok := ParseUsage([]byte(`{"duration":3.5,"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	if !ok || u.Total != 3 || u.Seconds != 0 {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
	if _, ok := ParseUsage([]byte(`{"text":"hi"}`)); ok {
		t.Fatal("usage found in a body with no usage")
	}
}
