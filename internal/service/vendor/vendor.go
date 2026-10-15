// Package vendor is what the gateway knows of each third party it dials: the headers it reads the
// credential from, and the fields it reads differently from the shape it claims to speak. One
// value per vendor, by the name the control plane pushes; a vendor the gateway has not met gets
// Default. What a vendor is reached at, speaks, and answers to is the route row, not this.
package vendor

import "github.com/frappe/pathway/internal/domain"

// Vendor is how one third party differs from the shape it claims to speak.
type Vendor interface {
	// Headers carries the credential the way the vendor reads it, by the front's dialect. A blank
	// secret sends no credential header; the version header, when the row has one, goes regardless.
	Headers(dialect, secret, apiVersion string) map[string]string
	Quirks() Quirks
}

// The two names of the output cap on the OpenAI chat shape. An upstream reads one of them.
const (
	MaxTokens           = "max_tokens"
	MaxCompletionTokens = "max_completion_tokens"
	// AsSent leaves the cap under whichever name the client wrote.
	AsSent = ""
)

// Quirks is what a vendor reads differently. Each value was measured through a gateway: the
// README's "What each upstream was seen to do" says when, and vendor_quirks.md shows each rewrite.
type Quirks struct {
	// OutputCap is the field the upstream reads the output cap from, on the OpenAI shape.
	OutputCap string
	// UsagePerChunk asks a stream for usage on every chunk (`continuous_usage_stats`), so one that
	// is cut is still billed for what it had generated.
	UsagePerChunk bool
	// UntypedTools sends a caller's own tool without `"type":"custom"`, on the Anthropic shape. A
	// tool with no type means the same.
	UntypedTools bool
	// DeveloperAsSystem sends a `developer` message as a `system` one, on the OpenAI shape.
	DeveloperAsSystem bool
	// BlankReasoning gives a past tool-call turn that came back without its reasoning an empty one:
	// `reasoning_content` on the OpenAI shape, a thinking block on the Anthropic one. The upstream
	// refuses the turn without it while it thinks, and takes it empty.
	BlankReasoning bool
	// UnthinkForcedTool switches thinking off when the caller forces a tool and said nothing about
	// thinking. The upstream refuses a forced tool while it thinks, which is its default.
	UnthinkForcedTool bool
	// UnreasonWithTools switches reasoning off (`reasoning_effort: "none"`) when the request has
	// tools and the caller set no effort, on the OpenAI shape. The upstream refuses a function tool
	// while the model reasons, which is its default.
	UnreasonWithTools bool
	// DefaultSampling drops `temperature`, `top_p`, `logprobs` and `top_logprobs` unless reasoning
	// is off, on the OpenAI shape. The upstream refuses them while the model reasons.
	DefaultSampling bool
}

// base is the vendor every other is a change to: the credential where the dialect's own API reads
// it, and the cap under the name every vendor but OpenAI was seen to read.
type base struct{ quirks Quirks }

// Headers: an Anthropic front takes `x-api-key` and its version header; an OpenAI-compatible one
// takes the Bearer everyone else does.
func (v base) Headers(dialect, secret, apiVersion string) map[string]string {
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

func (v base) Quirks() Quirks { return v.quirks }

// Default is a third party the table does not name: no option one of the named refuses.
var Default Vendor = base{Quirks{
	OutputCap: MaxTokens, UsagePerChunk: false,
	UntypedTools: false, DeveloperAsSystem: false, BlankReasoning: false, UnthinkForcedTool: false,
	UnreasonWithTools: false, DefaultSampling: false,
}}

// baseten's Anthropic front reads the key as a Bearer and 401s on the `x-api-key` header
// Anthropic's own takes (2026-10-05). The version header goes as for any Anthropic front.
type baseten struct{ base }

func (v baseten) Headers(dialect, secret, apiVersion string) map[string]string {
	headers := v.base.Headers(domain.DialectOpenAI, secret, apiVersion)
	if dialect == domain.DialectAnthropic && apiVersion != "" {
		headers["anthropic-version"] = apiVersion
	}
	return headers
}

// known is each third party the gateway has met, by the provider's name as the control plane
// pushes it. Every flag is written out on every entry, so a new flag is a visible decision per
// vendor rather than a zero value. Anthropic is reached on its own shape only, where none of this
// applies, so it is Default.
var known = map[string]Vendor{
	// 400s on `max_tokens`, on `continuous_usage_stats`, and while the model reasons on a function
	// tool, a temperature or `top_p` other than 1, and `logprobs`.
	"openai": base{Quirks{
		OutputCap: MaxCompletionTokens, UsagePerChunk: false,
		UntypedTools: false, DeveloperAsSystem: false, BlankReasoning: false, UnthinkForcedTool: false,
		UnreasonWithTools: true, DefaultSampling: true,
	}},
	// Ignores `continuous_usage_stats`: its usage comes on the last chunk only. Refuses a tool typed
	// `custom`, a `developer` message, a forced tool while thinking, and a past tool call sent back
	// with no reasoning.
	"deepseek": base{Quirks{
		OutputCap: MaxTokens, UsagePerChunk: false,
		UntypedTools: true, DeveloperAsSystem: true, BlankReasoning: true, UnthinkForcedTool: true,
		UnreasonWithTools: false, DefaultSampling: false,
	}},
	// Reports usage on every chunk asked or not; asked all the same, so it stays that way.
	"baseten": baseten{base{Quirks{
		OutputCap: MaxTokens, UsagePerChunk: true,
		UntypedTools: false, DeveloperAsSystem: false, BlankReasoning: false, UnthinkForcedTool: false,
		UnreasonWithTools: false, DefaultSampling: false,
	}}},
}

// For is the vendor named, or Default for one the gateway has not met.
func For(name string) Vendor {
	if v, met := known[name]; met {
		return v
	}
	return Default
}
