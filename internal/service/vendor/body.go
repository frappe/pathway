package vendor

import (
	"bytes"
	"encoding/json"

	"github.com/bytedance/sonic"
)

// What a rewrite reads and writes of a body: the shared pieces, so each vendor's own method is
// only the difference.

var (
	thinkingDisabled = json.RawMessage(`{"type":"disabled"}`)
	emptyThinking    = json.RawMessage(`{"type":"thinking","thinking":"","signature":""}`)
)

// sampling is the fields an upstream that reasons takes only at their defaults.
var sampling = []string{"temperature", "top_p", "logprobs", "top_logprobs"}

// renameOutputCap moves the output cap under `name`. A name the vendor does not read is refused by
// one and silently ignored by another, which uncaps the output. Sent under both names, the newer
// one is the caller's word.
func renameOutputCap(req *Request, name string) (bool, error) {
	other := MaxTokens
	if name == MaxTokens {
		other = MaxCompletionTokens
	}
	body := req.Body
	value, stray := body[other]
	if !stray {
		return false, nil
	}
	if _, both := body[name]; !both || other == MaxCompletionTokens {
		body[name] = value
	}
	delete(body, other)
	return true, nil
}

// isTakenWhileReasoning is a temperature or top_p of 1: the default, written out.
func isTakenWhileReasoning(field string, raw json.RawMessage) bool {
	var value float64
	return (field == "temperature" || field == "top_p") && sonic.Unmarshal(raw, &value) == nil && value == 1
}

// said reports whether the caller sent one of `fields`.
func said(body map[string]json.RawMessage, fields ...string) bool {
	for _, field := range fields {
		if _, spoke := body[field]; spoke {
			return true
		}
	}
	return false
}

func isThinkingOff(body map[string]json.RawMessage) bool {
	return typeOf(body["thinking"]) == "disabled"
}

// typeOf is the `type` of a JSON object, blank for anything else.
func typeOf(raw json.RawMessage) string {
	var typed struct {
		Type string `json:"type"`
	}
	_ = sonic.Unmarshal(raw, &typed)
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
func mentions(body map[string]json.RawMessage, token string) bool {
	return bytes.Contains(body["messages"], []byte(token))
}

// eachObject runs `edit` over the objects of the list at `field`, and writes the list back if one
// changed. Only the objects' own keys are re-encoded: what is inside them stays as sent.
func eachObject(body map[string]json.RawMessage, field string, edit func(map[string]json.RawMessage) bool) (bool, error) {
	var objects []map[string]json.RawMessage
	if err := sonic.Unmarshal(body[field], &objects); err != nil {
		return false, nil
	}
	changed := false
	for _, object := range objects {
		changed = edit(object) || changed
	}
	if !changed {
		return false, nil
	}
	encoded, err := sonic.Marshal(objects)
	if err != nil {
		return false, err
	}
	body[field] = encoded
	return true, nil
}
