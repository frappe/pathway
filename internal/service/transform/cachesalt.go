package transform

import "encoding/json"

func init() {
	Register(cacheSalt{})
}

// cacheSalt namespaces a client-supplied vLLM cache_salt under the tenant, and strips it from
// vendor hops. Opt-in on purpose (user call, 2026-08-31): no salt is added when the caller sent
// none — unsalted tenants share the default prefix-cache namespace, trading isolation for
// cross-tenant cache hits. What is NOT optional is the prefix on a salt that was sent: passed
// through raw, one tenant could echo another's salt string and land inside their namespace.
type cacheSalt struct{}

func (cacheSalt) Name() string { return "cachesalt" }

// Every JSON path: the field rides whichever chat surface the caller used.
func (cacheSalt) Endpoints() []string { return nil }

func (cacheSalt) Apply(ctx Context, body Body) (bool, error) {
	raw, present := body["cache_salt"]
	if !present {
		return false, nil
	}
	// A vendor has no such field and a strict one 400s on unknowns; their cache is their own.
	if ctx.Provider {
		delete(body, "cache_salt")
		return true, nil
	}
	var salt string
	if json.Unmarshal(raw, &salt) != nil || ctx.User == "" {
		// Not a string — the engine's schema error to give, not ours to guess around.
		return false, nil
	}
	encoded, err := json.Marshal(ctx.User + ":" + salt)
	if err != nil {
		return false, err
	}
	body["cache_salt"] = encoded
	return true, nil
}
