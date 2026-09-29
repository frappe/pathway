package domain

import "testing"

// A model is just a name in the route table, so nothing but its modality distinguishes an ASR
// container from a chat engine. These are the rules that keep a transcription off a chat model.

func TestServes(t *testing.T) {
	for _, c := range []struct {
		modality string
		path     string
		want     bool
		why      string
	}{
		{"text", "/v1/chat/completions", true, "the ordinary case"},
		{"text", "/v1/completions", true, "legacy completions is still text"},
		{"multimodal", "/v1/chat/completions", true, "images ride inside the chat body"},
		{"embedding", "/v1/embeddings", true, ""},
		{"audio", "/v1/audio/transcriptions", true, ""},
		{"audio", "/v1/audio/translations", true, ""},
		{"multimodal", "/v1/audio/transcriptions", true, "a multimodal model takes every input surface"},
		{"multimodal", "/v1/audio/translations", true, ""},
		{"multimodal", "/v1/embeddings", true, ""},

		{"text", "/v1/audio/transcriptions", false, "a chat engine would 404 this after a round trip"},
		{"audio", "/v1/chat/completions", false, "an ASR box cannot hold a conversation"},
		{"embedding", "/v1/chat/completions", false, ""},
		{"text", "/v1/embeddings", false, ""},

		// /v1/messages is the same request as /v1/chat/completions in Anthropic's spelling, so it
		// is claimed by the same modalities. Enforcing one spelling and not the other was an
		// omission, not a policy.
		{"text", "/v1/messages", true, ""},
		{"multimodal", "/v1/messages", true, ""},
		{"embedding", "/v1/messages", false, "a pooling model cannot hold a conversation either way"},
		{"audio", "/v1/messages", false, ""},

		// Generosity is deliberate: this refuses only what another modality has claimed.
		{"text", "/v1/rerank", true, "engines serve more than the OpenAI core"},
		{"audio", "/tokenize", true, "an unclaimed path is nobody's to refuse"},
		{"embedding", "/v1/score", true, ""},

		// Never take traffic away over a value this build does not recognise.
		{"", "/v1/audio/transcriptions", true, "blank = a control plane predating the field"},
		{"   ", "/v1/audio/transcriptions", true, "blank after trimming is still blank"},
		{"video", "/v1/chat/completions", true, "newer control plane, older binary — must not refuse"},

		{"audio", "/v1/audio/transcriptions/", true, "a trailing slash is the same endpoint"},
	} {
		if got := Serves(c.modality, c.path); got != c.want {
			t.Errorf("Serves(%q, %q) = %v, want %v — %s", c.modality, c.path, got, c.want, c.why)
		}
	}
}

// A vendor is a closed set: only its own chat dialect exists there, so anything else is our 404
// rather than a round trip that comes back as theirs. The engine table is generous for the
// opposite reason — an engine serves more than we have written down.
func TestServesRoute(t *testing.T) {
	vendor := Route{Kind: "provider", Modality: "text", Dialect: DialectAnthropic}
	openaiVendor := Route{Kind: "provider", Modality: "text", Dialect: DialectOpenAI}
	blankVendor := Route{Kind: "provider", Modality: "text"}
	engine := Route{Kind: "direct", Modality: "text"}

	for _, c := range []struct {
		name  string
		route Route
		path  string
		want  bool
		why   string
	}{
		{"vendor", vendor, "/v1/messages", true, "the surface the dialect exists for"},
		{"vendor", vendor, "/v1/messages/count_tokens", true, ""},
		{"vendor", vendor, "/v1/messages/", true, "a trailing slash is the same endpoint"},
		{"vendor", vendor, "/v1/chat/completions", false, "nothing translates"},
		{"vendor", vendor, "/v1/embeddings", false, ""},
		{"vendor", vendor, "/v1/rerank", false, "unclaimed is allowed on an engine and refused on a vendor"},
		{"vendor", vendor, "/tokenize", false, ""},

		{"openai-vendor", openaiVendor, "/v1/chat/completions", true, ""},
		{"openai-vendor", openaiVendor, "/v1/completions", true, ""},
		{"openai-vendor", openaiVendor, "/v1/messages", false, "nothing translates"},

		{"blank-vendor", blankVendor, "/v1/chat/completions", false, "a vendor row without a dialect serves nothing"},
		{"blank-vendor", blankVendor, "/v1/messages", false, ""},

		{"engine", engine, "/v1/rerank", true, "engines still serve more than the OpenAI core"},
		{"engine", engine, "/v1/chat/completions", true, ""},
		{"engine", engine, "/v1/messages", true, "vLLM answers both dialects natively"},
		{"engine", engine, "/v1/embeddings", false, "a text engine is still held to its modality"},
	} {
		if got := ServesRoute(c.route, c.path); got != c.want {
			t.Errorf("ServesRoute(%s, %q) = %v, want %v — %s", c.name, c.path, got, c.want, c.why)
		}
	}
}

// A route pushed before the vendor split has no Kind, and must keep being read as one of ours.
func TestABlankKindIsStillAnEngine(t *testing.T) {
	if !ServesRoute(Route{Modality: "text"}, "/v1/rerank") {
		t.Error("a route with no kind was treated as a vendor")
	}
}
