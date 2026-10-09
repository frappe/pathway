package transform

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func decode(t *testing.T, raw string) Body {
	t.Helper()
	var body Body
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return body
}

func chain(t *testing.T, names ...string) *Chain {
	t.Helper()
	c, err := NewChain(names)
	if err != nil {
		t.Fatalf("NewChain(%v): %v", names, err)
	}
	return c
}

// logged runs `c` over `raw` for `ctx` and returns what it wrote to the process log.
func logged(t *testing.T, c *Chain, ctx Context, raw string) string {
	t.Helper()
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, nil)))
	defer slog.SetDefault(previous)
	if _, err := c.Apply(ctx, decode(t, raw)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// Whatever a transform removes is said once per team and field, by name and by the transform that
// did it, and never by value: nothing upstream errors on a field that is not there.
func TestEveryDroppedFieldIsWarnedOncePerTeam(t *testing.T) {
	c := chain(t, "modelmap", "cachesalt", "servicetier")
	vendor := Context{Path: "/v1/chat/completions", Team: "u1", Provider: true, UpstreamModel: "upstream-m"}
	const sent = `{"model":"m","cache_salt":"team-secret","service_tier":"priority"}`

	first := logged(t, c, vendor, sent)
	for _, want := range []string{
		"level=WARN", "team=u1",
		"field=cache_salt transform=cachesalt",
		"field=service_tier transform=servicetier",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("first request logged %q, want %q in it", first, want)
		}
	}
	if strings.Contains(first, "team-secret") || strings.Contains(first, "priority") {
		t.Errorf("a caller's value reached the log: %q", first)
	}
	// Rewritten is not dropped: modelmap changed `model`, and says nothing.
	if strings.Contains(first, "field=model") {
		t.Errorf("a rewritten field was logged as dropped: %q", first)
	}
	if again := logged(t, c, vendor, sent); again != "" {
		t.Errorf("the same team and fields logged twice: %q", again)
	}
	vendor.Team = "u2"
	if other := logged(t, c, vendor, sent); strings.Count(other, "team=u2") != 2 {
		t.Errorf("another team logged %q, want both fields", other)
	}
}

// A body nothing was taken from logs nothing, whatever else was rewritten on the way.
func TestNothingDroppedLogsNothing(t *testing.T) {
	c := chain(t, "modelmap", "streamusage", "cachesalt", "servicetier")
	ctx := Context{Path: "/v1/chat/completions", Team: "u1", UpstreamModel: "upstream-m"}
	if out := logged(t, c, ctx, `{"model":"m","stream":true,"cache_salt":"team-a"}`); out != "" {
		t.Errorf("logged %q", out)
	}
}

// A misspelt name must stop the process. `priority` silently not running is the difference between
// a client being unable to elevate itself and being able to, and nothing downstream looks wrong.
func TestAnUnknownTransformIsAStartupError(t *testing.T) {
	if _, err := NewChain([]string{"priorty"}); err == nil {
		t.Fatal("a misspelt transform name was accepted")
	}
}

// Without this a streaming request reports no usage at all and bills as zero. An upstream known to
// take it is also asked for the count on every chunk, which is what bills a stream that was cut;
// one that refuses the field (OpenAI), ignores it (DeepSeek) or is not known is not.
func TestStreamUsageIsForcedOnAStreamingCompletion(t *testing.T) {
	const both, final = `{"continuous_usage_stats":true,"include_usage":true}`, `{"include_usage":true}`
	for name, tc := range map[string]struct{ vendor, raw, want string }{
		"engine":                 {"", `{"model":"m","stream":true}`, both},
		"engine, caller said no": {"", `{"stream":true,"stream_options":{"include_usage":false,"continuous_usage_stats":false}}`, both},
		"baseten":                {"baseten", `{"model":"m","stream":true}`, both},
		"openai":                 {"openai", `{"model":"m","stream":true}`, final},
		"deepseek":               {"deepseek", `{"model":"m","stream":true}`, final},
		"not known":              {"mistral", `{"model":"m","stream":true}`, final},
	} {
		body := decode(t, tc.raw)
		ctx := Context{Path: "/v1/chat/completions", Provider: tc.vendor != "", Vendor: tc.vendor}
		changed, err := chain(t, "streamusage").Apply(ctx, body)
		if err != nil || !changed {
			t.Fatalf("%s: changed=%v err=%v", name, changed, err)
		}
		if got := string(body["stream_options"]); got != tc.want {
			t.Errorf("%s: stream_options = %s, want %s", name, got, tc.want)
		}
	}

	set := decode(t, `{"stream":true,"stream_options":{"include_usage":true,"continuous_usage_stats":true}}`)
	if changed, _ := chain(t, "streamusage").Apply(Context{Path: "/v1/chat/completions"}, set); changed {
		t.Error("a body that already asks for both was rewritten")
	}
}

