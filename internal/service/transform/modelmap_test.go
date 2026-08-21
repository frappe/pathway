package transform

import "testing"

// The whole point: a customer's id and the vendor's dated snapshot are different strings, and only
// the control plane knows the mapping.
func TestTheUpstreamModelReplacesTheOneTheCallerSent(t *testing.T) {
	body := decode(t, `{"model":"anthropic/claude-4-5","max_tokens":16}`)
	ctx := Context{Path: "/v1/messages", UpstreamModel: "claude-sonnet-4-5-20250929"}
	changed, err := chain(t, "modelmap").Apply(ctx, body)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got := string(body["model"]); got != `"claude-sonnet-4-5-20250929"` {
		t.Errorf("model = %s", got)
	}
	if got := string(body["max_tokens"]); got != "16" {
		t.Errorf("an untouched field was re-encoded: max_tokens = %s", got)
	}
}

// Blank is every route we run ourselves, which is all of them today. Reporting a change would
// re-encode a body that nothing rewrote, and the caller's bytes must reach vLLM as sent.
func TestABlankUpstreamModelLeavesTheBodyAlone(t *testing.T) {
	body := decode(t, `{"model":"frappe/qwen3-8b"}`)
	changed, err := chain(t, "modelmap").Apply(Context{Path: "/v1/chat/completions"}, body)
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got := string(body["model"]); got != `"frappe/qwen3-8b"` {
		t.Errorf("model = %s", got)
	}
}

// Adding the field would send a request the caller did not make. An endpoint whose schema carries
// no `model` has to stay that way.
func TestABodyWithNoModelIsNotGivenOne(t *testing.T) {
	body := decode(t, `{"input":"hello"}`)
	changed, err := chain(t, "modelmap").Apply(Context{UpstreamModel: "claude-x"}, body)
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if _, present := body["model"]; present {
		t.Error("modelmap invented a model field")
	}
}

// The rewrite has to happen wherever a JSON body carries a model, not on a list of paths that a
// new OpenAI surface would silently fall off.
func TestModelMapAppliesToEveryPath(t *testing.T) {
	if endpoints := (modelMap{}).Endpoints(); len(endpoints) != 0 {
		t.Errorf("Endpoints() = %v, want empty (every path)", endpoints)
	}
}

// Both default lists must agree, or the dataplane test asserts a chain production never runs.
func TestModelMapRunsByDefaultBeforeStreamUsage(t *testing.T) {
	if len(Default) < 2 || Default[0] != "modelmap" {
		t.Fatalf("Default = %v, want modelmap first", Default)
	}
}
