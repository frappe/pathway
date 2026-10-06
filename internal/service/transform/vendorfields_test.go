package transform

import (
	"slices"
	"testing"
)

// A vendor gets the cap under the one name it honours, and an engine we run gets the body as sent.
func TestTheOutputCapIsNamedForTheVendor(t *testing.T) {
	for _, tc := range []struct {
		vendor, raw, old, renamed string
		changed                   bool
	}{
		{"openai", `{"max_tokens":16}`, "", "16", true},
		{"openai", `{"max_completion_tokens":8}`, "", "8", false},
		{"openai", `{"max_tokens":16,"max_completion_tokens":8}`, "", "8", true},
		{"deepseek", `{"max_tokens":16}`, "16", "", false},
		{"deepseek", `{"max_completion_tokens":8}`, "8", "", true},
		{"deepseek", `{"max_tokens":16,"max_completion_tokens":8}`, "8", "", true},
		{"baseten", `{"max_tokens":16}`, "16", "", false},
		{"baseten", `{"max_completion_tokens":8}`, "8", "", true},
		{"a vendor the table does not name", `{"max_completion_tokens":8}`, "8", "", true},
		{"", `{"max_tokens":16}`, "16", "", false},
		{"", `{"max_completion_tokens":8}`, "", "8", false},
	} {
		body := decode(t, tc.raw)
		changed, err := (vendorFields{}).Apply(Context{Path: "/v1/chat/completions", Provider: tc.vendor != "", Vendor: tc.vendor}, body)
		if err != nil {
			t.Fatal(err)
		}
		if changed != tc.changed {
			t.Errorf("%q %s: changed = %v, want %v", tc.vendor, tc.raw, changed, tc.changed)
		}
		if got := string(body["max_tokens"]); got != tc.old {
			t.Errorf("%q %s: max_tokens = %q, want %q", tc.vendor, tc.raw, got, tc.old)
		}
		if got := string(body["max_completion_tokens"]); got != tc.renamed {
			t.Errorf("%q %s: max_completion_tokens = %q, want %q", tc.vendor, tc.raw, got, tc.renamed)
		}
	}
}

// A vendor that refuses `"type":"custom"` gets the caller's own tools without it, and nothing else
// about a tool moves. Every other upstream gets the tools as sent.
func TestACustomToolLosesItsTypeForAVendorThatRefusesIt(t *testing.T) {
	const typed = `[{"type":"custom","name":"a","input_schema":{"type":"object"}}]`
	const untyped = `[{"input_schema":{"type":"object"},"name":"a"}]`
	const search = `{"max_uses":1,"name":"web_search","type":"web_search_20250305"}`
	for _, tc := range []struct {
		vendor, tools, want string
		changed             bool
	}{
		{"deepseek", typed, untyped, true},
		{"deepseek", untyped, untyped, false},
		{"deepseek", `[` + search + `,{"type":"custom","name":"a"}]`, `[` + search + `,{"name":"a"}]`, true},
		{"deepseek", `"not a list"`, `"not a list"`, false},
		{"anthropic", typed, typed, false},
		{"baseten", typed, typed, false},
		{"", typed, typed, false},
	} {
		body := decode(t, `{"max_tokens":16,"tools":`+tc.tools+`}`)
		changed, err := (vendorFields{}).Apply(Context{Path: "/v1/messages", Provider: tc.vendor != "", Vendor: tc.vendor}, body)
		if err != nil {
			t.Fatal(err)
		}
		if changed != tc.changed {
			t.Errorf("%q %s: changed = %v, want %v", tc.vendor, tc.tools, changed, tc.changed)
		}
		if got := string(body["tools"]); got != tc.want {
			t.Errorf("%q: tools = %s, want %s", tc.vendor, got, tc.want)
		}
	}

	body := decode(t, `{"max_tokens":16}`)
	if changed, err := (vendorFields{}).Apply(Context{Path: "/v1/messages", Provider: true, Vendor: "deepseek"}, body); changed || err != nil || len(body) != 1 {
		t.Errorf("no tools: changed=%v err=%v body = %v", changed, err, body)
	}
}

