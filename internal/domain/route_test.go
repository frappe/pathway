package domain

import "testing"

func TestPickRouteFewestInFlight(t *testing.T) {
	routes := []Route{
		{EngineURL: "a", Healthy: true, InFlight: 9},
		{EngineURL: "b", Healthy: true, InFlight: 2},
		{EngineURL: "c", Healthy: true, InFlight: 5},
	}
	r, status := PickRoute(routes, "", "")
	if status != 200 || r.EngineURL != "b" {
		t.Fatalf("want fewest-in-flight b, got %q status=%d", r.EngineURL, status)
	}
}

func TestPickRouteStickyHonored(t *testing.T) {
	routes := []Route{
		{EngineURL: "a", Healthy: true, InFlight: 9},
		{EngineURL: "b", Healthy: true, InFlight: 2},
	}
	// Sticky to the busier "a" — stickiness wins over load.
	r, status := PickRoute(routes, "a", "")
	if status != 200 || r.EngineURL != "a" {
		t.Fatalf("want sticky a, got %q status=%d", r.EngineURL, status)
	}
}

func TestPickRouteStickyUnhealthyFailsOver(t *testing.T) {
	routes := []Route{
		{EngineURL: "a", Healthy: false, InFlight: 1}, // pinned server gone
		{EngineURL: "b", Healthy: true, InFlight: 5},
	}
	// Sticky target unhealthy → re-pick among remaining healthy (same model).
	r, status := PickRoute(routes, "a", "")
	if status != 200 || r.EngineURL != "b" {
		t.Fatalf("want failover to b, got %q status=%d", r.EngineURL, status)
	}
}

func TestPickRouteNoneHealthy(t *testing.T) {
	routes := []Route{
		{EngineURL: "a", Healthy: false},
		{EngineURL: "b", Healthy: false},
	}
	if _, status := PickRoute(routes, "", ""); status != 503 {
		t.Fatalf("status = %d, want 503 when no route is healthy", status)
	}
}

func TestPickRouteSkipsEmptyEngineURL(t *testing.T) {
	routes := []Route{
		{EngineURL: "", Healthy: true, InFlight: 1}, // pushed without a derived engine_url
		{EngineURL: "b", Healthy: true, InFlight: 9},
	}
	if r, status := PickRoute(routes, "", ""); status != 200 || r.EngineURL != "b" {
		t.Fatalf("want b despite higher load, got %q status=%d", r.EngineURL, status)
	}
	if _, status := PickRoute(routes[:1], "", ""); status != 503 {
		t.Fatalf("status = %d, want 503 (not 500) when the only route has no engine URL", status)
	}
}

func TestPickRouteEmpty(t *testing.T) {
	if _, status := PickRoute(nil, "x", ""); status != 503 {
		t.Fatalf("status = %d, want 503 for empty routes", status)
	}
}

func TestPickRouteCarriesTheDeployment(t *testing.T) {
	// Two deployments of one model on one box: identical Server, so only Deployment tells them
	// apart — which is why the request id and the access line are built from it.
	routes := []Route{
		{EngineURL: "https://box/e/md-00007", Deployment: "MD-00007", Server: "inf-a", Healthy: true},
		{EngineURL: "https://box/e/md-00008", Deployment: "MD-00008", Server: "inf-a", Healthy: true},
	}
	r, status := PickRoute(routes, "", "")
	if status != 200 || r.Deployment != "MD-00007" {
		t.Fatalf("want the first healthy route, got %q status=%d", r.Deployment, status)
	}
	r, status = PickRoute(routes, "https://box/e/md-00008", "")
	if status != 200 || r.Deployment != "MD-00008" {
		t.Fatalf("want the sticky deployment, got %q status=%d", r.Deployment, status)
	}
}

