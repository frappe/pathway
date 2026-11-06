package vendor

import (
	"encoding/json"

	"github.com/bytedance/sonic"
)

// deepseek ignores `continuous_usage_stats`: its usage comes on the last chunk only. It refuses a
// tool typed `custom`, a `developer` message, a forced tool while thinking, and a past tool call
// sent back with no reasoning (2026-10-05). It thinks by default.
type deepseek struct{ base }

func (deepseek) Quirks() []string {
	return []string{"name_output_cap", "untype_custom_tools", "developer_as_system", "blank_reasoning_content",
		"blank_thinking_block", "unthink_forced_function", "unthink_forced_tool"}
}

// CacheKey: DeepSeek isolates its KV cache by `user_id`.
func (deepseek) CacheKey(req *Request) (bool, error) {
	return namespaceKey(req.Team, req.Body, "user_id")
}

// MetadataCacheKey: the same `user_id`, inside `metadata` on the Anthropic shape.
func (deepseek) MetadataCacheKey(req *Request) (bool, error) {
	return namespaceMetadataKey(req.Team, req.Body)
}

// UntypeCustomTools drops `"type":"custom"` from each tool. Any other type stays: one the vendor
// would run on its own side was refused at `route`, from the route's denied_tools, before this ran.
func (deepseek) UntypeCustomTools(req *Request) (bool, error) {
	return eachObject(req.Body, "tools", func(tool map[string]json.RawMessage) bool {
		if string(tool["type"]) != `"custom"` {
			return false
		}
		delete(tool, "type")
		return true
	})
}

// DeveloperAsSystem renames the role of a `developer` message.
func (deepseek) DeveloperAsSystem(req *Request) (bool, error) {
	if !mentions(req.Body, `"developer"`) {
		return false, nil
	}
	return eachObject(req.Body, "messages", func(message map[string]json.RawMessage) bool {
		if string(message["role"]) != `"developer"` {
			return false
		}
		message["role"] = json.RawMessage(`"system"`)
		return true
	})
}

// BlankReasoningContent gives a past tool-call turn an empty `reasoning_content` when the caller
// sent none back, which is what most clients do. Nothing when the caller disabled thinking: the
// upstream asks for no reasoning then.
func (deepseek) BlankReasoningContent(req *Request) (bool, error) {
	if isThinkingOff(req.Body) || !mentions(req.Body, `"tool_calls"`) {
		return false, nil
	}
	return eachObject(req.Body, "messages", func(message map[string]json.RawMessage) bool {
		var calls []json.RawMessage
		_, reasoned := message["reasoning_content"]
		if reasoned || sonic.Unmarshal(message["tool_calls"], &calls) != nil || len(calls) == 0 {
			return false
		}
		message["reasoning_content"] = json.RawMessage(`""`)
		return true
	})
}

// BlankThinkingBlock is BlankReasoningContent on the Anthropic shape: a past `tool_use` turn sent
// back with no thinking block gets an empty one in front.
func (deepseek) BlankThinkingBlock(req *Request) (bool, error) {
	if isThinkingOff(req.Body) || !mentions(req.Body, `"tool_use"`) {
		return false, nil
	}
	return eachObject(req.Body, "messages", func(message map[string]json.RawMessage) bool {
		var blocks []json.RawMessage
		if sonic.Unmarshal(message["content"], &blocks) != nil || !hasBlock(blocks, "tool_use") || hasBlock(blocks, "thinking") {
			return false
		}
		content, err := sonic.Marshal(append([]json.RawMessage{emptyThinking}, blocks...))
		if err != nil {
			return false
		}
		message["content"] = content
		return true
	})
}

// UnthinkForcedFunction switches thinking off when the caller forces a tool. A caller who also
// asked for reasoning gets the upstream's refusal: the gateway does not choose between the two.
func (deepseek) UnthinkForcedFunction(req *Request) (bool, error) {
	body := req.Body
	forced := string(body["tool_choice"]) == `"required"` || typeOf(body["tool_choice"]) == "function"
	if !forced || said(body, "thinking", "reasoning_effort") {
		return false, nil
	}
	body["thinking"] = thinkingDisabled
	req.Tell("thinking=disabled")
	return true, nil
}

// UnthinkForcedTool is UnthinkForcedFunction on the Anthropic shape, where only a named tool is
// refused: `any` is taken.
func (deepseek) UnthinkForcedTool(req *Request) (bool, error) {
	body := req.Body
	if typeOf(body["tool_choice"]) != "tool" || said(body, "thinking") {
		return false, nil
	}
	body["thinking"] = thinkingDisabled
	req.Tell("thinking=disabled")
	return true, nil
}
