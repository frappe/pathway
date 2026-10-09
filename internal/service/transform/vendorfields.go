package transform

import (
	"bytes"
	"encoding/json"
)

func init() {
	Register(vendorFields{})
}

const messages = "/v1/messages"

var (
	thinkingDisabled = json.RawMessage(`{"type":"disabled"}`)
	emptyThinking    = json.RawMessage(`{"type":"thinking","thinking":"","signature":""}`)
)

// vendorFields sends a vendor a field the way it reads it, from the `vendors` table. Engines we run
// get the body as sent. It runs per attempt on the client's own body, so a request that moves to
// another vendor is written afresh for that one.
type vendorFields struct{}

func (vendorFields) Name() string        { return "vendorfields" }
func (vendorFields) Endpoints() []string { return []string{"/v1/chat/completions", messages} }

// rewrite is one difference of an upstream, written into the body. It reports whether it changed
// anything, and leaves a body it cannot read to the upstream to refuse.
type rewrite func(Context, Body) (bool, error)

// Thinking is switched off before a past turn is given blank reasoning: an upstream that is not
// thinking asks for none.
var (
	// Sampling is judged after the tools rule: a request it switched reasoning off for keeps its own.
	chatRewrites = []rewrite{
		nameOutputCap, developerAsSystem, unthinkForcedFunction, blankReasoningContent, unreasonWithTools,
		defaultSampling,
	}
	messagesRewrites = []rewrite{dropSpeed, untypeCustomTools, unthinkForcedTool, blankThinkingBlock}
)

// dropSpeed takes fast mode off a request to a vendor. `speed` is Anthropic's faster tier at twice
// the rate, and nothing here prices it: the request goes at the standard speed and the caller is
// told. Ours, not a vendor's quirk, so every vendor's Anthropic front gets it and the table has no
// flag. An engine we run gets the body as sent. OpenAI's tier is `service_tier`, which the
// `servicetier` transform drops on every hop.
func dropSpeed(ctx Context, body Body) (bool, error) {
	if !ctx.Provider || !said(body, "speed") {
		return false, nil
	}
	delete(body, "speed")
	ctx.tell("speed=default")
	return true, nil
}

func (vendorFields) Apply(ctx Context, body Body) (bool, error) {
	rewrites := chatRewrites
	if ctx.Path == messages {
		rewrites = messagesRewrites
	}
	changed := false
	for _, rewrite := range rewrites {
		did, err := rewrite(ctx, body)
		if err != nil {
			return changed, err
		}
		changed = changed || did
	}
	return changed, nil
}

// nameOutputCap moves the output cap under the one name the vendor reads. A name the vendor does
// not read is refused by one and silently ignored by another, which uncaps the output.
func nameOutputCap(ctx Context, body Body) (bool, error) {
	name := upstreamOf(ctx).outputCap
	if name == asSent {
		return false, nil
	}
	other := maxTokens
	if name == maxTokens {
		other = maxCompletionTokens
	}
	value, stray := body[other]
	if !stray {
		return false, nil
	}
	// Sent under both names, the newer one is the caller's word.
	if _, both := body[name]; !both || other == maxCompletionTokens {
		body[name] = value
	}
	delete(body, other)
	return true, nil
}

// untypeCustomTools drops `"type":"custom"` from each tool. Any other type stays: one the vendor
// would run on its own side was refused at `route`, from the route's denied_tools, before this ran.
func untypeCustomTools(ctx Context, body Body) (bool, error) {
	if !upstreamOf(ctx).untypedTools {
		return false, nil
	}
	return eachObject(body, "tools", func(tool map[string]json.RawMessage) bool {
		if string(tool["type"]) != `"custom"` {
			return false
		}
		delete(tool, "type")
		return true
	})
}

// developerAsSystem renames the role of a `developer` message.
func developerAsSystem(ctx Context, body Body) (bool, error) {
	if !upstreamOf(ctx).developerAsSystem || !mentions(body, `"developer"`) {
		return false, nil
	}
	return eachObject(body, "messages", func(message map[string]json.RawMessage) bool {
		if string(message["role"]) != `"developer"` {
			return false
		}
		message["role"] = json.RawMessage(`"system"`)
		return true
	})
}

// blankReasoningContent gives a past tool-call turn an empty `reasoning_content` when the caller
// sent none back, which is what most clients do.
func blankReasoningContent(ctx Context, body Body) (bool, error) {
	if !upstreamOf(ctx).blankReasoning || isThinkingOff(body) || !mentions(body, `"tool_calls"`) {
		return false, nil
	}
	return eachObject(body, "messages", func(message map[string]json.RawMessage) bool {
		var calls []json.RawMessage
		_, reasoned := message["reasoning_content"]
		if reasoned || json.Unmarshal(message["tool_calls"], &calls) != nil || len(calls) == 0 {
			return false
		}
		message["reasoning_content"] = json.RawMessage(`""`)
		return true
	})
}