// Two placements of one model is the case this whole mechanism exists for: before in-flight
// counts existed every route carried the same zero, so healthy[0] took every request and the
// second engine only ever saw traffic through an explicit session.
func TestPickRouteTakesTheIdleEngine(t *testing.T) {
	routes := []Route{
		{EngineURL: "https://box/e/md-00007", Deployment: "MD-00007", Healthy: true, InFlight: 3},
		{EngineURL: "https://box/e/md-00008", Deployment: "MD-00008", Healthy: true},
	}
	if r, status := PickRoute(routes, "", ""); status != 200 || r.Deployment != "MD-00008" {
		t.Fatalf("want the idle MD-00008, got %q status=%d", r.Deployment, status)
	}
}

// Two idle engines tie, and the tie is broken by the caller claiming what it was handed rather
// than by picking at random — so a cold pair alternates on its own.
func TestPickRouteAlternatesFromCold(t *testing.T) {
	routes := []Route{
		{EngineURL: "a", Healthy: true},
		{EngineURL: "b", Healthy: true},
	}
	first, status := PickRoute(routes, "", "")
	if status != 200 || first.EngineURL != "a" {
		t.Fatalf("want a on a tie, got %q status=%d", first.EngineURL, status)
	}
	routes[0].InFlight = 1 // what claim() records against the engine just handed out
	if second, status := PickRoute(routes, "", ""); status != 200 || second.EngineURL != "b" {
		t.Fatalf("want b once a is claimed, got %q status=%d", second.EngineURL, status)
	}
}

// --max-num-seqs is what the engine runs at once. Past it vLLM queues internally, where the
// gateway can neither see the wait nor spend it on an idle replica — so admission stops at the
// same number.
func TestPickRouteSpillsToAReplicaAtCapacity(t *testing.T) {
	routes := []Route{
		{EngineURL: "a", Healthy: true, InFlight: 8, Capacity: 8},
		{EngineURL: "b", Healthy: true, InFlight: 7, Capacity: 8},
	}
	if r, status := PickRoute(routes, "", ""); status != 200 || r.EngineURL != "b" {
		t.Fatalf("want the replica with room, got %q status=%d", r.EngineURL, status)
	}
}

func TestPickRouteRefusesWhenEveryReplicaIsFull(t *testing.T) {
	// 429, not 503: the model is up and serving, the caller just has to come back.
	routes := []Route{
		{EngineURL: "a", Healthy: true, InFlight: 8, Capacity: 8},
		{EngineURL: "b", Healthy: true, InFlight: 9, Capacity: 8},
	}
	if _, status := PickRoute(routes, "", ""); status != 429 {
		t.Fatalf("status = %d, want 429", status)
	}
}

func TestAnUnhealthyReplicaIsStillDownNotBusy(t *testing.T) {
	// Full and dead must not read the same: 429 tells a client to retry, 503 says the model has
	// nowhere to go at all.
	routes := []Route{{EngineURL: "a", Healthy: false, InFlight: 8, Capacity: 8}}
	if _, status := PickRoute(routes, "", ""); status != 503 {
		t.Fatalf("status = %d, want 503", status)
	}
}

func TestCapacityBeatsStickiness(t *testing.T) {
	// A warm prefix cache is not worth queueing behind a full engine while a replica sits idle.
	routes := []Route{
		{EngineURL: "a", Healthy: true, InFlight: 8, Capacity: 8},
		{EngineURL: "b", Healthy: true, InFlight: 0, Capacity: 8},
	}
	if r, status := PickRoute(routes, "a", ""); status != 200 || r.EngineURL != "b" {
		t.Fatalf("want the idle b, got %q status=%d", r.EngineURL, status)
	}
}

func TestNoCapacitySetMeansNoCap(t *testing.T) {
	// Blank --max-num-seqs leaves vLLM on its own default, which the control plane does not know
	// — so there is no number to hold the engine to and it must not be treated as zero.
	routes := []Route{{EngineURL: "a", Healthy: true, InFlight: 500}}
	if r, status := PickRoute(routes, "", ""); status != 200 || r.EngineURL != "a" {
		t.Fatalf("want a admitted uncapped, got %q status=%d", r.EngineURL, status)
	}
}

