package vendor

import (
	"encoding/json"

	"github.com/bytedance/sonic"
)

// openai 400s on `max_tokens`, on `continuous_usage_stats`, and while the model reasons, which it
// does by default, on a function tool, a temperature or `top_p` other than 1, and `logprobs`
// (2026-10-05, 2026-10-06).
type openai struct{ base }

func (openai) Quirks() []string {
	return []string{"name_output_cap", "unreason_with_tools", "default_sampling"}
}

// NameOutputCap: OpenAI reads only the newer name.
func (openai) NameOutputCap(req *Request) (bool, error) {
	return renameOutputCap(req, MaxCompletionTokens)
}

// UnreasonWithTools switches reasoning off when the request carries tools and the caller set no
// effort. A caller who set one gets the upstream's refusal: the gateway does not choose between
// the two. A model that does not reason has the row switch this off (skip_quirks).
func (openai) UnreasonWithTools(req *Request) (bool, error) {
	var tools []json.RawMessage
	body := req.Body
	if said(body, "reasoning_effort") || sonic.Unmarshal(body["tools"], &tools) != nil || len(tools) == 0 {
		return false, nil
	}
	body["reasoning_effort"] = json.RawMessage(`"none"`)
	req.Tell("reasoning_effort=none")
	return true, nil
}

// DefaultSampling drops the sampling fields while the model reasons, which it does unless the
// effort is `"none"`. This one overrides what the caller asked for: the upstream refuses them while
// it reasons, and the reasoning is what the model is for. A model that does not reason by default
// has the row switch this off (skip_quirks), or its temperature is taken from it for nothing.
func (openai) DefaultSampling(req *Request) (bool, error) {
	body := req.Body
	if string(body["reasoning_effort"]) == `"none"` {
		return false, nil
	}
	changed := false
	for _, field := range sampling {
		if raw, sent := body[field]; sent && !isTakenWhileReasoning(field, raw) {
			delete(body, field)
			req.Tell(field + "=default")
			changed = true
		}
	}
	return changed, nil
}
