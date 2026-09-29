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

// Whatever a transform removes is said once per user and field, by name and by the transform that
// did it, and never by value: nothing upstream errors on a field that is not there.
func TestEveryDroppedFieldIsWarnedOncePerUser(t *testing.T) {
	c := chain(t, "modelmap", "cachesalt", "servicetier")
	vendor := Context{Path: "/v1/chat/completions", User: "u1", Provider: true, UpstreamModel: "upstream-m"}
	const sent = `{"model":"m","cache_salt":"team-secret","service_tier":"priority"}`

	first := logged(t, c, vendor, sent)
	for _, want := range []string{
		"level=WARN", "user=u1",
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
		t.Errorf("the same user and fields logged twice: %q", again)
	}
	vendor.User = "u2"
	if other := logged(t, c, vendor, sent); strings.Count(other, "user=u2") != 2 {
		t.Errorf("another user logged %q, want both fields", other)
	}
}

// A body nothing was taken from logs nothing, whatever else was rewritten on the way.
func TestNothingDroppedLogsNothing(t *testing.T) {
	c := chain(t, "modelmap", "streamusage", "cachesalt", "servicetier")
	ctx := Context{Path: "/v1/chat/completions", User: "u1", UpstreamModel: "upstream-m"}
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

// Without this a streaming request reports no usage at all and bills as zero.
func TestStreamUsageIsForcedOnAStreamingCompletion(t *testing.T) {
	body := decode(t, `{"model":"m","stream":true}`)
	changed, err := chain(t, "streamusage").Apply(Context{Path: "/v1/chat/completions"}, body)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got := string(body["stream_options"]); got != `{"include_usage":true}` {
		t.Errorf("stream_options = %s", got)
	}
}

// A caller may have set other stream options. Replacing the object wholesale would be a silent
// rewrite of their request.
func TestStreamUsageKeepsTheCallersOtherOptions(t *testing.T) {
	body := decode(t, `{"stream":true,"stream_options":{"continuous_usage_stats":true}}`)
	if _, err := chain(t, "streamusage").Apply(Context{Path: "/v1/chat/completions"}, body); err != nil {
		t.Fatal(err)
	}
	var options map[string]any
	if err := json.Unmarshal(body["stream_options"], &options); err != nil {
		t.Fatal(err)
	}
	if options["continuous_usage_stats"] != true {
		t.Error("the caller's own stream option was dropped")
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
