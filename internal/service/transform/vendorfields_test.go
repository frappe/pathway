package transform

import "testing"

// A vendor gets the cap under the one name it honours, and an engine we run gets the body as sent.
func TestTheOutputCapIsNamedForTheVendor(t *testing.T) {
	for _, tc := range []struct {
		vendor, raw, old, renamed string
		changed                   bool
	}{
		{"openai", `{"max_tokens":16}`, "", "16", true},
		{"openai", `{"max_completion_tokens":8}`, "", "8", false},
		{"openai", `{"max_tokens":16,"max_completion_tokens":8}`, "", "8", true},
		{"deepseek", `{"max_tokens":16}`, "16", "", false},
		{"deepseek", `{"max_completion_tokens":8}`, "8", "", true},
		{"deepseek", `{"max_tokens":16,"max_completion_tokens":8}`, "8", "", true},
		{"baseten", `{"max_tokens":16}`, "16", "", false},
		{"baseten", `{"max_completion_tokens":8}`, "8", "", true},
		{"a vendor the table does not name", `{"max_completion_tokens":8}`, "8", "", true},
		{"", `{"max_tokens":16}`, "16", "", false},
		{"", `{"max_completion_tokens":8}`, "", "8", false},
	} {
		body := decode(t, tc.raw)
		changed, err := (vendorFields{}).Apply(Context{Path: "/v1/chat/completions", Provider: tc.vendor != "", Vendor: tc.vendor}, body)
		if err != nil {
			t.Fatal(err)
		}
		if changed != tc.changed {
			t.Errorf("%q %s: changed = %v, want %v", tc.vendor, tc.raw, changed, tc.changed)
		}
		if got := string(body["max_tokens"]); got != tc.old {
			t.Errorf("%q %s: max_tokens = %q, want %q", tc.vendor, tc.raw, got, tc.old)
		}
		if got := string(body["max_completion_tokens"]); got != tc.renamed {
			t.Errorf("%q %s: max_completion_tokens = %q, want %q", tc.vendor, tc.raw, got, tc.renamed)
		}
	}
}

// The table is about the OpenAI chat shape: a body of the Anthropic shape is sent as it came.
func TestTheMessagesShapeIsSentAsItCame(t *testing.T) {
	body := decode(t, `{"max_tokens":16,"max_completion_tokens":8}`)
	changed, err := chain(t, "vendorfields").Apply(Context{Path: "/v1/messages", Provider: true, Vendor: "deepseek"}, body)
	if err != nil || changed || len(body) != 2 {
		t.Errorf("changed=%v err=%v body = %v", changed, err, body)
	}
}

// Registered is not running: the rename only happens while the default list names it.
func TestVendorFieldsRunsByDefault(t *testing.T) {
	body := decode(t, `{"max_tokens":16}`)
	chain(t, Default...).Apply(Context{Path: "/v1/chat/completions", Provider: true, Vendor: "openai"}, body)
	if _, present := body["max_tokens"]; present {
		t.Error("max_tokens survived the default chain")
	}
}
