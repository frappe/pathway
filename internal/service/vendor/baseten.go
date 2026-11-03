package vendor

import (
	"strconv"

	"github.com/frappe/pathway/internal/domain"
)

// baseten reports usage on every chunk asked or not, and its Anthropic front reads the key as a
// Bearer: it 401s on the `x-api-key` header Anthropic's own takes (2026-10-05). It names its
// request id and its own retries under headers of its own.
type baseten struct{ base }

// Headers: the Bearer on either front; the version header goes as for any Anthropic front.
func (v baseten) Headers(dialect, secret, apiVersion string) map[string]string {
	headers := v.base.Headers(domain.DialectOpenAI, secret, apiVersion)
	if dialect == domain.DialectAnthropic && apiVersion != "" {
		headers["anthropic-version"] = apiVersion
	}
	return headers
}

// UsagePerChunk: asked all the same, so it stays that way.
func (baseten) UsagePerChunk() bool { return true }

func (baseten) Quirks() []string { return []string{"name_output_cap", QuirkUsagePerChunk} }

// Answered: X-Baseten-Request-Id when the usual names are blank, and how many times Baseten ran the
// request on its side, off X-Baseten-Model-Prediction-Attempts.
func (v baseten) Answered(status int, header Header) Answer {
	answer := v.base.Answered(status, header)
	if answer.UpstreamRID == "" {
		answer.UpstreamRID = header.Get("X-Baseten-Request-Id")
	}
	answer.UpstreamAttempts, _ = strconv.Atoi(header.Get("X-Baseten-Model-Prediction-Attempts"))
	return answer
}
