package transform

// The two names of the output cap on the OpenAI chat shape. An upstream reads one of them.
const (
	maxTokens           = "max_tokens"
	maxCompletionTokens = "max_completion_tokens"
	// asSent leaves the cap under whichever name the client wrote.
	asSent = ""
)

// upstream is how one upstream is spoken to. Each value was measured through a gateway: the
// README's "What each upstream was seen to do" says when.
type upstream struct {
	// outputCap is the field the upstream reads the output cap from, on the OpenAI shape.
	outputCap string
	// usagePerChunk asks a stream for usage on every chunk (`continuous_usage_stats`), so one that
	// is cut is still billed for what it had generated.
	usagePerChunk bool
	// untypedTools sends a caller's own tool without `"type":"custom"`, on the Anthropic shape. A
	// tool with no type means the same.
	untypedTools bool
	// developerAsSystem sends a `developer` message as a `system` one, on the OpenAI shape.
	developerAsSystem bool
	// blankReasoning gives a past tool-call turn that came back without its reasoning an empty one:
	// `reasoning_content` on the OpenAI shape, a thinking block on the Anthropic one. The upstream
	// refuses the turn without it while it thinks, and takes it empty.
	blankReasoning bool
	// unthinkForcedTool switches thinking off when the caller forces a tool and said nothing about
	// thinking. The upstream refuses a forced tool while it thinks, which is its default.
	unthinkForcedTool bool
	// bearerOnAnthropic is an Anthropic front that reads the key as a Bearer and 401s on the
	// `x-api-key` header Anthropic's own takes.
	bearerOnAnthropic bool
	// unreasonWithTools switches reasoning off (`reasoning_effort: "none"`) when the request has
	// tools and the caller set no effort, on the OpenAI shape. The upstream refuses a function tool
	// while the model reasons, which is its default.
	unreasonWithTools bool
	// defaultSampling drops `temperature`, `top_p`, `logprobs` and `top_logprobs` unless reasoning
	// is off, on the OpenAI shape. The upstream refuses them while the model reasons.
	defaultSampling bool
}

// vendors is the third parties the gateway knows, by the provider's name as the control plane
// pushes it. Anthropic is reached on its own shape only, where none of this applies.
var vendors = map[string]upstream{
	// 400s on `max_tokens`, on `continuous_usage_stats`, and while the model reasons on a function
	// tool, a temperature or `top_p` other than 1, and `logprobs`.
	"openai": {
		outputCap: maxCompletionTokens, usagePerChunk: false,
		untypedTools: false, developerAsSystem: false, blankReasoning: false, unthinkForcedTool: false,
		bearerOnAnthropic: false, unreasonWithTools: true, defaultSampling: true,
	},
	// Ignores `continuous_usage_stats`: its usage comes on the last chunk only. Refuses a tool typed
	// `custom`, a `developer` message, a forced tool while thinking, and a past tool call sent back
	// with no reasoning.
	"deepseek": {
		outputCap: maxTokens, usagePerChunk: false,
		untypedTools: true, developerAsSystem: true, blankReasoning: true, unthinkForcedTool: true,
		bearerOnAnthropic: false, unreasonWithTools: false, defaultSampling: false,
	},
	// Reports usage on every chunk asked or not; asked all the same, so it stays that way. Its
	// Anthropic front 401s on `x-api-key`.
	"baseten": {
		outputCap: maxTokens, usagePerChunk: true,
		untypedTools: false, developerAsSystem: false, blankReasoning: false, unthinkForcedTool: false,
		bearerOnAnthropic: true, unreasonWithTools: false, defaultSampling: false,
	},
}

// ownEngine is an upstream we run: vLLM reads the cap under either name.
var ownEngine = upstream{
	outputCap: asSent, usagePerChunk: true,
	untypedTools: false, developerAsSystem: false, blankReasoning: false, unthinkForcedTool: false,
	bearerOnAnthropic: false, unreasonWithTools: false, defaultSampling: false,
}

// unknownVendor is a third party the table does not name: the cap under the name every vendor but
// OpenAI was seen to read, and no option one of them refuses.
var unknownVendor = upstream{
	outputCap: maxTokens, usagePerChunk: false,
	untypedTools: false, developerAsSystem: false, blankReasoning: false, unthinkForcedTool: false,
	bearerOnAnthropic: false, unreasonWithTools: false, defaultSampling: false,
}

// HasBearerAnthropicFront reports whether a vendor's Anthropic front reads the key as a Bearer.
func HasBearerAnthropicFront(vendor string) bool {
	return vendors[vendor].bearerOnAnthropic
}

// upstreamOf is the entry for the hop a request is on.
func upstreamOf(ctx Context) upstream {
	if !ctx.Provider {
		return ownEngine
	}
	if known, named := vendors[ctx.Vendor]; named {
		return known
	}
	return unknownVendor
}
