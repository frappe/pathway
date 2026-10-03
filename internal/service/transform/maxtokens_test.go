package transform

import "testing"

// A vendor gets the cap under the one name it honours, and an engine we run gets the body as sent.
func TestMaxTokensIsNamedForTheVendor(t *testing.T) {
	for _, tc := range []struct {
		vendor, raw, old, renamed string
	}{
		{"openai", `{"max_tokens":16}`, "", "16"},
		{"openai", `{"max_completion_tokens":8}`, "", "8"},
		{"openai", `{"max_tokens":16,"max_completion_tokens":8}`, "", "8"},
		{"deepseek", `{"max_tokens":16}`, "16", ""},
		{"deepseek", `{"max_completion_tokens":8}`, "8", ""},
		{"deepseek", `{"max_tokens":16,"max_completion_tokens":8}`, "8", ""},
		{"", `{"max_tokens":16}`, "16", ""},
		{"", `{"max_completion_tokens":8}`, "", "8"},
	} {
		body := decode(t, tc.raw)
		if _, err := (maxTokens{}).Apply(Context{Vendor: tc.vendor}, body); err != nil {
			t.Fatal(err)
		}
		if got := string(body["max_tokens"]); got != tc.old {
			t.Errorf("%q %s: max_tokens = %q, want %q", tc.vendor, tc.raw, got, tc.old)
		}
		if got := string(body["max_completion_tokens"]); got != tc.renamed {
			t.Errorf("%q %s: max_completion_tokens = %q, want %q", tc.vendor, tc.raw, got, tc.renamed)
		}
	}
}

// Registered is not running: the rename only happens while the default list names it.
func TestMaxTokensRunsByDefault(t *testing.T) {
	body := decode(t, `{"max_tokens":16}`)
	chain(t, Default...).Apply(Context{Path: "/v1/chat/completions", Vendor: "openai"}, body)
	if _, present := body["max_tokens"]; present {
		t.Error("max_tokens survived the default chain")
	}
}
