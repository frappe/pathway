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

// vendorPaths are the paths a vendor serves, each owned by one dialect's surface. A new
// surface for a dialect (say /v1/responses for OpenAI) is a row here.
var vendorPaths = map[string]string{
	"/v1/chat/completions": DialectOpenAI,
	"/v1/messages":         DialectAnthropic,
}

// PathDialect is the dialect that owns a vendor path, "" for any other path.
func PathDialect(path string) string {
	return vendorPaths[strings.TrimRight(path, "/")]
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
	case 402:
		return "billing_error"
	case 429:
		return "rate_limit_error"
	case 503, 529:
		return "overloaded_error"
	}
	return "api_error"
}
