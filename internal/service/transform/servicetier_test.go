package transform

import "testing"

// The tier is dropped on every hop and surface, and the rest of the body is left as it was sent.
func TestAServiceTierNeverReachesAnUpstream(t *testing.T) {
	for _, ctx := range []Context{
		{Path: "/v1/chat/completions"},
		{Path: "/v1/messages", Provider: true},
	} {
		body := saltBody(t, `{"model":"m","service_tier":"priority"}`)
		changed, err := serviceTier{}.Apply(ctx, body)
		if err != nil || !changed {
			t.Fatalf("%s: changed=%v err=%v", ctx.Path, changed, err)
		}
		if _, present := body["service_tier"]; present {
			t.Errorf("%s: service_tier reached the upstream", ctx.Path)
		}
		if string(body["model"]) != `"m"` {
			t.Errorf("%s: model = %s", ctx.Path, body["model"])
		}
	}
}

// A body that sent none is forwarded byte-for-byte.
func TestABodyWithNoServiceTierIsUntouched(t *testing.T) {
	changed, _ := serviceTier{}.Apply(Context{}, saltBody(t, `{"model":"m"}`))
	if changed {
		t.Error("a body with no service_tier was reported changed")
	}
}

// Registered is not running: the tier is only dropped while both default lists name it.
func TestServiceTierRunsByDefault(t *testing.T) {
	chain, err := NewChain(Default)
	if err != nil {
		t.Fatal(err)
	}
	body := saltBody(t, `{"model":"m","service_tier":"flex"}`)
	if changed, _ := chain.Apply(Context{Path: "/v1/chat/completions"}, body); !changed {
		t.Error("the default chain left service_tier in place")
	}
	if _, present := body["service_tier"]; present {
		t.Error("service_tier survived the default chain")
	}
}
