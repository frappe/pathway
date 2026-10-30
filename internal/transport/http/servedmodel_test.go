package http

import (
	"net/http"
	"strings"
	"testing"
)

// The line names the model the vendor answered under when that is not the one the client knows:
// the client's body still says its own id, the swap being untouched. Answered under the client's
// id, the field is `-`.
func TestTheLineNamesTheModelTheVendorAnsweredUnder(t *testing.T) {
	for _, tc := range []struct{ answered, want string }{
		{"claude-sonnet-4-5-20250929", "claude-sonnet-4-5-20250929"},
		{"anthropic/claude-4-5", "-"},
	} {
		f := providerFixtureAnswering(t, jsonEngine(`{"model":"`+tc.answered+`",`+usageObject+`}`))
		access := captureAccess(t, f)
		resp := f.post("/anthropic/v1/messages", `{"model":"anthropic/claude-4-5","max_tokens":16}`)
		if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"model":"anthropic/claude-4-5"`) {
			t.Fatalf("status = %d, body = %s; want the client's own id in the body", resp.Code, resp.Body)
		}
		if line := jsonLine(t, access); line["served_model"] != tc.want {
			t.Errorf("answered under %q: served_model = %v, want %q", tc.answered, line["served_model"], tc.want)
		}
	}
}