// A caller may have set other stream options. Replacing the object wholesale would be a silent
// rewrite of their request.
func TestStreamUsageKeepsTheCallersOtherOptions(t *testing.T) {
	body := decode(t, `{"stream":true,"stream_options":{"include_obfuscation":false}}`)
	if _, err := chain(t, "streamusage").Apply(Context{Path: "/v1/chat/completions", Provider: true}, body); err != nil {
		t.Fatal(err)
	}
	var options map[string]any
	if err := json.Unmarshal(body["stream_options"], &options); err != nil {
		t.Fatal(err)
	}
	if options["include_obfuscation"] != false || len(options) != 2 {
		t.Errorf("the caller's own stream option was not kept as sent: %v", options)
	}
	if options["include_usage"] != true {
		t.Error("include_usage was not set")
	}
}

func TestStreamUsageLeavesANonStreamingBodyAlone(t *testing.T) {
	for _, raw := range []string{`{"model":"m"}`, `{"model":"m","stream":false}`} {
		body := decode(t, raw)
		changed, err := chain(t, "streamusage").Apply(Context{Path: "/v1/chat/completions"}, body)
		if err != nil || changed {
			t.Errorf("%s: changed=%v err=%v", raw, changed, err)
		}
		if _, present := body["stream_options"]; present {
			t.Errorf("%s: stream_options was invented", raw)
		}
	}
}

// The gate is the point of Endpoints(): a field one vLLM schema accepts, another rejects.
func TestTransformsAreGatedToTheirEndpoints(t *testing.T) {
	for _, path := range []string{"/v1/embeddings", "/v1/messages", "/v1/rerank"} {
		body := decode(t, `{"model":"m","stream":true}`)
		changed, err := chain(t, "streamusage").Apply(Context{Path: path}, body)
		if err != nil || changed {
			t.Errorf("%s: changed=%v err=%v", path, changed, err)
		}
		if len(body) != 2 {
			t.Errorf("%s: body was rewritten: %v", path, body)
		}
	}
}

// A body no transform touched must be forwarded byte-for-byte rather than re-encoded for nothing.
func TestNothingToDoReportsNoChange(t *testing.T) {
	body := decode(t, `{"model":"m","priority":-10}`)
	changed, err := chain(t, "streamusage").Apply(Context{Path: "/v1/completions"}, body)
	if err != nil || changed {
		t.Errorf("changed=%v err=%v, want no change", changed, err)
	}
}

// Values a transform did not touch must survive as they arrived. Decoding into `any` would turn a
// large integer into a float and hand the engine a different number.
func TestUntouchedFieldsKeepTheirExactBytes(t *testing.T) {
	const big = `12345678901234567890`
	body := decode(t, `{"model":"m","seed":`+big+`,"stream":true}`)
	if _, err := chain(t, "streamusage").Apply(
		Context{Path: "/v1/chat/completions"}, body); err != nil {
		t.Fatal(err)
	}
	if got := string(body["seed"]); got != big {
		t.Errorf("seed = %s, want %s", got, big)
	}
}
