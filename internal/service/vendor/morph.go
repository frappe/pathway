package vendor

import (
	"encoding/json"

	"github.com/bytedance/sonic"
)

// morph speaks both shapes as base does: `x-api-key` (or a Bearer) on /v1/messages, its id under
// X-Request-Id, no server tool run (probed 2026-11-05). No CacheKey on purpose: one key shared
// across tenants funnels them all onto one worker, and 429s. It refuses a system turn in
// `messages` whose blocks are not all text (2026-11-09).
type morph struct{ base }

func (morph) Quirks() []string { return []string{"name_output_cap", "fold_tool_changes"} }

// InputCountsCache: Morph's /v1/messages counts a cache read inside `input_tokens`, as its chat
// shape does inside `prompt_tokens`. One request sent twice answered `input_tokens` 8057 both
// times, `cache_read_input_tokens` 0 and then 4480 (2026-11-17); Anthropic would have answered 3577
// the second time. It reports no cache write.
func (morph) InputCountsCache() bool { return true }

// DroppedFields: the tags of Morph's run scheduler, under both names it reads each by. Requests
// that share a run id are scheduled as one run and a `run_final` call releases it, and every team
// here dials Morph with one key, so one team's id could be another's. Its reference reads them
// for Kimi K3.1 alone: both models dialled here answered a `run_final` call as any other
// (2026-11-17).
func (morph) DroppedFields() []string {
	return []string{"run_id", "parent_run_id", "run_final", "program_id", "parent_program_id", "program_final"}
}

// FoldToolChanges: Morph answers 400 `system blocks must be of type 'text'` to the turn Claude
// Code 2.1 loads a deferred tool with, a `role: system` message of `tool_addition` blocks
// (`{"tool": {"type": "tool_definition", "definition": {...}}}`) beside its text. Each definition
// joins `tools`, a `tool_removal` takes the tool it names out, and the turn keeps its text where
// it was, so the cached prefix behind it stands.
func (morph) FoldToolChanges(req *Request) (bool, error) {
	if !mentions(req.Body, `"tool_addition"`) && !mentions(req.Body, `"tool_removal"`) {
		return false, nil
	}
	var tools []json.RawMessage
	_ = sonic.Unmarshal(req.Body["tools"], &tools)
	changed, err := eachObject(req.Body, "messages", func(message map[string]json.RawMessage) bool {
		var blocks []map[string]json.RawMessage
		if string(message["role"]) != `"system"` || sonic.Unmarshal(message["content"], &blocks) != nil {
			return false
		}
		kept := make([]map[string]json.RawMessage, 0, len(blocks))
		for _, block := range blocks {
			change, isChange := readToolChange(block)
			if !isChange {
				kept = append(kept, block)
				continue
			}
			if change.Definition != nil {
				tools = append(tools, change.Definition)
			}
			if change.Name != "" {
				tools = withoutTool(tools, change.Name)
			}
		}
		if len(kept) == len(blocks) {
			return false
		}
		message["content"], _ = sonic.Marshal(kept)
		return true
	})
	if err != nil || !changed {
		return changed, err
	}
	if len(tools) == 0 {
		delete(req.Body, "tools")
		return true, nil
	}
	req.Body["tools"], err = sonic.Marshal(tools)
	return true, err
}

// toolChange is what a `tool_addition` or `tool_removal` block carries under `tool`: the
// definition the one adds, the name the other removes.
type toolChange struct {
	Definition json.RawMessage `json:"definition"`
	Name       string          `json:"name"`
}

// readToolChange reads a tool change block; false on any other block.
func readToolChange(block map[string]json.RawMessage) (toolChange, bool) {
	kind := string(block["type"])
	if kind != `"tool_addition"` && kind != `"tool_removal"` {
		return toolChange{}, false
	}
	var change toolChange
	_ = sonic.Unmarshal(block["tool"], &change)
	return change, true
}

// withoutTool is `tools` less every tool named `name`.
func withoutTool(tools []json.RawMessage, name string) []json.RawMessage {
	kept := make([]json.RawMessage, 0, len(tools))
	for _, tool := range tools {
		var named struct {
			Name string `json:"name"`
		}
		_ = sonic.Unmarshal(tool, &named)
		if named.Name != name {
			kept = append(kept, tool)
		}
	}
	return kept
}