func TestAnUncappedReplicaAbsorbsWhatACappedOneCannot(t *testing.T) {
	routes := []Route{
		{EngineURL: "capped", Healthy: true, InFlight: 8, Capacity: 8},
		{EngineURL: "uncapped", Healthy: true, InFlight: 40},
	}
	if r, status := PickRoute(routes, "", ""); status != 200 || r.EngineURL != "uncapped" {
		t.Fatalf("want the uncapped engine, got %q status=%d", r.EngineURL, status)
	}
}

func TestARouteInOurOwnRegionWins(t *testing.T) {
	// Least-in-flight alone would send this to the idle box on another continent. The
	// cross-region hop costs far more than the queue depth it saves.
	routes := []Route{
		{EngineURL: "far", Healthy: true, Region: "us-east-1", InFlight: 0},
		{EngineURL: "near", Healthy: true, Region: "ap-south-1", InFlight: 5},
	}
	if r, status := PickRoute(routes, "", "ap-south-1"); status != 200 || r.EngineURL != "near" {
		t.Errorf("got %q/%d, want near/200", r.EngineURL, status)
	}
}

func TestAFarRouteBeatsA503(t *testing.T) {
	routes := []Route{{EngineURL: "far", Healthy: true, Region: "us-east-1"}}
	if r, status := PickRoute(routes, "", "ap-south-1"); status != 200 || r.EngineURL != "far" {
		t.Errorf("got %q/%d, want far/200", r.EngineURL, status)
	}
}

func TestAGatewayWithNoRegionTreatsEveryRouteAlike(t *testing.T) {
	routes := []Route{
		{EngineURL: "a", Healthy: true, Region: "us-east-1", InFlight: 3},
		{EngineURL: "b", Healthy: true, Region: "ap-south-1", InFlight: 0},
	}
	if r, status := PickRoute(routes, "", ""); status != 200 || r.EngineURL != "b" {
		t.Errorf("got %q/%d, want the least loaded and 200", r.EngineURL, status)
	}
}

func TestARouteWithNoRegionIsTreatedAsFarAway(t *testing.T) {
	// Every route carried a blank Region before tiering existed, and the safe reading of
	// "unknown" is "not local".
	routes := []Route{
		{EngineURL: "unknown", Healthy: true, InFlight: 0},
		{EngineURL: "near", Healthy: true, Region: "ap-south-1", InFlight: 9},
	}
	if r, status := PickRoute(routes, "", "ap-south-1"); status != 200 || r.EngineURL != "near" {
		t.Errorf("got %q/%d, want near/200", r.EngineURL, status)
	}
}

func TestCapacityStillAppliesInsideTheWinningTier(t *testing.T) {
	// Tiering narrows the field; it does not excuse a full engine. With every local replica at
	// its cap the answer is 429, not a quiet spill to another region.
	routes := []Route{
		{EngineURL: "near", Healthy: true, Region: "ap-south-1", InFlight: 4, Capacity: 4},
		{EngineURL: "far", Healthy: true, Region: "us-east-1"},
	}
	if _, status := PickRoute(routes, "", "ap-south-1"); status != 429 {
		t.Errorf("status %d, want 429", status)
	}
}

func TestAnIngressRouteIsToldApartFromAnEngine(t *testing.T) {
	if !(Route{Kind: "ingress"}).IsIngress() {
		t.Error("an ingress row was not recognised as one")
	}
	// Empty is what every route pushed before the split carries, and those are all engines.
	for _, kind := range []string{"", "direct"} {
		if (Route{Kind: kind}).IsIngress() {
			t.Errorf("kind %q was treated as an ingress", kind)
		}
	}
}

func TestTheIngressNeverSeesTheCallersSession(t *testing.T) {
	// A caller-chosen session can name the tenant ("acme-support-bot"). The ingress keys its
	// stickiness on the hash, so the string itself never leaves the tenant plane.
	const session = "acme-support-bot"
	if key := SHA256Hex(session); key == session || len(key) != 64 {
		t.Errorf("session key %q is not an opaque hash of %q", key, session)
	}
}

