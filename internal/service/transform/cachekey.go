package transform

import "github.com/frappe/pathway/internal/service/vendor"

func init() {
	Register(cacheKey{})
}

// cacheKey pins a vendor's prompt-cache partition to the tenant, in the field the vendor's type
// names (vendor.Cache: OpenAI's prompt_cache_key, DeepSeek's user_id, inside metadata on the
// Anthropic shape): the team, or "<team>:<caller's value>" when the caller sent one. Passed through
// raw, one tenant could echo another's key and land inside their cache namespace; sent by nobody,
// every tenant on the credential shares one. A type that names no field changes nothing: Anthropic's
// cache is per workspace, Baseten's affinity header is routing. An engine of ours is cachesalt's.
type cacheKey struct{}

func (cacheKey) Name() string        { return "cachekey" }
func (cacheKey) Endpoints() []string { return []string{"/v1/chat/completions", messages} }

func (cacheKey) Apply(ctx Context, body Body) (bool, error) {
	if !ctx.Provider {
		return false, nil
	}
	req := &vendor.Request{Body: body, Team: ctx.Team}
	if ctx.Path == messages {
		return upstreamOf(ctx).MetadataCacheKey(req)
	}
	return upstreamOf(ctx).CacheKey(req)
}
