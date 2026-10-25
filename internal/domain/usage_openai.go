package domain

import (
	"encoding/json"
	"math"
)

// openaiUsage reads the OpenAI shape, which vLLM and every OpenAI-compatible vendor share: one
// usage object at the top of the body and of the final stream chunk alike, null on the chunks
// before it. The responses API nests it under `response`; a transcription may report a duration.
type openaiUsage struct{}

type openaiWire struct {
	Usage    *openaiUsageWire `json:"usage"`
	Response *struct {
		Usage *openaiUsageWire `json:"usage"`
	} `json:"response"`
	// Duration is what a verbose_json transcription carries instead of a usage object.
	Duration *float64 `json:"duration"`
}

// openaiUsageWire is the usage object. Pointers tell absent from zero: a chunk before the final
// frame carries none of these, a transcription's duration shape carries only type and seconds.
type openaiUsageWire struct {
	PromptTokens     *int `json:"prompt_tokens"`
	InputTokens      *int `json:"input_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	OutputTokens     *int `json:"output_tokens"`
	TotalTokens      *int `json:"total_tokens"`
	// The prompt breakdown under its three names: chat, responses, transcription.
	PromptTokensDetails *openaiDetails `json:"prompt_tokens_details"`
	InputTokensDetails  *openaiDetails `json:"input_tokens_details"`
	InputTokenDetails   *openaiDetails `json:"input_token_details"`
	CompletionDetails   *openaiDetails `json:"completion_tokens_details"`
	Type                string         `json:"type"`
	Seconds             *int           `json:"seconds"`
}

type openaiDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	AudioTokens      int `json:"audio_tokens"`
}

func (openaiUsage) Event(payload []byte) (Usage, bool) {
	var w openaiWire
	if json.Unmarshal(payload, &w) != nil {
		return Usage{}, false
	}
	return w.usage()
}

func (openaiUsage) Document(body []byte) (Usage, bool) {
	var w openaiWire
	if json.Unmarshal(body, &w) == nil {
		return w.usage()
	}
	return openaiUsageObject(tailUsageObject(body))
}

func openaiUsageObject(raw []byte) (Usage, bool) {
	var object openaiUsageWire
	if raw == nil || json.Unmarshal(raw, &object) != nil {
		return Usage{}, false
	}
	return object.usage()
}

func (w openaiWire) usage() (Usage, bool) {
	object := w.Usage
	if object == nil && w.Response != nil {
		object = w.Response.Usage
	}
	if object != nil {
		if u, ok := object.usage(); ok {
			return u, true
		}
	}
	if w.Duration != nil {
		return Usage{Seconds: int(math.Ceil(*w.Duration))}, true
	}
	return Usage{}, false
}

func (w openaiUsageWire) usage() (Usage, bool) {
	// A transcription's usage is a duration, not tokens.
	if w.Seconds != nil && w.Type == "duration" {
		return Usage{Seconds: *w.Seconds}, true
	}
	prompt, promptOK := first(w.PromptTokens, w.InputTokens)
	completion, completionOK := first(w.CompletionTokens, w.OutputTokens)
	if !promptOK && !completionOK && w.TotalTokens == nil {
		return Usage{}, false
	}
	// prompt_tokens already INCLUDES the cached and written parts; the breakdown names them.
	u := Usage{Prompt: prompt, Completion: completion, Total: prompt + completion}
	if w.TotalTokens != nil {
		u.Total = *w.TotalTokens
	}
	if details, ok := first(w.PromptTokensDetails, w.InputTokensDetails, w.InputTokenDetails); ok {
		u.Cached, u.CacheWrite, u.Audio = details.CachedTokens, details.CacheWriteTokens, details.AudioTokens
	}
	if w.CompletionDetails != nil {
		u.CompletionAudio = w.CompletionDetails.AudioTokens
	}
	return u, true
}

// first is the first of the pointers that is set, false when none is: the shapes name one field
// several ways, and a null arrives as nil like an absent one.
func first[T any](candidates ...*T) (T, bool) {
	for _, candidate := range candidates {
		if candidate != nil {
			return *candidate, true
		}
	}
	var zero T
	return zero, false
}
