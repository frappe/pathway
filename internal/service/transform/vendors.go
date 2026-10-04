package transform

// The two names of the output cap on the OpenAI chat shape. An upstream reads one of them.
const (
	maxTokens           = "max_tokens"
	maxCompletionTokens = "max_completion_tokens"
	// asSent leaves the cap under whichever name the client wrote.
	asSent = ""
)

// upstream is how one upstream is spoken to on the OpenAI shape. Each value was measured through a
// gateway: the README's "What each upstream was seen to do" says when.
type upstream struct {
	// outputCap is the field the upstream reads the output cap from.
	outputCap string
	// usagePerChunk asks a stream for usage on every chunk (`continuous_usage_stats`), so one that
	// is cut is still billed for what it had generated.
	usagePerChunk bool
}

// vendors is the third parties the gateway knows, by the provider's name as the control plane
// pushes it. Anthropic is reached on its own shape only, where none of this applies.
var vendors = map[string]upstream{
	// 400s on `max_tokens`, and on `continuous_usage_stats`.
	"openai": {outputCap: maxCompletionTokens, usagePerChunk: false},
	// Ignores `continuous_usage_stats`: its usage comes on the last chunk only.
	"deepseek": {outputCap: maxTokens, usagePerChunk: false},
	// Reports usage on every chunk asked or not; asked all the same, so it stays that way.
	"baseten": {outputCap: maxTokens, usagePerChunk: true},
}

// ownEngine is an upstream we run: vLLM reads the cap under either name.
var ownEngine = upstream{outputCap: asSent, usagePerChunk: true}

// unknownVendor is a third party the table does not name: the cap under the name every vendor but
// OpenAI was seen to read, and no option one of them refuses.
var unknownVendor = upstream{outputCap: maxTokens, usagePerChunk: false}

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
