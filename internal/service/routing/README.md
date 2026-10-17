# Routing

A model is one kind: every row ours (`direct`/`ingress`) or every row a vendor's (`provider`). A
push that mixes them is a 400. `routing.Pick` runs the shared admission filters (surface, inputs,
denied tools), then hands the table to one of two routers. The **engine router** (`engine.go`) runs
all five steps below, pins the session and claims an in-flight slot. The **vendor router**
(`vendor.go`) runs the same rule, but only steps 1 and 2 can change its answer — a vendor row is
never counted, ejected or pinned, so capacity, stickiness and in-flight see nothing — then takes
the next credential round the row's key ring.

`domain.PickRoute` decides, in this order. Every step narrows the set the next one sees.

1. **Healthy and routable.** A row must be `healthy` (pushed by the control plane, then possibly
   flipped off by passive ejection) and have a non-empty `engine_url`. None left → **503**.
2. **Region tier.** If the gateway has a region and any row shares it, everything else is dropped.
   Two tiers only, no ranking within them: a cross-region hop costs so much more than the gap
   between two remote regions that ordering the far ones is precision nobody can feel. A row with
   no region counts as remote — the safe reading of "unknown".
3. **Capacity.** A row is out if `in_flight >= capacity` (the engine's `--max-num-seqs`). Capacity 0
   means uncapped. None left → wait up to `capacity_wait` (default `0s`) for a slot, re-reading the
   counts every 50ms, then **429** — deliberately not 503: the model is up, come back shortly.
   Counts are per `engine_url` in Redis and shared by every gateway.
4. **Stickiness.** If the caller's session is pinned to a row still in the set, that row wins.
   Stickiness loses to capacity by construction — a warm prefix cache is not worth queueing behind
   a full engine when a replica is idle.
5. **Least in flight.** Otherwise the row with the fewest live requests. A tie keeps the first.

## Session affinity

A caller names its session with the `X-Grove-Session` header or the body's `user` field; the header
wins. The pin lives at `sticky:<session>` for 30 minutes and holds a caller to one engine so its
prefix cache stays warm.

A caller that names nothing is **balanced** by default. Setting `synthetic_session_ttl` synthesises
one from `sha256(meter_id|model)`, which pins a whole API key to one engine — what a
single-placement fleet always did, and the lever to pull if balancing goes wrong.

Behind an ingress the same rule runs one tier down, keyed on `sha256(session)`. The gateway's pin
selects a *network*; the ingress's selects a *box*, which is the one that actually keeps a cache
warm. The hash is the boundary: the gateway never learns which box, the ingress never learns which
tenant.

## Passive ejection

No prober. Every request reports how its hop went, and three consecutive failures take a target out
for 60 seconds. A success clears the count outright, so it is three **in a row**, not three ever.

What counts as a failure is the part that matters: a connection error, a 502 or a 504 mean the hop
is broken. A 503 carrying `X-Grove-Reason: no-replica` means an ingress answered perfectly well and
one model has nowhere to go behind it — counting that would let a single unplaced model pull an
ingress out of rotation for every other model on it.

A client that left before the upstream answered is not a failure either. The hop has no status,
which would otherwise read as a dead connection, and three impatient clients in a row would take
a healthy model away from everyone for a minute. It moves the count in neither direction.

An upstream that answered and then did not finish is a failure, whatever status it had sent: its
body broke off (`cut=upstream`), or it went silent for the read timeout (`cut=upstream_idle`). A
client leaving in the middle of a good answer is not, and clears the count like any served request.
One that ends a stream early but cleanly cannot be told from one that finished, and counts as served.

This is cheaper than active probing and strictly better informed: a probe tests a path no customer
is on.

Engine and ingress rows only. A vendor row is never counted or ejected: it has no sibling to steer
to, and its counter would be its base URL, shared by every model on that front. Its 5xx reaches the
caller, and the caller's fallbacks, as it is.

## Fallback models

A caller may name other models to take the request when the one asked for cannot, in a JSON body:

```json
{"model": "qwen/qwen3-4b", "messages": [], "fallbacks": ["anthropic/claude-sonnet-5-5"]}
```

At most 3 names; anything else in `fallbacks` is a 400. The field never reaches an upstream. The
gateway never picks a fallback of its own: the caller named the model, and pays its price.

The list is not judged when the request arrives: a fallback that cannot serve is found at its
turn and passed over. One with no route on this surface and path — nothing translates between the
OpenAI and Anthropic shapes, and a model answers only the paths its outputs allow — is skipped
without a dial, as is one the caller is not granted or one with no routes right now. So is one
that declares its inputs and lacks what the request carries: a model that takes `["text"]` is not
sent a request with an image in it. The image and file parts of `messages` are what is
looked for, on either shape; the model asked for is never held to this, only its stand-ins. So is
one whose vendor row denies a tool the request names (`denied_tools`, see [The records themselves](../../repository/redis/README.md#the-records-themselves)); there the model asked for is held to it
too, as a 400, since a vendor-run tool is a bill and not a round trip. One
that is dialled and refuses the request (a model that declares no inputs sent an image, or any
model sent a field it does not take) costs that dial, and the next is tried.

A fallback is tried, in list order, when:

- `route` finds nowhere for the model to go — no healthy server (503) or every one full (429) —
  before anything is dialled; or
- the upstream answers 5xx, or with a key failure (429, 401/402/403) after `retry` has walked every
  key the model has; or
- the model serving is itself a fallback and refuses the request, with any status from 400 up. A
  stand-in that will not take the request as written costs one dial, not the request: models on one
  shape still differ in what they accept.

A 4xx from the model the client asked for is the request's fault and is relayed at once, as is the
answer of a client that left.

Every attempt starts from the client's own body and is rewritten for the vendor it goes to (see
[The client interface](../vendor/README.md#the-client-interface)): a request sent with `max_tokens` reaches DeepSeek
under that name and OpenAI as `max_completion_tokens`.

A response that has begun is never moved: the decision is made on the status line, and once a 200
has gone out the client holds part of one model's answer. A stream that breaks after that ends in
the error event of [Streaming](../../transport/http/proxy/README.md#streaming), on the model that began it, billed for what it
reported. The same holds for a vendor that answers 200 and puts its error in the stream's first
event: the gateway does not read ahead, so that is relayed too.

Each fallback must be granted to the caller like any model; one that is not is skipped. It is
picked with no session, so the caller's pin stays on the model they asked for. When no model is left, the last
answer dialled is the client's, headers and body.

The request is billed once, on the model that served: usage lands under that model and its pricing,
and the access and payload lines carry it as `fallback`. The client reads it off the response:
`X-Grove-Fallback: <model>` is set on any answer that is not the first model's — an audio body has
nowhere else to say so — and the body's `model` names it too. The model that failed keeps its
health mark and gives its slot back, and leaves an `attempt` line on the access log (see
[Correlation](../../transport/http/middleware/README.md#correlation)).

## Before changing something

- **Least-in-flight balances concurrent traffic, not sequential.** A sequential caller releases its
  slot before the next pick, so both replicas read zero and the tie takes the first. Fine for one
  chatty client — it keeps a prefix cache warm — but a fleet of many sequential clients pins them
  all to one engine.
