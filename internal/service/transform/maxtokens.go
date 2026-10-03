package transform

func init() {
	Register(maxTokens{})
}

// maxTokens sends a vendor the output cap under the one name it honours: OpenAI 400s on
// `max_tokens`, every other vendor takes only that and ignores `max_completion_tokens` — which
// silently uncaps the output. Engines we run accept both and are left alone.
type maxTokens struct{}

func (maxTokens) Name() string        { return "maxtokens" }
func (maxTokens) Endpoints() []string { return []string{"/v1/chat/completions"} }

func (maxTokens) Apply(ctx Context, body Body) (bool, error) {
	if ctx.Vendor == "" {
		return false, nil
	}
	keep, drop := "max_tokens", "max_completion_tokens"
	if ctx.Vendor == "openai" {
		keep, drop = drop, keep
	}
	if _, present := body[drop]; !present {
		return false, nil
	}
	// Both sent: the newer field wins, as it does at OpenAI, which deprecated the other for it.
	value, newer := body["max_completion_tokens"]
	if !newer {
		value = body["max_tokens"]
	}
	body[keep] = value
	delete(body, drop)
	return true, nil
}
