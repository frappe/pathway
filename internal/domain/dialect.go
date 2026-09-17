package domain

import "strings"

// The two dialects this gateway fronts. The surface a request arrives on IS the shape its upstream
// must speak, and nothing translates between them: a model whose every placement speaks the other
// shape is a 404 on this surface, the same as any path a route does not serve.
const (
	DialectOpenAI    = "openai"
	DialectAnthropic = "anthropic"
)

// ClientDialect is the dialect a surface path declares. "" for every path outside the two
// chat-shaped surfaces — those keep their existing modality/404 rules.
func ClientDialect(path string) string {
	switch strings.TrimRight(path, "/") {
	case "/v1/chat/completions", "/v1/completions":
		return DialectOpenAI
	case "/v1/messages", "/v1/messages/count_tokens":
		return DialectAnthropic
	}
	return ""
}
