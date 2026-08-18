package http

import "testing"

func TestOwnedByIsTheProviderPrefix(t *testing.T) {
	for id, want := range map[string]string{
		"anthropic/claude-opus-5": "anthropic",
		"frappe/qwen3-4b":         "frappe",
		"qwen3-4b":                "frappe",
		"a/b/c":                   "a",
	} {
		if got := ownerOf(id); got != want {
			t.Errorf("ownerOf(%q) = %q, want %q", id, got, want)
		}
	}
}
