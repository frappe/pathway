package domain

// Route is one placement of a model: the vLLM instance's URL plus the internal
// key to reach it. Mirrors an entry of Redis deploy:<model> (§8A.D).
type Route struct {
	EngineURL string `json:"engine_url"`
	// InternalKey is the one credential an engine or ingress row carries. Blank on a provider row,
	// whose credentials ride Credentials.
	InternalKey string `json:"internal_key"`
	// Credentials is every key the control plane holds with a vendor, each under the id it is
	// counted by, in the order the ring is walked. KeySelection names how one is picked per
	// request: only "round_robin" exists, so it is carried, not read.
	Credentials  []Credential `json:"credentials,omitempty"`
	KeySelection string       `json:"key_selection,omitempty"`
	Healthy      bool         `json:"healthy"`
	Region       string       `json:"region"`
	// Requests admitted to this engine and not yet metered, counted at decide time. Never pushed —
	// the control plane has no view of what is running right now.
	InFlight int `json:"-"`
	// The engine's --max-num-seqs: what it runs concurrently before vLLM starts queueing. 0 =
	// unset on the placement, which means no cap here rather than a guess at vLLM's default.
	Capacity int `json:"capacity"`
	// Model Deployment / pod id — which placement this is; the access line's `deployment`. One box
	// can serve the same model from two deployments, so Server alone names neither. Empty on a
	// route pushed before this field existed.
	Deployment string `json:"deployment"`
	Server     string `json:"server"` // inference-server / pod id — which box it is on
	// "ingress" when this row is an Ingress Server that will pick a replica of its own, "direct"
	// (or empty, on a route pushed before this field existed) when it is an engine to dial.
	Kind string `json:"kind"`
	// InputModalities and OutputModalities are what the model takes ("text", "image", …) and what
	// it gives ("text", "embeddings", …), stamped on every row of the model because deploy:<model>
	// is the only thing pushed per model. The outputs say which surfaces it answers on; the inputs
	// are carried, not read. Empty on a row pushed before the control plane declared them, which
	// reads as unrestricted.
	InputModalities  []string `json:"input_modalities,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
	// What this upstream answers to, when that is not the id the caller sent. The control plane
	// owns the mapping. Blank on every route we run ourselves — an engine is started under the
	// Grove id — and on any route pushed before this field existed, which reads as "send unchanged".
	UpstreamModel string `json:"upstream_model"`
	// The vendor's API version header, sent only on a provider route. Blank sends none.
	APIVersion string `json:"api_version"`
	// Dialect is the API shape this upstream speaks: "openai" or "anthropic". Blank means BOTH on a
	// row we run — vLLM answers both surfaces natively — and NOTHING on a provider: a vendor row
	// without a dialect is malformed, so it serves no path rather than a guessed one.
	Dialect string `json:"dialect"`
	// Pricing is the sell price in force, stamped on every row of the model. Absent on an unpriced
	// model: such a request costs 0, and the control plane's pull is where that shows up.
	Pricing *Pricing `json:"pricing,omitempty"`
}

// Pricing is one Model Pricing as the control plane pushed it: its id, which tags every counter it
// charged so the pull prices them with the same table, its rates per counter in nano-USD per unit
// (see CounterTable.Cost), and the counter table itself, so both sides count under the same rows.
type Pricing struct {
	ID       string           `json:"id"`
	Rates    map[string]int64 `json:"rates"`
	Counters CounterTable     `json:"counters,omitempty"`
}

// Credential is one key to dial an upstream with. ID is what the gateway counts and rotates by,
// never the secret; blank on the single credential of an engine row, which is counted nowhere.
type Credential struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
}

// Keyring is the credentials a request to this route may dial with: the pushed list, or the one
// InternalKey — possibly blank — of a row that carries none, so every row has at least one.
func (r Route) Keyring() []Credential {
	if len(r.Credentials) > 0 {
		return r.Credentials
	}
	return []Credential{{Secret: r.InternalKey}}
}

// KeyStats is what one credential has answered, lifetime, as the store counts it. OK is every
// answer that was not the key's fault nor the upstream's — a 2xx, or a 4xx the request earned;
// Failed is a 5xx or a hop that produced no status. Timestamps are unix seconds.
type KeyStats struct {
	Requests        int64 `json:"requests"`
	OK              int64 `json:"ok"`
	RateLimited     int64 `json:"rate_limited"`
	Rejected        int64 `json:"rejected"`
	Failed          int64 `json:"failed"`
	LastUsed        int64 `json:"last_used"`
	LastRateLimited int64 `json:"last_rate_limited"`
}

// IsKeyFailure reports whether an upstream status is the credential's fault rather than the
// request's or the upstream's: a rate limit, or a key refused outright. Only these move a request
// to another key — a 502 on one key is a 502 on the next.
func IsKeyFailure(status int) bool { return status == 429 || IsKeyRejected(status) }

// IsKeyRejected reports a status that says the credential is dead for this request: unauthorised,
// unpaid or forbidden. A rate limit is not — that key may be back once the others are spent.
func IsKeyRejected(status int) bool { return status == 401 || status == 402 || status == 403 }

// KeyStatusClass is the KeyStats bucket an attempt's status lands in, by the field's JSON name.
func KeyStatusClass(status int) string {
	switch {
	case status == 429:
		return "rate_limited"
	case IsKeyRejected(status):
		return "rejected"
	case status >= 200 && status < 500:
		return "ok"
	default:
		return "failed"
	}
}

// SpeaksDialect reports whether this upstream answers requests of this dialect.
func (r Route) SpeaksDialect(dialect string) bool {
	if r.Dialect == "" {
		return !r.IsProvider()
	}
	return r.Dialect == dialect
}

// IsIngress reports whether this row hands off to an ingress rather than naming an engine.
// Empty Kind is direct, which is what every route pushed before the split was.
func (r Route) IsIngress() bool { return r.Kind == "ingress" }

// IsProvider reports whether this row dials a third-party vendor rather than anything we run. The
// hop then leaves our network, which is what makes its credential and its certificate different in
// kind from an engine's.
func (r Route) IsProvider() bool { return r.Kind == "provider" }

// HasRoom reports whether this engine can take another request. A placement with no
// --max-num-seqs set has no number to hold it to, so it is never held back.
func (r Route) HasRoom() bool { return r.Capacity <= 0 || r.InFlight < r.Capacity }

// nearest narrows to routes in this gateway's region when there are any, so stickiness and
// least-in-flight run inside the winning tier. Two tiers only — ranking remote regions is precision
// nobody can feel. Blank Region reads as remote; nothing local falls through, since far beats 503.
func nearest(routes []Route, region string) []Route {
	if region == "" {
		return routes
	}
	var local []Route
	for _, r := range routes {
		if r.Region == region {
			local = append(local, r)
		}
	}
	if len(local) == 0 {
		return routes
	}
	return local
}

// PickRoute reuses the sticky route when healthy and with room, else fewest in-flight. 200 admits,
// 503 means nowhere healthy, 429 means every replica is at capacity — only 429 says retry. A tie
// keeps the first; the caller's claim breaks it next time.
func PickRoute(routes []Route, stickyURL, region string) (Route, int) {
	var healthy []Route
	for _, r := range routes {
		// An empty engine URL is unroutable, whatever the pusher claimed: the proxy would have no
		// target and the caller would see a 500 instead of the 503 that a model with nowhere to go
		// actually means.
		if r.Healthy && r.EngineURL != "" {
			healthy = append(healthy, r)
		}
	}
	if len(healthy) == 0 {
		return Route{}, 503
	}
	healthy = nearest(healthy, region)

	var free []Route
	for _, r := range healthy {
		if r.HasRoom() {
			free = append(free, r)
		}
	}
	if len(free) == 0 {
		return Route{}, 429
	}

	// Stickiness loses to capacity: a warm prefix cache is not worth queueing behind a full
	// engine when a replica is idle.
	if stickyURL != "" {
		for _, r := range free {
			if r.EngineURL == stickyURL {
				return r, 200
			}
		}
	}
	best := free[0]
	for _, r := range free[1:] {
		if r.InFlight < best.InFlight {
			best = r
		}
	}
	return best, 200
}
