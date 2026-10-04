package domain

import "testing"

// A model is just a name in the route table, so nothing but what it gives distinguishes an ASR
// container from a chat engine. These are the rules that keep a transcription off a chat model.

func TestServes(t *testing.T) {
	chat, pooling, asr := []string{"text"}, []string{"embeddings"}, []string{"transcription"}
	for _, c := range []struct {
		outputs []string
		path    string
		want    bool
		why     string
	}{
		{chat, "/v1/chat/completions", true, "the ordinary case"},
		{chat, "/v1/completions", true, "legacy completions is unclaimed"},
		{[]string{"text", "image"}, "/v1/chat/completions", true, "a model that also draws still talks"},
		{pooling, "/v1/embeddings", true, ""},
		{asr, "/v1/audio/transcriptions", true, ""},
		{asr, "/v1/audio/translations", true, ""},

		{chat, "/v1/audio/transcriptions", false, "a chat engine would 404 this after a round trip"},
		{chat, "/v1/embeddings", false, "nor is it a pooling model"},
		{asr, "/v1/chat/completions", false, "an ASR box cannot hold a conversation"},
		{pooling, "/v1/chat/completions", false, ""},
		{[]string{"image"}, "/v1/chat/completions", false, "a model that only draws gives no text"},

		// /v1/messages is the same request as /v1/chat/completions in Anthropic's spelling, so it
		// asks for the same output.
		{chat, "/v1/messages", true, ""},
		{pooling, "/v1/messages", false, "a pooling model cannot hold a conversation either way"},
		{asr, "/v1/messages", false, ""},

		// Generosity is deliberate: this refuses only a path some output has claimed.
		{chat, "/v1/rerank", true, "engines serve more than the OpenAI core"},
		{asr, "/tokenize", true, "an unclaimed path is nobody's to refuse"},
		{pooling, "/v1/score", true, ""},

		// Never take traffic away from a model that declares nothing.
		{nil, "/v1/audio/transcriptions", true, "nothing declared = a control plane predating the lists"},
		{[]string{}, "/v1/chat/completions", true, "an empty list is nothing declared"},

		{asr, "/v1/audio/transcriptions/", true, "a trailing slash is the same endpoint"},
	} {
		if got := Serves(c.outputs, c.path); got != c.want {
			t.Errorf("Serves(%v, %q) = %v, want %v — %s", c.outputs, c.path, got, c.want, c.why)
		}
	}
}

// A vendor is a closed set: only its own chat dialect exists there, so anything else is our 404
// rather than a round trip that comes back as theirs. The engine table is generous for the
// opposite reason — an engine serves more than we have written down.
func TestServesRoute(t *testing.T) {
	vendor := Route{Kind: "provider", OutputModalities: []string{"text"}, Dialect: DialectAnthropic}
	openaiVendor := Route{Kind: "provider", OutputModalities: []string{"text"}, Dialect: DialectOpenAI}
	blankVendor := Route{Kind: "provider", OutputModalities: []string{"text"}}
	engine := Route{Kind: "direct", OutputModalities: []string{"text"}}

	for _, c := range []struct {
		name    string
		route   Route
		dialect string
		path    string
		want    bool
		why     string
	}{
		{"vendor", vendor, DialectAnthropic, "/v1/messages", true, "the surface the dialect exists for"},
		{"vendor", vendor, DialectAnthropic, "/v1/messages/count_tokens", false, "no surface for it"},
		{"vendor", vendor, DialectAnthropic, "/v1/messages/", true, "a trailing slash is the same endpoint"},
		{"vendor", vendor, DialectOpenAI, "/v1/chat/completions", false, "nothing translates"},
		{"vendor", vendor, DialectOpenAI, "/v1/embeddings", false, ""},
		{"vendor", vendor, DialectOpenAI, "/v1/rerank", false, "unclaimed is allowed on an engine and refused on a vendor"},
		{"vendor", vendor, DialectOpenAI, "/tokenize", false, ""},

		{"openai-vendor", openaiVendor, DialectOpenAI, "/v1/chat/completions", true, ""},
		{"openai-vendor", openaiVendor, DialectOpenAI, "/v1/completions", false, "legacy completions is not a surface"},
		{"openai-vendor", openaiVendor, DialectAnthropic, "/v1/messages", false, "nothing translates"},

		{"blank-vendor", blankVendor, DialectOpenAI, "/v1/chat/completions", false, "a vendor row without a dialect serves nothing"},
		{"blank-vendor", blankVendor, DialectAnthropic, "/v1/messages", false, ""},

		{"engine", engine, DialectOpenAI, "/v1/rerank", true, "engines still serve more than the OpenAI core"},
		{"engine", engine, DialectOpenAI, "/v1/chat/completions", true, ""},
		{"engine", engine, DialectAnthropic, "/v1/messages", true, "vLLM answers both dialects natively"},
		{"engine", engine, DialectOpenAI, "/v1/embeddings", false, "a text engine is still held to what it gives"},
	} {
		if got := ServesRoute(c.route, c.dialect, c.path); got != c.want {
			t.Errorf("ServesRoute(%s, %s, %q) = %v, want %v — %s", c.name, c.dialect, c.path, got, c.want, c.why)
		}
	}
}

// A route pushed before the vendor split has no Kind, and must keep being read as one of ours.
func TestABlankKindIsStillAnEngine(t *testing.T) {
	if !ServesRoute(Route{OutputModalities: []string{"text"}}, DialectOpenAI, "/v1/rerank") {
		t.Error("a route with no kind was treated as a vendor")
	}
}
