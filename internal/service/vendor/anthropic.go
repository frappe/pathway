package vendor

import "github.com/frappe/pathway/internal/domain"

// AnthropicVersion is the one value Anthropic's required version header has had since 2023.
const AnthropicVersion = "2023-06-01"

// anthropic is Anthropic's own front, reached on its own shape only: the one front that requires
// `anthropic-version`. Every other Anthropic-shaped front gets the header from a row, if at all.
type anthropic struct{ base }

// Headers: the shape's, plus the version header Anthropic refuses a request without.
func (v anthropic) Headers(dialect, secret string) map[string]string {
	headers := v.base.Headers(dialect, secret)
	if dialect == domain.DialectAnthropic {
		headers["anthropic-version"] = AnthropicVersion
	}
	return headers
}
