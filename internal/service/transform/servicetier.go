package transform

func init() {
	Register(serviceTier{})
}

// serviceTier drops a caller's `service_tier`. It picks the vendor's price class — priority bills
// above the rate a pricing holds — so the tier an upstream serves at is never the caller's to choose.
// The caller is told, as for `speed`: they asked for a tier and are served at the default one.
type serviceTier struct{}

func (serviceTier) Name() string { return "servicetier" }

// Every JSON path: both dialects carry the field at the top level.
func (serviceTier) Endpoints() []string { return nil }

func (serviceTier) Apply(ctx Context, body Body) (bool, error) {
	if _, present := body["service_tier"]; !present {
		return false, nil
	}
	delete(body, "service_tier")
	ctx.tell("service_tier=default")
	return true, nil
}
