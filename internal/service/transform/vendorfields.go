package transform

func init() {
	Register(vendorFields{})
}

// vendorFields sends a vendor the output cap under the one name it reads, from the `vendors`
// table. A name the vendor does not read is refused by one and silently ignored by another, which
// uncaps the output. Engines we run read either name and get the body as sent. It runs per attempt
// on the client's own body, so a request that moves to another vendor is named afresh for that one.
type vendorFields struct{}

func (vendorFields) Name() string        { return "vendorfields" }
func (vendorFields) Endpoints() []string { return []string{"/v1/chat/completions"} }

func (vendorFields) Apply(ctx Context, body Body) (bool, error) {
	name := upstreamOf(ctx).outputCap
	if name == asSent {
		return false, nil
	}
	other := maxTokens
	if name == maxTokens {
		other = maxCompletionTokens
	}
	value, stray := body[other]
	if !stray {
		return false, nil
	}
	// Sent under both names, the newer one is the caller's word.
	if _, both := body[name]; !both || other == maxCompletionTokens {
		body[name] = value
	}
	delete(body, other)
	return true, nil
}
