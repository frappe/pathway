package domain

// Usage is a token count normalized across the OpenAI and Anthropic response shapes, so both mean
// the same thing: Prompt is the FULL input including cache, Total = Prompt + Completion, and
// Cached ⊆ Prompt. Budget is Total - Cached on either.
type Usage struct {
	Prompt     int
	Completion int
	Total      int
	// Cached = prompt tokens served from the prefix cache, credited. Already inside prompt_tokens
	// on the OpenAI shape; separate from input_tokens on the Anthropic one, so it is folded back in
	// here. cache_creation is a write — billed, so it lands in Prompt but never in Cached.
	Cached int
	// CacheWrite is every prompt token written to the cache, CacheWrite1h the part of it written
	// with the hour TTL (Anthropic prices the two apart). Both ⊆ Prompt, CacheWrite1h ⊆ CacheWrite.
	// Already inside prompt_tokens on the OpenAI shape, like Cached.
	CacheWrite   int
	CacheWrite1h int
	// Audio = prompt tokens that were audio, priced apart from text. ⊆ Prompt, OpenAI shape only.
	Audio int
	// CompletionAudio = completion tokens that were audio, priced apart from text. ⊆ Completion,
	// OpenAI shape only.
	CompletionAudio int
	// Seconds of audio processed — all a duration-shaped transcription reports. Display only.
	Seconds int
}

// MergeUsage folds two usage reports of one request into one: the larger of each field, because
// counts only grow within a request, then Total raised to Prompt + Completion. An Anthropic stream
// reports the prompt on its first event and the output on its last.
func MergeUsage(a, b Usage) Usage {
	u := Usage{
		Prompt:          max(a.Prompt, b.Prompt),
		Completion:      max(a.Completion, b.Completion),
		Total:           max(a.Total, b.Total),
		Cached:          max(a.Cached, b.Cached),
		CacheWrite:      max(a.CacheWrite, b.CacheWrite),
		CacheWrite1h:    max(a.CacheWrite1h, b.CacheWrite1h),
		Audio:           max(a.Audio, b.Audio),
		CompletionAudio: max(a.CompletionAudio, b.CompletionAudio),
		Seconds:         max(a.Seconds, b.Seconds),
	}
	u.Total = max(u.Total, u.Prompt+u.Completion)
	return u
}
