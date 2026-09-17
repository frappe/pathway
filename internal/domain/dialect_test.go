package domain

import (
	"strings"
	"testing"
)

// The dialect a path declares. Everything outside the two chat pairs stays with the modality
// rules, which is what keeps "unknown format errored" the standing behaviour.
func TestClientDialect(t *testing.T) {
	for path, want := range map[string]string{
		"/v1/chat/completions":      DialectOpenAI,
		"/v1/completions":           DialectOpenAI,
		"/v1/messages":              DialectAnthropic,
		"/v1/messages/":             DialectAnthropic,
		"/v1/messages/count_tokens": DialectAnthropic,
		"/v1/embeddings":            "",
		"/v1/responses":             "",
		"/":                         "",
	} {
		if got := ClientDialect(path); got != want {
			t.Errorf("ClientDialect(%q) = %q, want %q", path, got, want)
		}
	}
}

// Blank means BOTH on a row we run — a vLLM engine really does answer both surfaces — including
// routes pushed before the field existed.
func TestSpeaksDialectBlankMeansBoth(t *testing.T) {
	for _, kind := range []string{"direct", "ingress", ""} {
		r := Route{Kind: kind}
		if !r.SpeaksDialect(DialectAnthropic) || !r.SpeaksDialect(DialectOpenAI) {
			t.Errorf("blank dialect on kind %q must mean both", kind)
		}
	}
	if (Route{Kind: "provider", Dialect: "anthropic"}).SpeaksDialect(DialectOpenAI) {
		t.Error("an explicit dialect must exclude the other")
	}
	if !(Route{Kind: "provider", Dialect: "openai"}).SpeaksDialect(DialectOpenAI) {
		t.Error("an explicit dialect was ignored")
	}
}

// A vendor row without a dialect is malformed: guessing would forward one surface's request to the
// other's front with the wrong credential, so it answers neither.
func TestABlankDialectVendorSpeaksNothing(t *testing.T) {
	r := Route{Kind: "provider"}
	if r.SpeaksDialect(DialectAnthropic) || r.SpeaksDialect(DialectOpenAI) {
		t.Error("a vendor row with no dialect must serve no surface")
	}
}

func TestAnthropicErrorSpeaksTheSDKsShape(t *testing.T) {
	out := AnthropicError(429, []byte(`{"error":{"message":"slow down","type":"rate_limit"}}`))
	if string(out) != `{"error":{"message":"slow down","type":"rate_limit_error"},"type":"error"}` {
		t.Errorf("envelope = %s", out)
	}
	out = AnthropicError(500, []byte("upstream exploded"))
	if !strings.Contains(string(out), `"type":"api_error"`) ||
		!strings.Contains(string(out), "upstream exploded") {
		t.Errorf("unparseable body must still carry its text: %s", out)
	}
}