// apply runs vendorfields for one vendor and returns the body's field after it.
func apply(t *testing.T, path, vendor, raw, field string) (bool, string) {
	t.Helper()
	body := decode(t, raw)
	changed, err := (vendorFields{}).Apply(Context{Path: path, Provider: vendor != "", Vendor: vendor}, body)
	if err != nil {
		t.Fatal(err)
	}
	return changed, string(body[field])
}

// OpenAI refuses a function tool while the model reasons: reasoning goes off, and the caller is
// told. Not when the caller set an effort, and not for another upstream.
func TestToolsSwitchOpenAIReasoningOff(t *testing.T) {
	const tools = `"tools":[{"type":"function","function":{"name":"a"}}]`
	for _, tc := range []struct {
		name, vendor, raw, effort string
		told                      []string
	}{
		{"tools, no effort", "openai", `{` + tools + `}`, `"none"`, []string{"reasoning_effort=none"}},
		{"tools, the caller set an effort", "openai", `{"reasoning_effort":"high",` + tools + `}`, `"high"`, nil},
		{"no tools", "openai", `{}`, "", nil},
		{"an empty tools list", "openai", `{"tools":[]}`, "", nil},
		{"deepseek", "deepseek", `{` + tools + `}`, "", nil},
		{"baseten", "baseten", `{` + tools + `}`, "", nil},
		{"an engine of ours", "", `{` + tools + `}`, "", nil},
	} {
		var told []string
		body := decode(t, tc.raw)
		if _, err := (vendorFields{}).Apply(Context{Path: "/v1/chat/completions", Provider: tc.vendor != "", Vendor: tc.vendor, Changed: &told}, body); err != nil {
			t.Fatal(err)
		}
		if got := string(body["reasoning_effort"]); got != tc.effort || !slices.Equal(told, tc.told) {
			t.Errorf("%s: reasoning_effort = %q told %v, want %q told %v", tc.name, got, told, tc.effort, tc.told)
		}
	}
}

// While it reasons OpenAI takes no sampling of the caller's: the fields are dropped and the caller
// is told. With reasoning off, the caller's own or the tools rule's, they go as sent.
func TestSamplingIsDroppedWhileOpenAIReasons(t *testing.T) {
	const tools = `"tools":[{"type":"function","function":{"name":"a"}}]`
	for _, tc := range []struct {
		name, vendor, raw string
		kept              []string
		told              []string
	}{
		{"temperature 0.2", "openai", `{"temperature":0.2}`, nil, []string{"temperature=default"}},
		{"temperature 0", "openai", `{"temperature":0}`, nil, []string{"temperature=default"}},
		{"temperature 1", "openai", `{"temperature":1}`, []string{"temperature"}, nil},
		{"top_p 0.5", "openai", `{"top_p":0.5}`, nil, []string{"top_p=default"}},
		{"top_p 1.0", "openai", `{"top_p":1.0}`, []string{"top_p"}, nil},
		{"logprobs", "openai", `{"logprobs":true,"top_logprobs":1}`, nil, []string{"logprobs=default", "top_logprobs=default"}},
		{"all four, and an effort", "openai", `{"reasoning_effort":"high","temperature":0.2,"top_p":0.5,"logprobs":true,"top_logprobs":2}`,
			nil, []string{"temperature=default", "top_p=default", "logprobs=default", "top_logprobs=default"}},
		{"reasoning off by the caller", "openai", `{"reasoning_effort":"none","temperature":0.2,"top_p":0.5,"logprobs":true}`,
			[]string{"temperature", "top_p", "logprobs"}, nil},
		{"reasoning off by the tools rule", "openai", `{"temperature":0.2,` + tools + `}`, []string{"temperature"}, []string{"reasoning_effort=none"}},
		{"deepseek", "deepseek", `{"temperature":0.2,"top_p":0.5,"logprobs":true}`, []string{"temperature", "top_p", "logprobs"}, nil},
		{"an engine of ours", "", `{"temperature":0.2,"top_p":0.5,"logprobs":true}`, []string{"temperature", "top_p", "logprobs"}, nil},
	} {
		var told []string
		body := decode(t, tc.raw)
		if _, err := (vendorFields{}).Apply(Context{Path: "/v1/chat/completions", Provider: tc.vendor != "", Vendor: tc.vendor, Changed: &told}, body); err != nil {
			t.Fatal(err)
		}
		var kept []string
		for _, field := range sampling {
			if _, present := body[field]; present {
				kept = append(kept, field)
			}
		}
		if !slices.Equal(kept, tc.kept) || !slices.Equal(told, tc.told) {
			t.Errorf("%s: kept %v told %v, want kept %v told %v", tc.name, kept, told, tc.kept, tc.told)
		}
	}
}

