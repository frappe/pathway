package transform

import "encoding/json"

// The two rules the OpenResty data path always applied. Both are gated to the endpoints whose vLLM
// schema carries the field: elsewhere the body is left untouched rather than risking a field the
// schema rejects.

var completions = []string{"/v1/chat/completions", "/v1/completions"}

func init() {
	Register(streamUsage{})
}

// streamUsage guarantees usage on a streaming response. Without `include_usage` a streaming request
// reports nothing and bills as zero, which is the whole reason metering can trust the stream. An
// upstream the `vendors` table says takes it is also asked for the running count on every chunk
// (`continuous_usage_stats`), so a stream that is cut is billed for what it had generated.
type streamUsage struct{}

func (streamUsage) Name() string        { return "streamusage" }
func (streamUsage) Endpoints() []string { return completions }

func (streamUsage) Apply(ctx Context, body Body) (bool, error) {
	var streaming bool
	raw, present := body["stream"]
	if !present {
		return false, nil
	}
	if err := json.Unmarshal(raw, &streaming); err != nil || !streaming {
		// A non-boolean `stream` is the engine's business to reject, not ours to fail on.
		return false, nil
	}

	options := map[string]json.RawMessage{}
	if existing, ok := body["stream_options"]; ok {
		// Merged, not replaced: a caller may have set other options, and dropping them here would
		// be a silent rewrite of their request.
		if err := json.Unmarshal(existing, &options); err != nil {
			options = map[string]json.RawMessage{}
		}
	}
	forced := []string{"include_usage"}
	if upstreamOf(ctx).usagePerChunk {
		forced = append(forced, "continuous_usage_stats")
	}
	changed := false
	for _, option := range forced {
		if string(options[option]) != "true" {
			options[option], changed = json.RawMessage("true"), true
		}
	}
	if !changed {
		return false, nil
	}

	encoded, err := json.Marshal(options)
	if err != nil {
		return false, err
	}
	body["stream_options"] = encoded
	return true, nil
}
