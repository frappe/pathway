package transform

import "github.com/frappe/pathway/internal/service/vendor"

func init() {
	Register(cacheSalt{})
}

// cacheSalt pins an engine's prefix-cache namespace to the tenant, in the field the engine's type
// names (vendor.Engine.CacheKey: vLLM's and SGLang's cache_salt): the team, or "<team>:<caller's
// salt>" when the caller sent one. Passed through raw, one tenant could echo another's salt and
// land inside their namespace; sent by nobody, every tenant on the box shares one. A vendor has no
// such field and a strict one 400s on unknowns, so a salt is stripped from a vendor hop; a vendor's
// partition is cachekey's.
type cacheSalt struct{}

func (cacheSalt) Name() string { return "cachesalt" }

// The two chat paths an engine reads the field on. An embeddings body gets none: vLLM logs every
// field it ignores.
func (cacheSalt) Endpoints() []string { return []string{"/v1/chat/completions", messages} }

func (cacheSalt) Apply(ctx Context, body Body) (bool, error) {
	if ctx.Provider {
		if _, present := body["cache_salt"]; !present {
			return false, nil
		}
		delete(body, "cache_salt")
		return true, nil
	}
	return upstreamOf(ctx).CacheKey(&vendor.Request{Body: body, Team: ctx.Team})
}