// The other change the caller is told of: thinking switched off for a forced tool.
func TestTheCallerIsToldThinkingWentOff(t *testing.T) {
	var told []string
	body := decode(t, `{"tool_choice":"required"}`)
	if _, err := (vendorFields{}).Apply(Context{Path: "/v1/chat/completions", Provider: true, Vendor: "deepseek", Changed: &told}, body); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(told, []string{"thinking=disabled"}) {
		t.Errorf("told %v", told)
	}
	told = nil
	body = decode(t, `{"messages":[{"role":"developer","content":"x"}]}`)
	if _, err := (vendorFields{}).Apply(Context{Path: "/v1/chat/completions", Provider: true, Vendor: "deepseek", Changed: &told}, body); err != nil || len(told) != 0 {
		t.Errorf("a rename was told of: %v (err %v)", told, err)
	}
}

// DeepSeek knows no `developer` role: the message goes as `system`. Others get it as sent.
func TestADeveloperMessageIsSystemForAVendorThatRefusesIt(t *testing.T) {
	const sent = `{"messages":[{"role":"developer","content":"be brief"},{"role":"user","content":"the developer said hi"}]}`
	const renamed = `[{"content":"be brief","role":"system"},{"content":"the developer said hi","role":"user"}]`
	if changed, got := apply(t, "/v1/chat/completions", "deepseek", sent, "messages"); !changed || got != renamed {
		t.Errorf("deepseek: changed=%v messages = %s", changed, got)
	}
	for _, vendor := range []string{"openai", "baseten", ""} {
		if changed, _ := apply(t, "/v1/chat/completions", vendor, sent, "messages"); changed {
			t.Errorf("%q: a developer message was rewritten", vendor)
		}
	}
	const none = `{"messages":[{"role":"user","content":"I am a \"developer\""}]}`
	if changed, _ := apply(t, "/v1/chat/completions", "deepseek", none, "messages"); changed {
		t.Error("a body with no developer message was rewritten")
	}
}

// A past tool-call turn the caller sent back without its reasoning gets an empty one, on either
// shape, unless thinking is off. A turn that has its reasoning, and a plain turn, are left alone.
func TestAPastToolCallGetsBlankReasoning(t *testing.T) {
	const call = `"tool_calls":[{"id":"c1","type":"function","function":{"name":"a","arguments":"{}"}}]`
	const use = `{"type":"tool_use","id":"t1","name":"a","input":{}}`
	const blank = `{"type":"thinking","thinking":"","signature":""}`
	for _, tc := range []struct {
		name, path, vendor, raw, want string
		changed                       bool
	}{
		{"chat, no reasoning", "/v1/chat/completions", "deepseek", `{"messages":[{"role":"assistant",` + call + `}]}`,
			`[{"reasoning_content":"","role":"assistant",` + call + `}]`, true},
		{"chat, reasoning kept", "/v1/chat/completions", "deepseek", `{"messages":[{"role":"assistant","reasoning_content":"r",` + call + `}]}`,
			`[{"role":"assistant","reasoning_content":"r",` + call + `}]`, false},
		{"chat, a plain turn", "/v1/chat/completions", "deepseek", `{"messages":[{"role":"assistant","content":"no tool_calls here"}]}`,
			`[{"role":"assistant","content":"no tool_calls here"}]`, false},
		{"chat, thinking off", "/v1/chat/completions", "deepseek", `{"thinking":{"type":"disabled"},"messages":[{"role":"assistant",` + call + `}]}`,
			`[{"role":"assistant",` + call + `}]`, false},
		{"chat, baseten", "/v1/chat/completions", "baseten", `{"messages":[{"role":"assistant",` + call + `}]}`,
			`[{"role":"assistant",` + call + `}]`, false},
		{"messages, no thinking block", "/v1/messages", "deepseek", `{"messages":[{"role":"assistant","content":[` + use + `]}]}`,
			`[{"content":[` + blank + `,` + use + `],"role":"assistant"}]`, true},
		{"messages, thinking block kept", "/v1/messages", "deepseek", `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"r"},` + use + `]}]}`,
			`[{"role":"assistant","content":[{"type":"thinking","thinking":"r"},` + use + `]}]`, false},
		{"messages, a tool result", "/v1/messages", "deepseek", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"}]}]}`,
			`[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"}]}]`, false},
		{"messages, thinking off", "/v1/messages", "deepseek", `{"thinking":{"type":"disabled"},"messages":[{"role":"assistant","content":[` + use + `]}]}`,
			`[{"role":"assistant","content":[` + use + `]}]`, false},
	} {
		if changed, got := apply(t, tc.path, tc.vendor, tc.raw, "messages"); changed != tc.changed || got != tc.want {
			t.Errorf("%s: changed=%v messages = %s, want %s", tc.name, changed, got, tc.want)
		}
	}
}

