package domain

import "strings"

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
	// provider row: a vendor is no placement of ours.
	Deployment string `json:"deployment"`
	Server     string `json:"server"` // inference-server / pod id — which box it is on; empty on a provider row
	// Vendor is the third party a provider row dials ("openai"), as the control plane names it.
	// Blank on anything we run.
	Vendor string `json:"vendor"`
	// "ingress" when this row is an Ingress Server that will pick a replica of its own, "direct"
	// (or empty, on a route pushed before this field existed) when it is an engine to dial.
	Kind string `json:"kind"`
	// InputModalities and OutputModalities are what the model takes ("text", "image", …) and what
	// it gives ("text", "embeddings", …), stamped on every row of the model because deploy:<model>
	// is the only thing pushed per model. The outputs say which surfaces it answers on; the inputs
	// say whether a fallback can take what the request carries, and are not read for the model
	// asked for. Empty on a row pushed before the control plane declared them, which reads as
	// unrestricted.
	InputModalities  []string `json:"input_modalities,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
	// DeniedTools is what this vendor would run on its own side and bill outside the token
	// counts: a `tools[].type` ("web_search_20250305") or a top-level request field
	// ("web_search_options"), as the control plane lists them. A request that names one is
	// refused before the dial (SentTools, Refuses). Empty on every row we run ourselves.
	DeniedTools []string `json:"denied_tools,omitempty"`
	// SkipQuirks names the rewrites of the vendor's type this model is not given, as the control
	// plane lists them (vendor.QuirkNames has every name, in snake case, and each vendor's own
	// Quirks the ones it has; a push naming another is refused): the type says what the vendor
	// does, and this says which of its models
	// does otherwise — one that does not reason keeps its sampling. Empty gives every rewrite.
	SkipQuirks []string `json:"skip_quirks,omitempty"`
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

// KeyStats is what one credential has answered since the control plane last took it, as the store
// counts it. OK is every
// answer that was not the key's fault nor the upstream's — a 2xx, or a 4xx the request earned;
// Failed is a 5xx or a hop that produced no status. Timestamps are unix seconds.
type KeyStats struct {
	Requests     int64 `json:"requests"`
	OK           int64 `json:"ok"`
	RateLimited  int64 `json:"rate_limited"`
	Unauthorized int64 `json:"unauthorized"`
	Unpaid       int64 `json:"unpaid"`
	Forbidden    int64 `json:"forbidden"`
	// Rejected is the three above together: the one count a reader had before they were split.
	Rejected        int64 `json:"rejected"`
	Failed          int64 `json:"failed"`
	LastUsed        int64 `json:"last_used"`
	LastRateLimited int64 `json:"last_rate_limited"`
}

// IsKeyRejected reports a status that says the credential is dead for this request: unauthorised,
// unpaid or forbidden. A rate limit is not — that key may be back once the others are spent. The
// status is the one an attempt is counted under (CountedStatus), which is where a class that hid
// under another status has already been put right.
func IsKeyRejected(status int) bool { _, ok := RefusedCredential(status); return ok }

// RefusedCredential names the KeyStats bucket a 401, 402 or 403 counts under — the gateway's own
// credential was turned away. A client is never told it.
func RefusedCredential(status int) (string, bool) {
	switch status {
	case 401:
		return "unauthorized", true
	case 402:
		return "unpaid", true
	case 403:
		return "forbidden", true
	}
	return "", false
}

// What an upstream's answer says went wrong, one name per cause whichever vendor said it, so a
// log reads the same word for the same thing. Read from the body first and the status second:
// vendors disagree on which status carries which fact — Gemini rejects a key with a 400,
// Anthropic reports an empty balance as a 400, OpenAI as a 429.
const (
	// ClassCredential: the key itself was rejected. Nothing on it will work until it is changed. A
	// bare 403 is one too, as it always has been here.
	ClassCredential = "credential"
	// ClassQuota: the account behind the key is out of credit or over a spend cap. Unlike a rate
	// limit, waiting does not help.
	ClassQuota = "quota"
	// ClassModelAccess: this key cannot reach the model — no access, no such deployment. Another
	// key may.
	ClassModelAccess = "model_access"
	// ClassModelGone: the vendor retired the model. Every key gets the same answer, so the ring
	// is not walked for it; a fallback model is.
	ClassModelGone = "model_gone"
	// ClassRegionBlocked: the vendor refuses the request's location. The block can be bound to the
	// key's project or account rather than to our address, so the next key is tried.
	ClassRegionBlocked = "region_blocked"
	// ClassRateLimit: the key is limited right now, and may not be shortly.
	ClassRateLimit = "rate_limit"
	// ClassTransient: the upstream broke in a way unrelated to the key or the request — a 5xx, a
	// connection refused or reset, a body that broke off.
	ClassTransient = "transient"
	// ClassCallerFault: the request itself was refused. No key would have served it.
	ClassCallerFault = "caller_fault"
	// ClassUnknown: a failure carrying nothing recognised — an HTML 404 from a proxy in the way.
	ClassUnknown = "unknown"
	// ClassTimeout: the upstream sent nothing for upstream_read_timeout, before its headers or
	// after them. The gateway cut the hop.
	ClassTimeout = "timeout"
	// ClassCancelled: the client left while the hop was in flight. It says nothing of the upstream.
	ClassCancelled = "cancelled"
)

// The phrases each class is read by, lowered. Substrings of the body rather than a parse: a code
// or a type is a substring of the JSON that carries it, and a vendor that sends only a message is
// read the same way. Each list is kept to phrases that cannot sit in an echoed prompt by accident —
// one that could would rotate or retire a key for the wrong reason.
var (
	quotaMarkers = []string{"insufficient_quota", "blocked_api_access", "credit balance", "spend limit",
		"api usage limits", "prepayment credits", "credits are depleted", "spending cap",
		"requires billing to be enabled", "key limit exceeded", "account is not active",
		"member_budget_exceeded"}
	// throttl: throttled, throttling.
	rateLimitMarkers = []string{"rate limit", "rate_limit", "ratelimit", "too many requests", "quota exceeded",
		"quota_exceeded", "throttl", "rate exceeded", "rpm exceeded", "tpm exceeded", "tokens per minute"}
	regionMarkers = []string{"unsupported_country_region_territory", "unsupported countries",
		"location is not supported", "not available in your country"}
	credentialMarkers = []string{"invalid_api_key", "authentication_error", "unauthenticated", "organization_restricted",
		"api key not valid", "api_key_invalid", "api key expired", "invalid api key", "invalid x-api-key", "incorrect api key"}
	modelMarkers = []string{"model_not_found", "deploymentnotfound", "unknown_model", "invalid_model",
		"model_terms_required", "model_permission_blocked", "no endpoints found", "no allowed providers"}
	retiredMarkers = []string{"model_decommissioned", "deprecated", "deprecation", "decommissioned", "retired",
		"end of its life", "end-of-life", "no longer available", "has been removed", "was removed"}
)

// FailureClass names what an upstream's answer says went wrong, blank on one that was no failure.
// body is the vendor's error as it was sent, nil when only the status is known; a status alone
// decides only what it can carry on its own (401, 402, 403, 429, the 5xx). The rules, in their
// order: quota before rate limit, since an exhausted account is reported under a 429 whose words
// also say "rate limit"; a 5xx stays transient whatever its body says, unless the body says the
// limit was hit; a 401 or 403 is the key's whatever its body says.
func FailureClass(status int, body []byte) string {
	if status < 400 {
		return ""
	}
	lower := strings.ToLower(string(body))
	transient := status >= 500
	switch {
	case !transient && (status == 402 || containsAny(lower, quotaMarkers)):
		return ClassQuota
	case status == 429 || (status != 401 && status != 403 && containsAny(lower, rateLimitMarkers)):
		return ClassRateLimit
	case transient:
		return ClassTransient
	case containsAny(lower, regionMarkers):
		return ClassRegionBlocked
	case status == 401 || containsAny(lower, credentialMarkers):
		return ClassCredential
	}
	// The model is unreachable with this key, or withdrawn altogether: the two share their signals
	// and differ in what is said of the model. A 410 is a withdrawn model whoever sent it.
	retired := containsAny(lower, retiredMarkers)
	model := strings.Contains(lower, "model")
	isModel := status == 410 || containsAny(lower, modelMarkers) ||
		(model && (status == 404 || retired || containsAny(lower, []string{"not_found_error", "permission_error"})))
	switch {
	case isModel && retired, status == 410:
		return ClassModelGone
	case isModel:
		return ClassModelAccess
	case status == 403:
		return ClassCredential
	case len(body) > 0 && (status == 400 || status == 409 || status == 413 || status == 415 || status == 422):
		return ClassCallerFault
	}
	return ClassUnknown
}

func containsAny(s string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// Retires reports a class that leaves the key dead for the rest of the request: waiting changes
// nothing for it, and another key may not say the same.
func Retires(class string) bool {
	switch class {
	case ClassCredential, ClassQuota, ClassModelAccess, ClassRegionBlocked:
		return true
	}
	return false
}

// Rotates reports a class that is the key's fault rather than the request's or the upstream's, so
// another key gets a go: one that retires it, or a rate limit, whose key may be back once the
// others are spent. Only these move a request to another key — a 502 on one key is a 502 on the next.
func Rotates(class string) bool { return class == ClassRateLimit || Retires(class) }

// Exhausts reports a class that holds for every model on the key — the key or the account behind
// it. When every key says it, the gateway answers 502 in the vendor's place: nothing the caller
// did, and nothing they should be told the shape of. A model or a region refusal is the request's
// to see, and is relayed as the last key got it.
func Exhausts(class string) bool { return class == ClassCredential || class == ClassQuota }

// UpstreamFault reports a class that was the upstream's doing — not the caller's request, not the
// client leaving — which is what the error log keeps the upstream's words for.
func UpstreamFault(class string) bool {
	return class != "" && class != ClassCallerFault && class != ClassCancelled
}

// MovesModel reports a class that says this model cannot serve the request here, whoever asks:
// its upstream broke or ran out of time, retired the model, or refused the credential. Only these
// move a request to a fallback model — the request's own fault would be refused there too.
func MovesModel(class string) bool {
	return Rotates(class) || class == ClassModelGone || class == ClassTransient || class == ClassTimeout
}

// MaxFallbacks is how many other models one request may name to fall back on: each can cost a dial.
const MaxFallbacks = 3

// CountedStatus is the status an attempt lands on its key under (KeyStatusClass): the one its
// class means when the vendor hid it under another — an exhausted account under a 429 or a 400
// counts unpaid, a key that cannot reach the model forbidden — else the one sent.
func CountedStatus(class string, status int) int {
	switch class {
	case ClassQuota:
		return 402
	case ClassCredential:
		if status != 403 {
			return 401
		}
	case ClassModelAccess, ClassRegionBlocked:
		return 403
	case ClassRateLimit:
		return 429
	}
	return status
}

// KeyStatusClass is the KeyStats bucket an attempt's status lands in, by the field's JSON name.
func KeyStatusClass(status int) string {
	if class, ok := RefusedCredential(status); ok {
		return class
	}
	switch {
	case status == 429:
		return "rate_limited"
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

// MixedKinds reports whether a table holds both a vendor's rows and ours. A model is one kind: its
// whole table goes to the vendor router or the engine router, and a mix has no router to go to.
func MixedKinds(table []Route) bool {
	var vendor, ours bool
	for _, r := range table {
		if r.IsProvider() {
			vendor = true
		} else {
			ours = true
		}
	}
	return vendor && ours
}

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
