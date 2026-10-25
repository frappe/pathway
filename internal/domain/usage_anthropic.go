package domain

import "encoding/json"

// anthropicUsage reads the Anthropic shape. input_tokens EXCLUDES the cache buckets, which are
// folded back in so Prompt and Total mean what they mean on the OpenAI shape. A stream reports the
// prompt under message.usage on message_start and the output on message_delta; a count_tokens
// answer is input_tokens alone.
type anthropicUsage struct{}

type anthropicWire struct {
	Usage   *anthropicUsageWire `json:"usage"`
	Message *struct {
		Usage *anthropicUsageWire `json:"usage"`
	} `json:"message"`
}

type anthropicUsageWire struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	CacheCreation            *struct {
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (anthropicUsage) Event(payload []byte) (Usage, bool) {
	var w anthropicWire
	if json.Unmarshal(payload, &w) != nil {
		return Usage{}, false
	}
	return w.usage()
}

func (anthropicUsage) Document(body []byte) (Usage, bool) {
	var w anthropicWire
	if json.Unmarshal(body, &w) == nil {
		if u, ok := w.usage(); ok {
			return u, true
		}
		// A count_tokens answer is the usage object itself, unwrapped.
		return anthropicUsageObject(body)
	}
	return anthropicUsageObject(tailUsageObject(body))
}

func anthropicUsageObject(raw []byte) (Usage, bool) {
	var object anthropicUsageWire
	if raw == nil || json.Unmarshal(raw, &object) != nil {
		return Usage{}, false
	}
	return object.usage()
}

func (w anthropicWire) usage() (Usage, bool) {
	object := w.Usage
	if object == nil && w.Message != nil {
		object = w.Message.Usage
	}
	if object == nil {
		return Usage{}, false
	}
	return object.usage()
}

func (w anthropicUsageWire) usage() (Usage, bool) {
	if w.InputTokens == nil && w.OutputTokens == nil && w.CacheReadInputTokens == nil && w.CacheCreationInputTokens == nil {
		return Usage{}, false
	}
	input, _ := first(w.InputTokens)
	output, _ := first(w.OutputTokens)
	read, _ := first(w.CacheReadInputTokens)
	written, _ := first(w.CacheCreationInputTokens)
	// Credit reads only — cache_creation is a write, billed, so it lands in Prompt and never in
	// Cached. The hour bucket is a part of the writes and can never exceed them.
	u := Usage{Prompt: input + read + written, Completion: output, Cached: read, CacheWrite: written}
	if w.CacheCreation != nil {
		u.CacheWrite1h = min(w.CacheCreation.Ephemeral1h, written)
	}
	u.Total = u.Prompt + u.Completion
	return u, true
}
