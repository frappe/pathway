package transform

import "github.com/frappe/pathway/internal/service/vendor"

func init() {
	Register(vendorFields{})
}

const messages = "/v1/messages"

// vendorFields sends an upstream a field the way it reads it: each rewrite of the upstream's own
// type in the `vendor` package, run in that package's order, on the shape the path names. It runs
// per attempt on the client's own body, so a request that moves to another vendor is written
// afresh for that one.
type vendorFields struct{}

func (vendorFields) Name() string        { return "vendorfields" }
func (vendorFields) Endpoints() []string { return []string{"/v1/chat/completions", messages} }

// upstreamOf is the upstream the hop a request is on dials.
func upstreamOf(ctx Context) vendor.Vendor {
	if !ctx.Provider {
		return vendor.Engine
	}
	return vendor.For(ctx.Vendor)
}

func (vendorFields) Apply(ctx Context, body Body) (bool, error) {
	req := &vendor.Request{Body: body, Skip: ctx.SkipQuirks}
	defer func() {
		for _, change := range req.Changed {
			ctx.tell(change)
		}
	}()
	if ctx.Path != messages {
		return vendor.RewriteChat(upstreamOf(ctx), req)
	}
	dropped := dropSpeed(ctx, body)
	changed, err := vendor.RewriteMessages(upstreamOf(ctx), req)
	return dropped || changed, err
}

// dropSpeed takes fast mode off a request to a vendor. `speed` is Anthropic's faster tier at twice
// the rate, and nothing here prices it: the request goes at the standard speed and the caller is
// told. Ours, not a vendor's quirk, so every vendor's Anthropic front gets it and no vendor type
// has a say. An engine we run gets the body as sent. OpenAI's tier is `service_tier`, which the
// `servicetier` transform drops on every hop.
func dropSpeed(ctx Context, body Body) bool {
	if _, sent := body["speed"]; !ctx.Provider || !sent {
		return false
	}
	delete(body, "speed")
	ctx.tell("speed=default")
	return true
}