func TestKeyringIsThePushedKeysOrTheOneInternalKey(t *testing.T) {
	engine := Route{InternalKey: "engine-key"}
	if ring := engine.Keyring(); len(ring) != 1 || ring[0].Secret != "engine-key" || ring[0].ID != "" {
		t.Errorf("engine keyring = %+v, want its one unnamed key", ring)
	}
	vendor := Route{Credentials: []Credential{{ID: "k1", Secret: "a"}, {ID: "k2", Secret: "b"}}}
	if ring := vendor.Keyring(); len(ring) != 2 || ring[1].ID != "k2" {
		t.Errorf("vendor keyring = %+v", ring)
	}
	// A row pushed before the field, or an engine with no key at all: one blank credential, so a
	// request still has something to dial with.
	if ring := (Route{}).Keyring(); len(ring) != 1 {
		t.Errorf("bare keyring = %+v", ring)
	}
}

func TestKeyStatusClass(t *testing.T) {
	for status, want := range map[int]string{
		200: "ok", 400: "ok", 404: "ok", 429: "rate_limited",
		401: "unauthorized", 402: "unpaid", 403: "forbidden",
		500: "failed", 502: "failed", 0: "failed",
	} {
		if got := KeyStatusClass(status); got != want {
			t.Errorf("KeyStatusClass(%d) = %q, want %q", status, got, want)
		}
	}
}

// A refusal of the gateway's credential has a class per status; anything else is no refusal.
func TestRefusedCredential(t *testing.T) {
	for status, want := range map[int]string{401: "unauthorized", 402: "unpaid", 403: "forbidden"} {
		if class, ok := RefusedCredential(status); !ok || class != want {
			t.Errorf("RefusedCredential(%d) = %q, %v", status, class, ok)
		}
	}
	for _, status := range []int{200, 400, 404, 429, 500} {
		if _, ok := RefusedCredential(status); ok {
			t.Errorf("RefusedCredential(%d) is a refusal", status)
		}
	}
}

// What an answer says went wrong is read from its body first and its status second: each row is
// a vendor's actual wording. The words alone do not decide: a 5xx that mentions a credit balance
// is still the upstream's failure.
func TestFailureClass(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{200, `{"choices":[]}`, ""},
		{429, `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`, ClassQuota},
		{429, `{"error":{"type":"INSUFFICIENT_QUOTA"}}`, ClassQuota},
		// Morph: an org member past their monthly budget, until the 1st (UTC).
		{429, `{"error":{"code":"member_budget_exceeded"}}`, ClassQuota},
		{400, `{"error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`, ClassQuota},
		{429, `{"error":{"message":"This request would exceed your organization's spend limit"}}`, ClassQuota},
		{402, ``, ClassQuota},
		{429, `{"error":{"message":"Rate limit reached for gpt-4o"}}`, ClassRateLimit},
		{429, ``, ClassRateLimit},
		{503, `{"error":{"message":"rate limit exceeded, retry later"}}`, ClassRateLimit},
		{503, `credit balance service unavailable`, ClassTransient},
		{500, ``, ClassTransient},
		{529, `{"error":{"type":"overloaded_error"}}`, ClassTransient},
		{403, `{"error":{"code":"unsupported_country_region_territory"}}`, ClassRegionBlocked},
		{400, `{"error":{"message":"User location is not supported for the API use."}}`, ClassRegionBlocked},
		{401, ``, ClassCredential},
		{401, `{"error":{"code":"invalid_api_key"}}`, ClassCredential},
		{400, `{"error":{"status":"INVALID_ARGUMENT","message":"API key not valid. Please pass a valid API key."}}`, ClassCredential},
		{403, `{"error":{"message":"permission denied"}}`, ClassCredential},
		{403, ``, ClassCredential},
		{404, `{"error":{"code":"model_not_found","message":"The model gpt-5 does not exist or you do not have access to it."}}`, ClassModelAccess},
		{403, `{"error":{"code":"model_not_found","message":"Project does not have access to model gpt-5"}}`, ClassModelAccess},
		{404, `{"type":"error","error":{"type":"not_found_error","message":"model: claude-9"}}`, ClassModelAccess},
		{403, `{"type":"error","error":{"type":"permission_error","message":"Your workspace does not have access to the model claude-9"}}`, ClassModelAccess},
		{410, ``, ClassModelGone},
		{404, `{"error":{"message":"The model text-davinci-003 has been deprecated"}}`, ClassModelGone},
		{400, `{"error":{"code":"model_decommissioned","message":"The model has been decommissioned"}}`, ClassModelGone},
		{400, `{"error":{"message":"messages[0].content is invalid","type":"invalid_request_error"}}`, ClassCallerFault},
		{400, `{"error":{"message":"max_tokens exceeds the model's maximum"}}`, ClassCallerFault},
		{413, `{"error":{"message":"request too large"}}`, ClassCallerFault},
		{400, ``, ClassUnknown},
		{404, `<html><body>Not Found</body></html>`, ClassUnknown},
	} {
		if got := FailureClass(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("FailureClass(%d, %q) = %q, want %q", tc.status, tc.body, got, tc.want)
		}
	}
}

