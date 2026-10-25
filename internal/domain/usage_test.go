package domain

import "testing"

var openai, anthropic = UsageAdapterFor(DialectOpenAI), UsageAdapterFor(DialectAnthropic)

func TestOpenAIDocument(t *testing.T) {
	u, ok := openai.Document([]byte(`{"id":"x","usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}}`))
	if !ok || u.Prompt != 12 || u.Completion != 8 || u.Total != 20 || u.Cached != 0 {
		t.Fatalf("got %+v ok=%v (Cached must default 0 when no details)", u, ok)
	}
}

func TestOpenAICachedTokens(t *testing.T) {
	// Live vLLM shape (--enable-prompt-tokens-details): cached_tokens is a subset of
	// prompt_tokens and already inside total_tokens.
	raw := []byte(`{"usage":{"prompt_tokens":617,"completion_tokens":5,"total_tokens":622,"prompt_tokens_details":{"cached_tokens":608,"multimodal_tokens":null}}}`)
	u, ok := openai.Document(raw)
	if !ok || u.Prompt != 617 || u.Total != 622 || u.Cached != 608 {
		t.Fatalf("got %+v ok=%v (want Cached=608 ⊆ Prompt=617, in Total=622)", u, ok)
	}
}

func TestOpenAICachedNullDetails(t *testing.T) {
	// Flag off (or field absent) → prompt_tokens_details null → Cached stays 0.
	u, ok := openai.Document([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":null}}`))
	if !ok || u.Cached != 0 {
		t.Fatalf("got %+v ok=%v (want Cached=0)", u, ok)
	}
}

// OpenAI reports a cache write inside prompt_tokens, beside the cached read.
func TestOpenAICacheWrite(t *testing.T) {
	raw := []byte(`{"usage":{"prompt_tokens":300,"completion_tokens":8,"total_tokens":308,` +
		`"prompt_tokens_details":{"cached_tokens":200,"cache_write_tokens":80}}}`)
	u, ok := openai.Document(raw)
	if !ok || u.Prompt != 300 || u.Cached != 200 || u.CacheWrite != 80 || u.CacheWrite1h != 0 || u.Total != 308 {
		t.Fatalf("got %+v ok=%v (want CacheWrite=80 ⊆ Prompt=300)", u, ok)
	}
}

// The responses API names the prompt input_tokens and nests the cached part under
// input_tokens_details, on the body and on its completed event alike.
func TestOpenAIResponsesShape(t *testing.T) {
	event := []byte(`{"type":"response.completed","response":{"output":[],"usage":{"input_tokens":30,"output_tokens":5,` +
		`"total_tokens":35,"input_tokens_details":{"cached_tokens":10}}}}`)
	u, ok := openai.Event(event)
	if !ok || u.Prompt != 30 || u.Completion != 5 || u.Total != 35 || u.Cached != 10 {
		t.Fatalf("got %+v ok=%v (want the cached part read off input_tokens_details)", u, ok)
	}
	body := []byte(`{"id":"resp_1","usage":{"input_tokens":30,"output_tokens":5,"total_tokens":35}}`)
	if u, ok := openai.Document(body); !ok || u.Total != 35 {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
}

func TestAnthropicDocument(t *testing.T) {
	// Anthropic non-streaming: input_tokens/output_tokens, no total.
	u, ok := anthropic.Document([]byte(`{"type":"message","usage":{"input_tokens":30,"output_tokens":5}}`))
	if !ok || u.Prompt != 30 || u.Completion != 5 || u.Total != 35 {
		t.Fatalf("got %+v ok=%v (total should derive as 35)", u, ok)
	}
}

func TestAnthropicCache(t *testing.T) {
	// Live vLLM native /v1/messages shape: input_tokens EXCLUDES cache; cache_read is separate; no
	// total_tokens. The cache is folded back in so Prompt=617, Total=622, Cached=608 — same as the
	// OpenAI shape.
	raw := []byte(`{"usage":{"input_tokens":9,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":608}}`)
	u, ok := anthropic.Document(raw)
	if !ok || u.Prompt != 617 || u.Completion != 5 || u.Total != 622 || u.Cached != 608 {
		t.Fatalf("got %+v ok=%v (want Prompt=617 Total=622 Cached=608)", u, ok)
	}
	// billable = Total - Cached must equal the non-cached input + output.
	if u.Total-u.Cached != 14 {
		t.Fatalf("billable %d, want 14", u.Total-u.Cached)
	}
}

func TestAnthropicCacheCreation(t *testing.T) {
	// cache_creation is a WRITE: counts in Prompt/Total (billed) but NOT credited.
	u, ok := anthropic.Document([]byte(`{"usage":{"input_tokens":10,"output_tokens":2,"cache_creation_input_tokens":100,"cache_read_input_tokens":0}}`))
	if !ok || u.Prompt != 110 || u.Total != 112 || u.Cached != 0 {
		t.Fatalf("got %+v ok=%v (want Prompt=110 Total=112 Cached=0)", u, ok)
	}
}

// The cache writes, split by TTL, come off the Anthropic shape with or without the nested
// breakdown; the hour bucket can never exceed the writes it is part of.
func TestAnthropicCacheWrites(t *testing.T) {
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
		{"creation without read still folds", `{"usage":{"input_tokens":10,"output_tokens":2,"cache_creation_input_tokens":20}}`,
			20, 0, 30, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, ok := anthropic.Document([]byte(tc.raw))
			if !ok || u.CacheWrite != tc.write || u.CacheWrite1h != tc.hour || u.Prompt != tc.prompt || u.Cached != tc.read {
				t.Fatalf("got %+v ok=%v, want write=%d hour=%d prompt=%d cached=%d", u, ok, tc.write, tc.hour, tc.prompt, tc.read)
			}
			if u.Completion != 2 || u.Total != tc.prompt+2 {
				t.Fatalf("got %+v, want completion 2 and total %d", u, tc.prompt+2)
			}
		})
	}
}

// An Anthropic stream: the prompt and its cache buckets on message_start, the output on
// message_delta. Merged, the request is whole. A count_tokens answer is the prompt alone.
func TestAnthropicStreamEventsMerge(t *testing.T) {
	start, ok := anthropic.Event([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":66,` +
		`"cache_read_input_tokens":2,"cache_creation_input_tokens":5,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":3},"output_tokens":1}}}`))
	if !ok {
		t.Fatal("message_start carried no usage")
	}
	delta, ok := anthropic.Event([]byte(`{"type":"message_delta","usage":{"output_tokens":26}}`))
	if !ok {
		t.Fatal("message_delta carried no usage")
	}
	want := Usage{Prompt: 73, Completion: 26, Total: 99, Cached: 2, CacheWrite: 5, CacheWrite1h: 3}
	if got := MergeUsage(start, delta); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if u, ok := anthropic.Document([]byte(`{"input_tokens":123}`)); !ok || u.Prompt != 123 || u.Total != 123 {
		t.Fatalf("count_tokens: got %+v ok=%v", u, ok)
	}
}

// What is not usage: a chunk before the final frame, OpenAI's null on every chunk before it, a
// body with none, an event name, garbage.
func TestWhatCarriesNoUsage(t *testing.T) {
	for name, tc := range map[string]struct {
		adapter UsageAdapter
		event   bool
		raw     string
	}{
		"delta":         {openai, true, `{"choices":[{"delta":{"content":"hi"}}]}`},
		"null":          {openai, true, `{"choices":[{"delta":{"content":"hi"}}],"usage":null}`},
		"content block": {anthropic, true, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`},
		"half a frame":  {openai, true, `{"usage":{"prompt_tok`},
		"no usage":      {openai, false, `{"text":"hi"}`},
		"garbage":       {openai, false, "not json"},
		"garbage event": {anthropic, true, "not json"},
	} {
		var ok bool
		if tc.event {
			_, ok = tc.adapter.Event([]byte(tc.raw))
		} else {
			_, ok = tc.adapter.Document([]byte(tc.raw))
		}
		if ok {
			t.Errorf("%s: usage found in %q", name, tc.raw)
		}
	}
}

// What the reader hands over for a body past its carry limit: the front is gone, so this is no
// longer a document, but the usage object at the tail is whole.
func TestADocumentCutAtTheFrontIsReadFromItsTail(t *testing.T) {
	raw := []byte(`lo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":617,"completion_tokens":5,"total_tokens":622,"prompt_tokens_details":{"cached_tokens":608}},"prompt_logprobs":null}`)
	u, ok := openai.Document(raw)
	if !ok || u.Prompt != 617 || u.Completion != 5 || u.Total != 622 || u.Cached != 608 {
		t.Fatalf("got %+v ok=%v (a truncated body must not bill as zero)", u, ok)
	}
	raw = []byte(`"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":608}}`)
	u, ok = anthropic.Document(raw)
	if !ok || u.Prompt != 617 || u.Total != 622 || u.Cached != 608 {
		t.Fatalf("got %+v ok=%v (want the Anthropic fold: Prompt=617 Total=622 Cached=608)", u, ok)
	}
	// A token whose text is "usage" is a string VALUE after the real key, and an escaped one inside
	// a string never matches at all. Neither may shadow the object.
	raw = []byte(`t":"say \"usage\": now"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7},"prompt_logprobs":[{"token":"usage"}]}`)
	if u, ok := openai.Document(raw); !ok || u.Total != 7 {
		t.Fatalf("got %+v ok=%v (want Total=7)", u, ok)
	}
	for _, raw := range []string{
		`lo"},"finish_reason":"stop"}],"usage":null}`,
		`lo"},"finish_reason":"stop"}]}`,
		`"}],"usage":{"prompt_tok`,
	} {
		if u, ok := openai.Document([]byte(raw)); ok {
			t.Errorf("%s: got %+v, want ok=false", raw, u)
		}
	}
}

// A transcription's usage is a duration: the seconds land and no token field is invented, off
// the usage object vLLM and whisper-1 send or the top-level float a verbose_json body carries.
func TestDuration(t *testing.T) {
	for name, raw := range map[string]string{
		"usage object":    `{"text":"hi","usage":{"type":"duration","seconds":12}}`,
		"verbose_json":    `{"task":"transcribe","duration":11.2,"text":"hi","segments":[]}`,
		"streamed object": `data: {"type":"transcript.text.done","text":"hi","usage":{"type":"duration","seconds":12}}`,
	} {
		t.Run(name, func(t *testing.T) {
			u, ok := openai.Event(TrimSSE([]byte(raw)))
			if !ok || u.Seconds != 12 {
				t.Fatalf("got %+v ok=%v, want Seconds=12", u, ok)
			}
			if u.Prompt != 0 || u.Completion != 0 || u.Total != 0 {
				t.Fatalf("got %+v, want no token fields", u)
			}
		})
	}
	// Token-shaped transcription usage prices as tokens, and a body with neither is still no usage.
	u, ok := openai.Document([]byte(`{"duration":3.5,"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	if !ok || u.Total != 3 || u.Seconds != 0 {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
	u, ok = openai.Document([]byte(`{"usage":{"type":"tokens","input_tokens":14,"output_tokens":45,"total_tokens":59,` +
		`"input_token_details":{"text_tokens":4,"audio_tokens":10}}}`))
	if !ok || u.Prompt != 14 || u.Completion != 45 || u.Audio != 10 {
		t.Fatalf("got %+v ok=%v (want the audio part off input_token_details)", u, ok)
	}
}
