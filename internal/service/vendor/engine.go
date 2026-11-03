package vendor

// engine is an upstream we run: vLLM reads the cap under either name, takes the running usage
// count, answers under no vendor headers, and its refusals are its own to be read as sent.
type engine struct{ base }

// NameOutputCap: as sent.
func (engine) NameOutputCap(*Request) (bool, error) { return false, nil }

// CacheKey: vLLM and SGLang partition the prefix cache by a top-level `cache_salt`, on either shape.
func (engine) CacheKey(req *Request) (bool, error) {
	return namespaceKey(req.Team, req.Body, "cache_salt")
}

func (engine) UsagePerChunk() bool { return true }

func (engine) Quirks() []string { return []string{QuirkUsagePerChunk} }

// Answered: an engine or ingress only echoes our id, and has nothing else to say.
func (engine) Answered(int, Header) Answer { return Answer{} }

// Peeks: an engine's refusals hide nothing under their status.
func (engine) Peeks(int) bool { return false }
