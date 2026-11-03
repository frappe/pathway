package vendor

import (
	"io"
	"strconv"

	"github.com/frappe/pathway/internal/domain"
)

// base is the vendor every other is a change to: each method is what the shape says, so a vendor
// type embeds it and overrides what it does differently. No method here calls another through its
// receiver: embedding does not dispatch to an override, the drivers in vendor.go do.
type base struct{}

// Headers: an Anthropic front takes `x-api-key` and its version header; an OpenAI-compatible one
// takes the Bearer everyone else does.
func (base) Headers(dialect, secret, apiVersion string) map[string]string {
	headers := map[string]string{}
	anthropic := dialect == domain.DialectAnthropic
	if anthropic && apiVersion != "" {
		headers["anthropic-version"] = apiVersion
	}
	switch {
	case secret == "":
	case anthropic:
		headers["x-api-key"] = secret
	default:
		headers["Authorization"] = "Bearer " + secret
	}
	return headers
}

// NameOutputCap: every vendor but OpenAI was seen to read `max_tokens`; sent the newer name alone,
// the output is not capped.
func (base) NameOutputCap(req *Request) (bool, error) { return renameOutputCap(req, MaxTokens) }

func (base) DeveloperAsSystem(*Request) (bool, error)     { return false, nil }
func (base) UnthinkForcedFunction(*Request) (bool, error) { return false, nil }
func (base) BlankReasoningContent(*Request) (bool, error) { return false, nil }
func (base) UnreasonWithTools(*Request) (bool, error)     { return false, nil }
func (base) DefaultSampling(*Request) (bool, error)       { return false, nil }
func (base) UntypeCustomTools(*Request) (bool, error)     { return false, nil }
func (base) UnthinkForcedTool(*Request) (bool, error)     { return false, nil }
func (base) BlankThinkingBlock(*Request) (bool, error)    { return false, nil }

func (base) UsagePerChunk() bool { return false }

// Quirks: the cap's name is every third party's; nothing else here changes a body.
func (base) Quirks() []string { return []string{"name_output_cap"} }

// Answered reads what either shape's own vendor puts in its headers, which a compatible front
// copies: the request id under OpenAI's name or Anthropic's, OpenAI's own timing of the hop, and
// on a refusal its back-off and the headroom that ran out, under either shape's name.
func (base) Answered(status int, header Header) Answer {
	answer := Answer{UpstreamRID: first(header, "X-Request-Id", "Request-Id")}
	answer.UpstreamMS, _ = strconv.Atoi(header.Get("Openai-Processing-Ms"))
	if status >= 400 {
		answer.RetryAfter = header.Get("Retry-After")
		answer.RemainingRequests = first(header, "X-Ratelimit-Remaining-Requests", "Anthropic-Ratelimit-Requests-Remaining")
		answer.RemainingTokens = first(header, "X-Ratelimit-Remaining-Tokens", "Anthropic-Ratelimit-Tokens-Remaining")
	}
	return answer
}

// first is the first of `names` the header carries, blank when none.
func first(header Header, names ...string) string {
	for _, name := range names {
		if value := header.Get(name); value != "" {
			return value
		}
	}
	return ""
}

// Peeks: every 4xx, since a key, an account, a model and a region refusal each hide under one or
// another of them (Classify).
func (base) Peeks(status int) bool { return status >= 400 && status < 500 }

// Classify: what the answer's words and status say, in the names every vendor's are read by
// (domain.FailureClass). A vendor whose words are not among them says so here.
func (base) Classify(status int, body []byte) string { return domain.FailureClass(status, body) }

// Relay: the body as it came. Responses are not rewritten; a vendor that writes something the
// shape does not would wrap the reader here.
func (base) Relay(_ int, _ bool, body io.ReadCloser) io.ReadCloser { return body }