// blankThinkingBlock is blankReasoningContent on the Anthropic shape: a past `tool_use` turn sent
// back with no thinking block gets an empty one in front.
func blankThinkingBlock(ctx Context, body Body) (bool, error) {
	if !upstreamOf(ctx).blankReasoning || isThinkingOff(body) || !mentions(body, `"tool_use"`) {
		return false, nil
	}
	return eachObject(body, "messages", func(message map[string]json.RawMessage) bool {
		var blocks []json.RawMessage
		if json.Unmarshal(message["content"], &blocks) != nil || !hasBlock(blocks, "tool_use") || hasBlock(blocks, "thinking") {
			return false
		}
		content, err := json.Marshal(append([]json.RawMessage{emptyThinking}, blocks...))
		if err != nil {
			return false
		}
		message["content"] = content
		return true
	})
}

// unthinkForcedFunction switches thinking off when the caller forces a tool. A caller who also
// asked for reasoning gets the upstream's refusal: the gateway does not choose between the two.
func unthinkForcedFunction(ctx Context, body Body) (bool, error) {
	forced := string(body["tool_choice"]) == `"required"` || typeOf(body["tool_choice"]) == "function"
	if !upstreamOf(ctx).unthinkForcedTool || !forced || said(body, "thinking", "reasoning_effort") {
		return false, nil
	}
	body["thinking"] = thinkingDisabled
	ctx.tell("thinking=disabled")
	return true, nil
}

// unthinkForcedTool is unthinkForcedFunction on the Anthropic shape, where only a named tool is
// refused: `any` is taken.
func unthinkForcedTool(ctx Context, body Body) (bool, error) {
	if !upstreamOf(ctx).unthinkForcedTool || typeOf(body["tool_choice"]) != "tool" || said(body, "thinking") {
		return false, nil
	}
	body["thinking"] = thinkingDisabled
	ctx.tell("thinking=disabled")
	return true, nil
}

// unreasonWithTools switches reasoning off when the request carries tools and the caller set no
// effort. A caller who set one gets the upstream's refusal.
// ponytail: every model of a flagged vendor must take `"none"`; a per-model switch when one does not.
func unreasonWithTools(ctx Context, body Body) (bool, error) {
	var tools []json.RawMessage
	if !upstreamOf(ctx).unreasonWithTools || said(body, "reasoning_effort") ||
		json.Unmarshal(body["tools"], &tools) != nil || len(tools) == 0 {
		return false, nil
	}
	body["reasoning_effort"] = json.RawMessage(`"none"`)
	ctx.tell("reasoning_effort=none")
	return true, nil
}

// sampling is the fields an upstream that reasons takes only at their defaults.
var sampling = []string{"temperature", "top_p", "logprobs", "top_logprobs"}

// defaultSampling drops the sampling fields while the model reasons, which it does unless the
// effort is `"none"`. This one overrides what the caller asked for: the upstream refuses them while
// it reasons, and the reasoning is what the model is for.
// ponytail: every model of a flagged vendor must reason by default; a per-model switch before one
// that does not is routed, or its temperature is taken from it for nothing.
func defaultSampling(ctx Context, body Body) (bool, error) {
	if !upstreamOf(ctx).defaultSampling || string(body["reasoning_effort"]) == `"none"` {
		return false, nil
	}
	changed := false
	for _, field := range sampling {
		if raw, sent := body[field]; sent && !isTakenWhileReasoning(field, raw) {
			delete(body, field)
			ctx.tell(field + "=default")
			changed = true
		}
	}
	return changed, nil
}

// isTakenWhileReasoning is a temperature or top_p of 1: the default, written out.
func isTakenWhileReasoning(field string, raw json.RawMessage) bool {
	var value float64
	return (field == "temperature" || field == "top_p") && json.Unmarshal(raw, &value) == nil && value == 1
}

// said reports whether the caller sent one of `fields`.
func said(body Body, fields ...string) bool {
	for _, field := range fields {
		if _, spoke := body[field]; spoke {
			return true
		}
	}
	return false
}

func isThinkingOff(body Body) bool {
	return typeOf(body["thinking"]) == "disabled"
}

// typeOf is the `type` of a JSON object, blank for anything else.
func typeOf(raw json.RawMessage) string {
	var typed struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &typed)
	return typed.Type
}

func hasBlock(blocks []json.RawMessage, kind string) bool {
	for _, block := range blocks {
		if typeOf(block) == kind {
			return true
		}
	}
	return false
}

// mentions is the cheap test before `messages` is decoded: most bodies hold nothing to rewrite.
func mentions(body Body, token string) bool {
	return bytes.Contains(body["messages"], []byte(token))
}

// eachObject runs `edit` over the objects of the list at `field`, and writes the list back if one
// changed. Only the objects' own keys are re-encoded: what is inside them stays as sent.
func eachObject(body Body, field string, edit func(map[string]json.RawMessage) bool) (bool, error) {
	var objects []map[string]json.RawMessage
	if err := json.Unmarshal(body[field], &objects); err != nil {
		return false, nil
	}
	changed := false
	for _, object := range objects {
		changed = edit(object) || changed
	}
	if !changed {
		return false, nil
	}
	encoded, err := json.Marshal(objects)
	if err != nil {
		return false, err
	}
	body[field] = encoded
	return true, nil
}
