package domain

import (
	"encoding/json"
	"strings"
)

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

// AnthropicError renders an upstream failure in the shape an Anthropic SDK parses. The type
// comes from the status — the one signal every OpenAI-compatible vendor agrees on.
func AnthropicError(status int, raw []byte) []byte {
	out, _ := json.Marshal(anthropicErrorObject(status, raw))
	return out
}

func anthropicErrorObject(status int, raw []byte) map[string]any {
	message := strings.TrimSpace(string(raw))
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Message != "" {
		message = envelope.Error.Message
	}
	return map[string]any{
		"type":  "error",
		"error": map[string]any{"type": AnthropicErrorType(status), "message": message},
	}
}

// AnthropicErrorType names a status the way the Anthropic API does. Exported for the transport
// layer's own refusals, which must speak the surface's dialect too.
func AnthropicErrorType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 503, 529:
		return "overloaded_error"
	}
	return "api_error"
}
