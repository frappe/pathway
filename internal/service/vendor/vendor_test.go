package vendor

import (
	"encoding/json"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/frappe/pathway/internal/domain"
)

func TestHeadersFollowTheDialectUnlessTheVendorReadsOtherwise(t *testing.T) {
	for _, tc := range []struct {
		name, vendor, dialect, secret string
		want                          map[string]string
	}{
		{"an unmet OpenAI front", "nobody", domain.DialectOpenAI, "k", map[string]string{"Authorization": "Bearer k"}},
		{"an unmet Anthropic-shaped front: the key alone", "nobody", domain.DialectAnthropic, "k",
			map[string]string{"x-api-key": "k"}},
		{"Anthropic's own front: the key and its version", "anthropic", domain.DialectAnthropic, "k",
			map[string]string{"x-api-key": "k", "anthropic-version": "2023-06-01"}},
		{"Anthropic's own front, no secret: the version still", "anthropic", domain.DialectAnthropic, "",
			map[string]string{"anthropic-version": "2023-06-01"}},
		{"no secret, nothing", "openai", domain.DialectOpenAI, "", map[string]string{}},
		{"Baseten's Anthropic front reads a Bearer and asks no version", "baseten", domain.DialectAnthropic, "k",
			map[string]string{"Authorization": "Bearer k"}},
		{"Baseten's OpenAI front", "baseten", domain.DialectOpenAI, "k", map[string]string{"Authorization": "Bearer k"}},
	} {
		if got := For(tc.vendor).Headers(tc.dialect, tc.secret); !maps.Equal(got, tc.want) {
			t.Errorf("%s: headers = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A vendor is the type of its name and an engine of ours one of its own; one the table does not
// name is the shape as documented. Each answers for itself, and the drivers reach the override.
func TestEachUpstreamIsItsOwnType(t *testing.T) {
	if For("nobody") != Default || For("openai") == Default || For("morph") == Default || Engine == Default {
		t.Error("the table does not tell its upstreams apart")
	}
	if Of(domain.Route{Kind: "provider", Vendor: "openai"}) != For("openai") || Of(domain.Route{Kind: "direct"}) != Engine {
		t.Error("Of does not follow the row")
	}
	if Default.UsagePerChunk() || !Engine.UsagePerChunk() || !For("baseten").UsagePerChunk() || For("openai").UsagePerChunk() {
		t.Error("usage per chunk is asked of the wrong upstreams")
	}
	for _, tc := range []struct {
		name string
		v    Vendor
		want string
	}{
		{"openai", For("openai"), MaxCompletionTokens},
		{"an unmet vendor", Default, MaxTokens},
		{"an engine of ours", Engine, MaxTokens},
	} {
		req := &Request{Body: map[string]json.RawMessage{MaxTokens: json.RawMessage("16")}}
		if _, err := RewriteChat(tc.v, req); err != nil {
			t.Fatal(err)
		}
		if string(req.Body[tc.want]) != "16" || len(req.Body) != 1 {
			t.Errorf("%s: cap = %v, want 16 under %s alone", tc.name, req.Body, tc.want)
		}
	}
}

// The row switches a quirk off for one model by name; every name is one the gateway lists, and a
// name it does not know is not a quirk.
func TestARowSwitchesAQuirkOffByName(t *testing.T) {
	req := &Request{Body: map[string]json.RawMessage{"temperature": json.RawMessage("0.2"), "tools": json.RawMessage(`[{}]`)},
		Skip: []string{"default_sampling"}}
	if _, err := RewriteChat(For("openai"), req); err != nil {
		t.Fatal(err)
	}
	if string(req.Body["temperature"]) != "0.2" || string(req.Body["reasoning_effort"]) != `"none"` {
		t.Errorf("skipping default_sampling: body = %v", req.Body)
	}
	want := []string{"blank_reasoning_content", "blank_thinking_block", "default_sampling", "developer_as_system",
		"fold_tool_changes", "name_output_cap", "unreason_with_tools", "unthink_forced_function",
		"unthink_forced_tool", "untype_custom_tools", "usage_per_chunk"}
	if got := QuirkNames(); !slices.Equal(got, want) {
		t.Errorf("QuirkNames() = %v, want %v", got, want)
	}
	if !IsQuirk("default_sampling") || IsQuirk("DefaultSampling") || IsQuirk("") {
		t.Error("IsQuirk reads the wrong names")
	}
}

// Each upstream declares the quirks it has, and the declaration is the truth: the method of a
// declared quirk changes a body that calls for it, and an undeclared one's leaves it alone. The
// bodies are the least each rewrite acts on.
func TestEachUpstreamDeclaresTheQuirksItHas(t *testing.T) {
	calls := map[string]string{
		"name_output_cap":         `{"max_tokens":16,"max_completion_tokens":8}`,
		"developer_as_system":     `{"messages":[{"role":"developer","content":"x"}]}`,
		"unthink_forced_function": `{"tool_choice":"required","tools":[{"type":"function","function":{"name":"a"}}]}`,
		"blank_reasoning_content": `{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"a","arguments":"{}"}}]}]}`,
		"unreason_with_tools":     `{"tools":[{"type":"function","function":{"name":"a"}}]}`,
		"default_sampling":        `{"temperature":0.2}`,
		"untype_custom_tools":     `{"tools":[{"type":"custom","name":"a"}]}`,
		"fold_tool_changes":       `{"messages":[{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"a","input_schema":{"type":"object"}}}}]}]}`,
		"unthink_forced_tool":     `{"tool_choice":{"type":"tool","name":"a"}}`,
		"blank_thinking_block":    `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"a","input":{}}]}]}`,
	}
	upstreams := map[string]Vendor{"an unmet vendor": Default, "an engine of ours": Engine}
	maps.Copy(upstreams, known)
	for name, v := range upstreams {
		declared := v.Quirks()
		for _, quirk := range declared {
			if !IsQuirk(quirk) {
				t.Errorf("%s declares %q, which is no quirk", name, quirk)
			}
		}
		for _, q := range slices.Concat(chat, messages) {
			body := map[string]json.RawMessage{}
			if err := json.Unmarshal([]byte(calls[q.name]), &body); err != nil {
				t.Fatalf("%s: %v", q.name, err)
			}
			changed, err := q.run(v, &Request{Body: body})
			if err != nil {
				t.Fatalf("%s %s: %v", name, q.name, err)
			}
			if has := slices.Contains(declared, q.name); changed != has {
				t.Errorf("%s: %s changed the body: %v, declared: %v", name, q.name, changed, has)
			}
		}
		if has := slices.Contains(declared, QuirkUsagePerChunk); v.UsagePerChunk() != has {
			t.Errorf("%s: usage per chunk %v, declared %v", name, v.UsagePerChunk(), has)
		}
	}
}

// Morph takes a system turn of text alone, so the turn Claude Code adds a tool with is folded: the
// definition into `tools`, a removal out of them, the text and its breakpoint where they were. A
// turn of plain text and every other message stay as sent, and the caller is told nothing.
func TestMorphFoldsAMidConversationToolChangeIntoTools(t *testing.T) {
	body := map[string]json.RawMessage{}
	sent := `{"tools":[{"name":"old","input_schema":{"type":"object"}}],"messages":[
		{"role":"user","content":"hi"},
		{"role":"system","content":[
			{"type":"text","text":"reminder","cache_control":{"type":"ephemeral","ttl":"1h"}},
			{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"new","description":"d","input_schema":{"type":"object"}}}},
			{"type":"tool_removal","tool":{"type":"tool_reference","name":"old"}}]},
		{"role":"system","content":"a plain one"}]}`
	if err := json.Unmarshal([]byte(sent), &body); err != nil {
		t.Fatal(err)
	}
	req := &Request{Body: body}
	changed, err := RewriteMessages(For("morph"), req)
	if err != nil || !changed || len(req.Changed) != 0 {
		t.Fatalf("changed %v, err %v, told %v", changed, err, req.Changed)
	}
	var tools []struct{ Name string }
	if err := json.Unmarshal(req.Body["tools"], &tools); err != nil || len(tools) != 1 || tools[0].Name != "new" {
		t.Errorf("tools = %s, want the added one alone", req.Body["tools"])
	}
	var messages []struct {
		Role    string
		Content json.RawMessage
	}
	if err := json.Unmarshal(req.Body["messages"], &messages); err != nil || len(messages) != 3 {
		t.Fatalf("messages = %s", req.Body["messages"])
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(messages[1].Content, &blocks); err != nil || len(blocks) != 1 ||
		string(blocks[0]["text"]) != `"reminder"` || string(blocks[0]["cache_control"]) != `{"type":"ephemeral","ttl":"1h"}` {
		t.Errorf("system turn = %s, want its text and breakpoint alone", messages[1].Content)
	}
	if string(messages[0].Content) != `"hi"` || string(messages[2].Content) != `"a plain one"` {
		t.Errorf("other messages changed: %s", req.Body["messages"])
	}

	untouched := &Request{Body: map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"system","content":"text"}]`)}}
	if changed, _ := RewriteMessages(For("morph"), untouched); changed {
		t.Error("a request with no tool change was rewritten")
	}
}

// header is a response's headers as a vendor reads them, without net/http.
type header map[string]string

func (h header) Get(name string) string { return h[name] }

// What a vendor says of the hop is read under the names that vendor uses. Headroom is kept on a
// refusal only. An engine has nothing to say.
func TestAVendorsAccountOfTheHopIsReadUnderItsOwnNames(t *testing.T) {
	sent := header{"X-Request-Id": "theirs", "Openai-Processing-Ms": "123", "X-Baseten-Model-Prediction-Attempts": "2",
		"Retry-After": "7", "X-Ratelimit-Remaining-Requests": "0", "Anthropic-Ratelimit-Tokens-Remaining": "15000"}
	for _, tc := range []struct {
		name   string
		v      Vendor
		status int
		want   Answer
	}{
		{"an unmet vendor answering", Default, 200, Answer{UpstreamRID: "theirs", UpstreamMS: 123}},
		{"an unmet vendor refusing", Default, 429, Answer{UpstreamRID: "theirs", UpstreamMS: 123,
			RetryAfter: "7", RemainingRequests: "0", RemainingTokens: "15000"}},
		{"baseten", For("baseten"), 200, Answer{UpstreamRID: "theirs", UpstreamMS: 123, UpstreamAttempts: 2}},
		{"an engine of ours", Engine, 429, Answer{}},
	} {
		if got := tc.v.Answered(tc.status, sent); got != tc.want {
			t.Errorf("%s: answered %+v, want %+v", tc.name, got, tc.want)
		}
	}
	own := header{"X-Baseten-Request-Id": "b"}
	if got := For("baseten").Answered(200, own).UpstreamRID; got != "b" {
		t.Errorf("baseten's own request id = %q", got)
	}
	if got := Default.Answered(200, own).UpstreamRID; got != "" {
		t.Errorf("another vendor read baseten's request id: %q", got)
	}
}

// A refusal is read to its end on a vendor hop and never on an engine's; what it says is classed
// in domain's names. A response is relayed as it came.
func TestARefusalIsPeekedAndClassedPerUpstream(t *testing.T) {
	if !Default.Peeks(400) || !Default.Peeks(429) || Default.Peeks(500) || Engine.Peeks(400) {
		t.Error("peeks: wrong statuses or wrong upstream")
	}
	if got := Default.Classify(429, []byte(`{"error":{"code":"insufficient_quota"}}`)); got != domain.ClassQuota {
		t.Errorf("classify = %q, want %q", got, domain.ClassQuota)
	}
	body := io.NopCloser(strings.NewReader("{}"))
	if Default.Relay(200, false, body) != body {
		t.Error("a response was not relayed as it came")
	}
}

// Each upstream puts the tenant where it partitions its prompt cache, prefixes a value the caller
// sent, and writes nothing with no tenant to name; one with no such field changes nothing. Only
// DeepSeek reads one on the Anthropic shape, inside metadata, whose other keys keep their bytes.
func TestEachUpstreamPartitionsItsCacheWhereItReadsIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		v     Vendor
		field string // what CacheKey writes on the chat shape; blank when the upstream has none
	}{
		{"an engine of ours", Engine, "cache_salt"},
		{"openai", For("openai"), "prompt_cache_key"},
		{"deepseek", For("deepseek"), "user_id"},
		{"baseten", For("baseten"), ""},
		{"anthropic", For("anthropic"), ""},
		{"morph", For("morph"), ""},
		{"an unmet vendor", Default, ""},
	} {
		req := &Request{Body: map[string]json.RawMessage{"model": json.RawMessage(`"m"`)}, Team: "t"}
		changed, err := tc.v.CacheKey(req)
		if err != nil {
			t.Fatal(err)
		}
		if tc.field == "" {
			if changed || len(req.Body) != 1 {
				t.Errorf("%s: wrote a cache key: %v", tc.name, req.Body)
			}
			continue
		}
		if !changed || string(req.Body[tc.field]) != `"t"` {
			t.Errorf("%s: %s = %s, want the tenant", tc.name, tc.field, req.Body[tc.field])
		}
		req.Body[tc.field] = json.RawMessage(`"mine"`)
		if _, err := tc.v.CacheKey(req); err != nil || string(req.Body[tc.field]) != `"t:mine"` {
			t.Errorf("%s: a sent value = %s, want it under the tenant", tc.name, req.Body[tc.field])
		}
		req.Team, req.Body[tc.field] = "", json.RawMessage(`"mine"`)
		if changed, _ := tc.v.CacheKey(req); changed || string(req.Body[tc.field]) != `"mine"` {
			t.Errorf("%s: with no tenant %s = %s", tc.name, tc.field, req.Body[tc.field])
		}
	}

	req := &Request{Body: map[string]json.RawMessage{"metadata": json.RawMessage(`{"other":1.000,"user_id":"u"}`)}, Team: "t"}
	if changed, err := For("deepseek").MetadataCacheKey(req); err != nil || !changed || string(req.Body["metadata"]) != `{"other":1.000,"user_id":"t:u"}` {
		t.Errorf("deepseek metadata = %s, changed = %v, err = %v", req.Body["metadata"], changed, err)
	}
	for name, v := range map[string]Vendor{"openai": For("openai"), "an engine of ours": Engine, "an unmet vendor": Default} {
		req := &Request{Body: map[string]json.RawMessage{}, Team: "t"}
		if changed, _ := v.MetadataCacheKey(req); changed || len(req.Body) != 0 {
			t.Errorf("%s wrote metadata: %v", name, req.Body)
		}
	}
}