// A forced tool switches DeepSeek's thinking off, unless the caller spoke about reasoning: then the
// body goes as sent and the vendor refuses it.
func TestAForcedToolSwitchesThinkingOff(t *testing.T) {
	const off = `{"type":"disabled"}`
	for _, tc := range []struct {
		name, path, vendor, raw, thinking string
	}{
		{"chat, required", "/v1/chat/completions", "deepseek", `{"tool_choice":"required"}`, off},
		{"chat, a named function", "/v1/chat/completions", "deepseek", `{"tool_choice":{"type":"function","function":{"name":"a"}}}`, off},
		{"chat, auto", "/v1/chat/completions", "deepseek", `{"tool_choice":"auto"}`, ""},
		{"chat, no tool_choice", "/v1/chat/completions", "deepseek", `{}`, ""},
		{"chat, the caller asked to think", "/v1/chat/completions", "deepseek", `{"tool_choice":"required","thinking":{"type":"enabled"}}`, `{"type":"enabled"}`},
		{"chat, the caller set an effort", "/v1/chat/completions", "deepseek", `{"tool_choice":"required","reasoning_effort":"high"}`, ""},
		{"chat, baseten", "/v1/chat/completions", "baseten", `{"tool_choice":"required"}`, ""},
		{"messages, a named tool", "/v1/messages", "deepseek", `{"tool_choice":{"type":"tool","name":"a"}}`, off},
		{"messages, any", "/v1/messages", "deepseek", `{"tool_choice":{"type":"any"}}`, ""},
		{"messages, the caller asked to think", "/v1/messages", "deepseek", `{"tool_choice":{"type":"tool","name":"a"},"thinking":{"type":"enabled","budget_tokens":1024}}`, `{"type":"enabled","budget_tokens":1024}`},
	} {
		if _, got := apply(t, tc.path, tc.vendor, tc.raw, "thinking"); got != tc.thinking {
			t.Errorf("%s: thinking = %q, want %q", tc.name, got, tc.thinking)
		}
	}
}

// With a forced tool the past turn is not given blank reasoning: thinking is off by then.
func TestAForcedToolLeavesThePastTurnAlone(t *testing.T) {
	const raw = `{"tool_choice":"required","messages":[{"role":"assistant","tool_calls":[{"id":"c1"}]}]}`
	if _, got := apply(t, "/v1/chat/completions", "deepseek", raw, "messages"); got != `[{"role":"assistant","tool_calls":[{"id":"c1"}]}]` {
		t.Errorf("messages = %s", got)
	}
}

// The output cap is renamed on the OpenAI chat shape only: on the Anthropic shape it is sent as it came.
func TestTheMessagesShapeIsSentAsItCame(t *testing.T) {
	body := decode(t, `{"max_tokens":16,"max_completion_tokens":8}`)
	changed, err := chain(t, "vendorfields").Apply(Context{Path: "/v1/messages", Provider: true, Vendor: "deepseek"}, body)
	if err != nil || changed || len(body) != 2 {
		t.Errorf("changed=%v err=%v body = %v", changed, err, body)
	}
}

// Registered is not running: the rename only happens while the default list names it.
func TestVendorFieldsRunsByDefault(t *testing.T) {
	body := decode(t, `{"max_tokens":16}`)
	chain(t, Default...).Apply(Context{Path: "/v1/chat/completions", Provider: true, Vendor: "openai"}, body)
	if _, present := body["max_tokens"]; present {
		t.Error("max_tokens survived the default chain")
	}
}