// What each class does: which retire the key, which rotate to another, which are the gateway's
// 502 once every key says so, which move the request to a fallback model.
func TestWhatEachClassDoes(t *testing.T) {
	for class, want := range map[string]struct{ retires, rotates, exhausts, moves bool }{
		ClassCredential:    {true, true, true, true},
		ClassQuota:         {true, true, true, true},
		ClassModelAccess:   {true, true, false, true},
		ClassRegionBlocked: {true, true, false, true},
		ClassRateLimit:     {false, true, false, true},
		ClassModelGone:     {false, false, false, true},
		ClassTransient:     {false, false, false, true},
		ClassTimeout:       {false, false, false, true},
		ClassCallerFault:   {false, false, false, false},
		ClassUnknown:       {false, false, false, false},
		ClassCancelled:     {false, false, false, false},
		"":                 {false, false, false, false},
	} {
		got := struct{ retires, rotates, exhausts, moves bool }{Retires(class), Rotates(class), Exhausts(class), MovesModel(class)}
		if got != want {
			t.Errorf("%q: retires/rotates/exhausts/moves = %+v, want %+v", class, got, want)
		}
	}
}

// The error log keeps the upstream's words for every failure that was its own, not the caller's.
func TestUpstreamFault(t *testing.T) {
	for class, want := range map[string]bool{
		ClassTransient: true, ClassRateLimit: true, ClassModelAccess: true, ClassModelGone: true, ClassRegionBlocked: true,
		ClassUnknown: true, ClassCredential: true, ClassQuota: true, ClassTimeout: true,
		"": false, ClassCallerFault: false, ClassCancelled: false,
	} {
		if got := UpstreamFault(class); got != want {
			t.Errorf("UpstreamFault(%q) = %v, want %v", class, got, want)
		}
	}
}

// An attempt is counted on its key under the status its class means, not the one the vendor hid
// it under.
func TestCountedStatus(t *testing.T) {
	for _, tc := range []struct {
		class        string
		status, want int
	}{
		{ClassQuota, 429, 402}, {ClassQuota, 400, 402},
		{ClassCredential, 401, 401}, {ClassCredential, 400, 401}, {ClassCredential, 403, 403},
		{ClassModelAccess, 404, 403}, {ClassRegionBlocked, 403, 403},
		{ClassRateLimit, 503, 429},
		{ClassModelGone, 410, 410}, {ClassCallerFault, 400, 400}, {ClassTransient, 502, 502}, {"", 200, 200}, {ClassTimeout, 0, 0},
	} {
		if got := CountedStatus(tc.class, tc.status); got != tc.want {
			t.Errorf("CountedStatus(%q, %d) = %d, want %d", tc.class, tc.status, got, tc.want)
		}
	}
}
