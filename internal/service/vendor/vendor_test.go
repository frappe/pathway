package vendor

import (
	"maps"
	"testing"

	"github.com/frappe/pathway/internal/domain"
)

func TestHeadersFollowTheDialectUnlessTheVendorReadsOtherwise(t *testing.T) {
	for _, tc := range []struct {
		name, vendor, dialect, secret, version string
		want                                   map[string]string
	}{
		{"an unmet OpenAI front", "nobody", domain.DialectOpenAI, "k", "", map[string]string{"Authorization": "Bearer k"}},
		{"an unmet Anthropic front", "nobody", domain.DialectAnthropic, "k", "2023-06-01",
			map[string]string{"x-api-key": "k", "anthropic-version": "2023-06-01"}},
		{"no secret, the version still", "nobody", domain.DialectAnthropic, "", "2023-06-01",
			map[string]string{"anthropic-version": "2023-06-01"}},
		{"no secret, no version", "openai", domain.DialectOpenAI, "", "", map[string]string{}},
		{"Baseten's Anthropic front reads a Bearer", "baseten", domain.DialectAnthropic, "k", "2023-06-01",
			map[string]string{"Authorization": "Bearer k", "anthropic-version": "2023-06-01"}},
		{"Baseten's OpenAI front", "baseten", domain.DialectOpenAI, "k", "", map[string]string{"Authorization": "Bearer k"}},
	} {
		if got := For(tc.vendor).Headers(tc.dialect, tc.secret, tc.version); !maps.Equal(got, tc.want) {
			t.Errorf("%s: headers = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAnUnmetVendorGetsTheDefaults(t *testing.T) {
	if For("nobody") != Default {
		t.Error("an unmet vendor is not Default")
	}
	if q := For("openai").Quirks(); q.OutputCap != MaxCompletionTokens || !q.UnreasonWithTools {
		t.Errorf("openai quirks = %+v", q)
	}
	if q := Default.Quirks(); q.OutputCap != MaxTokens || q.UsagePerChunk {
		t.Errorf("default quirks = %+v", q)
	}
}
