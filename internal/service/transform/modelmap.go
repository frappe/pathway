package transform

import "encoding/json"

func init() {
	Register(modelMap{})
}

// modelMap rewrites `model` to what the chosen upstream actually answers to. The control plane
// decides the string and pushes it on the route; nothing is derived here, so one id can map to a
// vendor's dated snapshot without the caller ever seeing it.
type modelMap struct{}

func (modelMap) Name() string { return "modelmap" }

// Every JSON path: a model id is a model id on whichever surface it arrives.
func (modelMap) Endpoints() []string { return nil }

func (modelMap) Apply(ctx Context, body Body) (bool, error) {
	if ctx.UpstreamModel == "" {
		return false, nil
	}
	if _, present := body["model"]; !present {
		// Nothing to rewrite. Adding the field would change a request the caller did not make.
		return false, nil
	}
	encoded, err := json.Marshal(ctx.UpstreamModel)
	if err != nil {
		return false, err
	}
	body["model"] = encoded
	return true, nil
}
