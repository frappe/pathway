# pathway

The Grove data plane. One Go binary that terminates TLS, authenticates callers, picks an engine,
proxies the request, meters what it cost, and upgrades itself without dropping a connection.

**One binary, two planes**, decided entirely by which id it is given:

| | Gateway Server | Ingress Server |
|---|---|---|
| env | `GROVE_GATEWAY_ID` | `GROVE_INGRESS_ID` |
| holds | keys, groups, usage | nothing tenant-shaped |
| picks | a route (network or engine) | a replica in its own VPC |
| meters | yes | no — usage belongs to a tenant it cannot see |

Both ids set is a startup refusal. The tenant stages are not *disabled* on an ingress, they are
never registered, so no handler on that box could read a key store even if one were pushed to it.

## What it does

1. Terminates TLS on `:443` with the fleet wildcard, reloading it from disk when it changes, and
   redirects `:80`.
2. Resolves the caller — bearer → key → user → groups — and refuses on the credential, the credit
   balance, or the model grant.
3. Picks an engine: region tier, capacity gate, session stickiness, least in flight.
4. Rewrites the request body where the endpoint's schema allows it, and swaps the client's key for
   the engine's own.
5. Proxies it, streaming the response through untouched, and reads the usage frame out on the way.
6. Records tokens and the hop's outcome, on every request including the ones that were abandoned.
7. Answers `/v1/models` and `/v1/credits` itself, and `/metrics/node` behind basic auth.
8. Upgrades its own binary and reloads its own configuration without dropping a connection.

Requirements: Redis (loopback, or the Network's shared store), a certificate on disk, and an admin token. It refuses to start
without the last one, and refuses to start as an ingress without a data token.

**Reading order** if you are new to it: *Where it sits* → *Layers* → *The request path*. If you are
about to change something, skip to *How to do things* and *Things worth knowing*.

---

## Where it sits

```
                    ┌──────────────────────────────────────────────┐
                    │  Grove (Frappe control plane)                │
                    │  keys · teams · groups · models · placements │
                    └───────┬──────────────────────────▲───────────┘
   POST /grove-admin/state  │                          │  GET /grove-admin/usage + ack
   every 2 min (hash-gated) │                          │  hourly (drain)
                            ▼                          │
   client ──► latency DNS ──► ┌───────────────────────────────────┐
              api.<zone>      │  GATEWAY SERVER   (tenant plane)  │
                              │  TLS · auth · quota · route · meter│
                              │  Redis: loopback or Network store  │
                              └────┬──────────────────────┬────────┘
                        direct     │                      │  ingress
                                   ▼                      ▼
                    ┌──────────────────────┐   ┌────────────────────────────┐
                    │  engine box          │   │  INGRESS SERVER (infra)    │
                    │  nginx :80           │   │  picks a replica in its VPC│
                    │  └─ vLLM             │   │  holds NO tenant state     │
                    └──────────────────────┘   └────────┬───────────────────┘
                                                        ▼  private IP
                                               ┌──────────────────────┐
                                               │  engine box · vLLM   │
                                               └──────────────────────┘
```

**The control plane pushes; the gateway never calls back.** Grove projects its state into each
box's Redis over `/grove-admin/*` and pulls usage counters back out. Between syncs the
gateway is autonomous — if Grove is down, traffic keeps flowing on the last table it was given.

**Two route kinds.** A `direct` row names an engine the gateway dials itself. An `ingress` row names
an Ingress Server that will pick a replica of its own, so replica topology never leaves its VPC and
a pod restarting in one region is invisible to a gateway in another. Which kind a model gets is the
control plane's decision; the gateway just reads `kind`. An empty `kind` is `direct` — that is what
every route pushed before the split carried.

**A gateway's state lives in one Redis:** loopback, or the Gateway Store its Network's
gateways share (`GROVE_REDIS_ADDR` + `GROVE_REDIS_PASSWORD`). Its contents are either pushed (keys,
groups, routes) or derived (sticky, in-flight, health, usage). On a shared store in-flight is
one counter, so a directly dialled replica's cap holds across those gateways. A dead store fails its
gateways closed: nothing authenticates and `/healthz` reports it. The store has to be a single Redis,
not a Cluster: authentication is one script that follows a key to its user and groups, records it is
not handed by name.

---

## Design

### The shape, and why

The whole service is four rings, and **imports only ever point inward**:

```
        ┌──────────────────────────────────────────────────────────┐
        │  transport/http        net/http · httputil · crypto/tls   │
        │  ┌────────────────────────────────────────────────────┐   │
        │  │  service          admission · routing · metering    │   │
        │  │                   catalog · transform · provisioning│   │
        │  │  ┌──────────────────────────────────────────────┐   │   │
        │  │  │  domain      PickRoute · Evaluate · ParseUsage│   │   │
        │  │  │              stdlib only. No I/O. No net/http.│   │   │
        │  │  └──────────────────────────────────────────────┘   │   │
        │  └───────────────────▲────────────────────────────────┘   │
        └──────────────────────┼─────────────────────────────────────┘
                               │ interfaces declared here
                    ┌──────────┴──────────┐
                    │  repository         │  ← redis/ and memory/ implement it
                    └─────────────────────┘

        cmd/pathway   the only place that knows all four exist
```

`service` depends on the repository **interfaces**, never on an implementation — so the arrow from
`redis/` points *up* into `repository`, not sideways into `service`. That inversion is the only
reason the services are testable.

Five invariants carry the whole thing, and **`cmd/pathway/architecture_test.go` enforces
them** — each is one import statement away from being broken, with nothing failing and nothing
looking wrong:

| Invariant | Protects |
|---|---|
| only `transport/http/**` imports `net/http` | swapping the router stays a one-package rewrite |
| only `repository/redis` imports `go-redis` | swapping the store stays a one-folder change |
| `domain` imports nothing from this module, and does no I/O | the rules stay runnable in another process |
| `service` never imports a repository *implementation* | the services stay testable — tests exempt, that is what `memory` is for |
| `repository` knows domain types and nothing else | imports point inward, including where the compiler would not catch it |

Stated as tests rather than as habits, because the thing they prevent is silent: the service goes on
working while the property that made it testable quietly goes away.

### What each ring is for

**`domain` — the rules, with the I/O taken out.** `PickRoute` gets a slice of routes and returns
one; it does not know where routes come from. `Evaluate` gets three records and returns a status.
`ParseUsage` gets bytes. Every hard decision in this service is one of these, and every one is a
pure function — which is why they carry the most test cases and the fewest mocks. It is also what
lets a *different process* run the identical rule: an ingress does, and a remote router could.

**`repository` — what storage must answer, not how.** The interfaces speak domain types and plain
values; no Redis vocabulary crosses the line. Deliberately coarse: `Usage.Add(prefix, fields)`
takes a map the service built, rather than the service knowing about `HINCRBY`. `InFlight.Counts`
returns numbers, not routes — the repository does not know what a Route is.

**`service` — the orchestration between them.** Fetch, apply a domain rule, act on the result. Each
service is a plain struct holding the repositories it needs, constructed once at startup. They
return `domain.Denial` — one refusal vocabulary — so no service invents its own error taxonomy for
a handler to translate.

**`transport/http` — everything HTTP, and nothing else.** Routing, decoding, status codes, TLS,
the reverse proxy, the signal loop. It is the thickest ring because that is where the framework
lives, and thick is fine as long as no decision hides in it.

### Design decisions, and what was rejected

| Decision | Rejected | Why |
|---|---|---|
| Repository interfaces in one `repository` package | one interface per consumer, Go-style | Nine repositories with one implementation each. Consumer-side interfaces would have meant nine near-duplicate declarations and no reader able to see the storage contract in one place |
| Services are concrete structs | an interface per service | An interface with one implementation is a lie about the design. The extension point is the middleware chain — see below |
| One `middleware.State` per request | a context key per field | The stages are ordered and each reads what the ones above wrote. That *is* a per-request record; N context keys would be the same object with the type safety spread thin |
| The chain is a list of names | a hardcoded handler tree | Order is the semantics here. `meter` below `route` is a correctness requirement, not a style choice, and a list makes it reviewable in one line |
| A separate transform registry | folding body rewrites into middleware | Different lifecycle: a transform is gated per endpoint and answers "did I change anything", so the body is re-encoded only when something touched it |
| `domain.Denial` as the one error type | per-package error types | Every gate speaks 401/403/429/503 already. One `errors.As` at the edge beats a translation table |
| The proxy is its own package | inline in the handler | It owns a transport pool and a response tee — real state with a lifecycle, and the only part that would change wholesale for HTTP/3 |
| Config split by **lifetime** | one config file, or all env | Identity needs a restart; tunables must not. Splitting on that axis makes "does this need a restart?" answerable by looking at where the value lives |

### The three seams

Extending this service means picking one of three, and the choice is not a matter of taste:

| Add a… | When | Costs |
|---|---|---|
| **middleware** | it needs the HTTP request or response — headers, body, timing, or the ability to refuse | one file + one `Register` + a name in the chain |
| **transform** | it rewrites the request body for particular endpoints | one file + one `Register` + a name in the list |
| **repository impl** | it changes *where* data lives, not what happens | one folder + one line in `main.go` |

The test: does it need to be on the request path, or is it a destination for data? Metering already
receives everything a usage row contains, so archiving usage to SQLite is a **repository**, not a
middleware — a middleware there would re-derive the same record from `State` and leave two places
that know how to read it.

### What is deliberately not here

No plugin system, no dependency-injection container, no code generation, no `interface{}` registry
of everything. Extension is a compiled-in file plus a name in a list, which is enough for a service
whose extensions ship in the same binary.

No abstraction over HTTP itself. `transport/http` is allowed to be net/http-shaped; hiding that
behind a "framework-agnostic" layer would buy portability nobody needs at the cost of every reader
having to learn a second vocabulary.

No metrics-per-anything. The access log carries what a request did; the process log carries why.
Warnings and worse are mirrored into `error.log` as well, pinned at Warn so moving the process log
level while hunting a failure never changes what that file holds.
A Prometheus surface is a middleware whenever someone wants one.

### State, and who owns it

The rule everywhere: **one owner per piece of state that can drift.**

| State | Lives | Lifetime | Owner |
|---|---|---|---|
| `middleware.State` | request context | one request | the chain |
| identity, decision, usage line | inside that State | one request | whichever stage wrote it |
| resolved tunables | `config.Live` | until SIGUSR1 | `built.reload` |
| the middleware chain | `atomic.Pointer` in `Server` | until SIGUSR1 | `Server.SetChain` |
| transport pool | `proxy.Proxy` | until a tunable changes | `Proxy.Reconfigure` |
| certificate | `certLoader` | until the file's mtime moves | the loader |
| sticky, in-flight, health, usage | the box's Redis | minutes to a pull cycle | this box (or its store's gateways) |
| keys, groups, routes | the box's Redis | until the next push | **the control plane** |

The last row is the important one. Everything pushed is a *projection*: the gateway never edits it,
never merges into it, and never treats a local change as authoritative. Anything it does own is
either derivable (in-flight, health) or drained (usage).

### Concurrency

One goroutine per request, as net/http gives it. What is shared between them:

- **`config.Live`** — `atomic.Pointer`, swapped whole. A request that started under the old values
  finishes under them, which is correct for every knob here.
- **The middleware chain** — one `atomic.Pointer[http.Handler]`, so a reload never rebuilds the mux
  or interrupts a request mid-chain.
- **`transform.Chain`** — `RWMutex`, read per request and replaced in place, so the middleware's
  pointer to it stays valid across a reload.
- **The transport pool** — `RWMutex` with double-checked insert; replaced wholesale on
  `Reconfigure`, so in-flight requests finish on the transport they started with.
- **The drain flag** — `atomic.Bool`.
- **Redis** — `go-redis` pools connections; every multi-key read is a pipeline or one script, and the
  usage write is a transaction so a drain never sees half a request.

Nothing takes a lock across an I/O call, and no request-scoped value is shared between requests.

### Upstream connections

Kept open and reused. `proxy.Proxy` holds one `http.Transport` per target host and verification
setting, with Go's keep-alive: up to 64 idle connections per host, each closed after 90s unused.
`ForceAttemptHTTP2` is on, so a target that offers h2 — OpenAI, Anthropic and Baseten all do —
carries its concurrent requests as streams on one connection, whichever of the vendor's keys each
one dials with: the key is a header, not a connection. An HTTP/1.1 target takes one connection per
request in flight, out of the same pool.

A dial and a TLS handshake are paid only by the first request to a host after:

- a start or a binary upgrade;
- 90s with no traffic to that host;
- a SIGUSR1 that changed any tunable, because `Reconfigure` drops the whole pool.

Each of those is a full handshake: no TLS session cache is set. Neither number is a tunable; both
are set in `proxy.transportFor`.

An HTTP/2 connection that has delivered nothing for 15s is pinged, and closed if no answer comes
within another 90s (`PingAfter` and `PingTimeout` in `proxy.Options`). A quiet one that answers is
pinged again every 15s; one that does not gets the single ping, which TCP keeps retransmitting, so
a blip shorter than 90s costs nothing. That is what finds a connection that died without a FIN:
under traffic it is never idle, so the 90s idle close never reaches it.

### Why the layering earns its keep here

It is not architecture for its own sake — it bought three specific things:

1. **The decisions became testable without Redis.** `PickRoute` and `Evaluate` were always pure;
   the layering is what stopped them being reachable only through a live store. The services got
   their first tests at all, via `repository/memory`.
2. **The data path became testable without nginx.** When Lua owned the bytes, "does a stream reach
   the client unbuffered" was not a question any test could ask. It is now one `httptest` case.
3. **The seams are where change actually arrives.** Every request since this was built — SQLite for
   usage, offloading routing, a config file — landed on one of the three seams without touching
   `domain` or `service`.

### Where things live

| Question | File |
|---|---|
| Which engine gets this request? | `domain/route.go` — `PickRoute` |
| May this caller use this model? | `domain/access.go` — `CanUse`, `Evaluate` |
| How many tokens did that cost? | `domain/usage.go` — `ParseUsage` |
| Is this target broken? | `domain/health.go` |
| What does Redis look like? | `repository/redis/` — every key name and TTL |
| The request pipeline | `transport/http/middleware/builtin.go` |
| The proxy itself | `transport/http/proxy/` |
| Shutdown, drain, binary upgrade | `transport/http/lifecycle.go` |
| How a reload reaches running state | `cmd/pathway/main.go` — `built.reload` |

---

## The request path

```
client
  │ TLS, HTTP/2
  ▼
recover → accesslog → drain → auth → quota → body → modelaccess → payloadlog → route → meter → fallback → retry → transform → upstreamauth
  │
  ▼
proxy ──► engine (or ingress ──► engine)
```

| Stage | Does |
|---|---|
| `recover` | panic → 500, so nothing below can drop a connection |
| `accesslog` | mints the request id, times the request, writes the one durable line per request |
| `drain` | while shutting down: 503 + `Retry-After` + "gateway is restarting" |
| `auth` | bearer → key → groups, once, into the request state |
| `quota` | the geography pin → 403; the credit flag the control plane pushed, or the key's cap spent → 402; then the key's rate limits → 429 + `Retry-After` |
| `body` | bounded read + JSON decode, or a streaming form parse; `model` and the session hint come out here. Not a JSON object, or no `model` in it (or in the form, or an upgrade's query) → 400; over `max_body_bytes` → 413; not all here within 60s → 408 |
| `modelaccess` | `CanUse` → 403 |
| `payloadlog` | the prompt as the client sent it and the output as it received it, one line per request to `GROVE_PAYLOAD_LOG`. Runs only when that file is set **and** the key's `log_payloads` is on. See [The payload log](#the-payload-log) |
| `route` | surface / sticky / region / capacity / least-in-flight, waiting up to `capacity_wait` for room; claims an in-flight slot, except on a vendor row |
| `meter` | **deferred** release + usage record — runs on disconnect, panic and dead upstream alike |
| `fallback` | runs the stages below again on the next model in the body's `fallbacks` when the one serving cannot (5xx, or a key failure `retry` could not rotate away). See [Fallback models](#fallback-models) |
| `retry` | runs the stages below again with another credential when a vendor refuses the one dialled (429, 401/402/403); counts every attempt against its key |
| `transform` | the registered body rewrites; re-encodes only if one changed something |
| `upstreamauth` | swaps in the engine's internal key, sets the forwarding and ingress headers |

Order is load-bearing in two places. `drain` sits above `auth`, so a restarting gateway answers the
same way whether or not the caller's key is any good. `meter` sits directly below `route`, because
`route` claims a slot and everything below it must give that slot back.

The ingress chain is the same machinery, six entries: `recover`, `accesslog`, `drain`,
`ingressauth`, `pick`, `upstreamauth`.

#### Bodies that are not JSON

`model` decides routing and access, and on the multipart endpoints — `/v1/audio/transcriptions`,
`/v1/audio/translations`, `/v1/images/edits`, `/v1/files` — it arrives as a form field rather than a
JSON key. `body` branches on the content type and reads the form through a tee, stopping at `model`,
then hands the request on as the exact bytes the client sent: same boundary, same part ordering, same
encoding. A form is never re-encoded, and never becomes a `transform.Body`, so the transform stage
skips it rather than trying to serialise a form as JSON.

Two consequences worth knowing:

- **No priority injection.** vLLM priority is written *into* the JSON body, and a form has nowhere to
  put it, so these requests run at the engine's default priority.
- **Memory is bounded by a threshold, not by file size.** A client that sends `model` before its file
  captures almost nothing. One that sends it last has to capture everything it walked past, and that
  goes to memory up to 1 MiB and to a temp file beyond — nginx's `client_body_buffer_size` split,
  for the same reason. The file is created in `os.TempDir()` and **unlinked immediately**, so the
  descriptor is the only handle and the space returns when the request ends, including if the
  process dies first. Set `TMPDIR` on the unit to move it. An unwritable spill directory logs once
  and falls back to memory, which `max_body_bytes` still bounds — an upload is not worth refusing
  over a full disk.
- A request that announces a `Content-Length` over `max_body_bytes` is refused before anything is
  read; a chunked one is caught mid-stream instead, after the route was already picked.
- **Only what `body` reads is on a clock.** Every body `body` reads — a whole JSON body, a form up
  to its `model` — must arrive within 60 seconds of the headers or the request is a **408**: header
  timeouts stop at the headers, and without this a client dripping its body holds a goroutine and up
  to `max_body_bytes` of buffer for as long as it likes. The rest of a form streams through the
  proxy unbounded in time, on purpose: a deadline there would fail inside the hop, where a slow
  client reads as a failed upstream and counts against it.

#### Realtime sessions

A WebSocket upgrade — `/v1/realtime`, a live transcription session — is a `GET` with no body at all,
so `body` takes the model from the **query string**, which is where the OpenAI realtime API puts it
(`?model=…`, and `?user=` for the session hint). It reads and restores nothing: stamping a
`Content-Length` on an upgrade breaks the handshake before it reaches an engine.

Everything else applies unchanged — the key is resolved, the grant is checked, a route is picked,
and `/v1/realtime` is not a path any output claims, so any model may serve it. Two consequences:

- **Usage lands at disconnect, not at connect.** `meter` is deferred, and for a hijacked connection
  the handler does not return until the session ends, so an open session is unbilled for as long as
  it stays open — and it is one `request_count` however long it ran. There are no token counts: once
  the connection is hijacked the usage tee never sees a frame.
- **The in-flight slot is held for the whole session.** The capacity gate was written for requests
  measured in seconds, so a long realtime session makes its engine look busier than it is.

`ModifyResponse` deliberately skips the usage tee on a `101`: ReverseProxy needs that body to stay an
`io.ReadWriteCloser` to write back to the engine, and wrapping it fails the handshake outright.

#### The payload log

The one place the gateway keeps customer content rather than metadata, so it is off twice over: the
box needs `GROVE_PAYLOAD_LOG`, and the key needs `log_payloads` (pushed on `key:<hash>` from its
team, read per request, so turning it takes effect on the next one). One JSON line per request, written when the
request ends — a client that hung up mid-stream still gets one, with what it had received:

`rid` (joins the access line), `key`, `user`, `model`, `fallback` (the model whose output this is,
when not the one asked for; else `-`), `path` (after the `/anthropic` strip),
`status`, `prompt`, `output`, `output_encoding` (only when `base64`), `prompt_bytes`,
`output_bytes`.

It sits between `modelaccess` and `route`: refusals above it (400, 401, 402, 403, 408, 413) leave no
line; everything from routing down does, once per request whatever `fallback` and `retry` did.

What is kept:

- **Text, whole.** `prompt` is the body as the client sent it, before any transform; `output` is
  what the client received, after the model swap, error event included. Never truncated.
  A stream is its raw `data:` frames, not reassembled text.
- **Inline media up to 256 KiB, as sent.** Past it — a `data:` URI anywhere, or base64 under
  `data` / `b64_json` (OpenAI `input_audio`, `audio`, generated images; Anthropic `source`) — the
  item becomes `[media image/png 834512 bytes sha256:9f2c…]`: its type, decoded size and the first
  16 hex of its hash, enough to match a file the customer sends without keeping it.
- **A file response** (`audio/*`, `image/*`, `video/*`, `application/octet-stream` — speech) is
  base64 with `output_encoding: "base64"` up to 256 KiB, else `[media audio/mpeg 2097152 bytes]`;
  the recorder stops holding it at the limit. The client gets every byte either way.
- **An upload** (transcription) is its form fields as a JSON object, each file a stand-in —
  `"file": "[file talk.mp3 audio/mpeg]"` — never the file. Only parts before `model` are seen,
  since the rest streams to the upstream unread; `prompt_bytes` is the whole form's
  `Content-Length`, `-1` when chunked.
- **Realtime sessions are not logged**: once the connection is hijacked nothing passes the
  recorder.
- **Secrets, never.** Every format OpenRouter's Secrets guardrail detects ([the
  list](https://openrouter.ai/docs/guides/features/guardrails/secret-formats) — AWS, GitHub, GitLab,
  OpenAI, Anthropic, OpenRouter, Google, Stripe, Slack, npm, SendGrid, Hugging Face, Databricks,
  Atlassian, Doppler, Linear, Shopify, Telegram, age, JWTs, Bitcoin extended and Ethereum keys, PEM
  private keys, PyPI, DigitalOcean), and Grove's own keys (`gr_…`), becomes `[SECRET:<format>]`
  under OpenRouter's labels (`payloadsecrets.go`). A known prefix followed by at least 16 key
  characters is enough, whatever the length past that — OpenRouter's own rule for most formats, and
  a superset of its match where it pins a length or marker, so a key of an odd size still goes; the
  floor keeps names like `hf_token` intact. Two keep a strict shape because their prefix says too
  little: Ethereum (`0x` + exactly 64 hex) and JWT (three dotted parts). OpenRouter's two Bitcoin
  WIF formats are left out on purpose: they have no prefix, only a length and an alphabet ordinary
  ids share. A `BEGIN … PRIVATE KEY` cut off before its `END` line is redacted to the end of the
  text. It runs on every string in the prompt, output (each stream event on its own) and upload
  fields — but not inside inline media, where a base64 image can hold runs shaped like a key and
  redacting them would corrupt it — and not on a file response. What it misses: a secret the model
  streams across several events, anything without a known prefix (a password), and the shape-alikes
  it catches wrongly (an Ethereum transaction hash looks like a private key, as OpenRouter also
  accepts). A body with no secret and no big media is logged byte-for-byte; one with either is
  re-encoded.

A text response is held in memory until its line is written; a long stream for an opted-in user
costs its size.

### A request, layer by layer

What each layer contributes, for one `POST /v1/chat/completions`:

| | Layer | |
|---|---|---|
| 1 | `transport/http` | TLS handshake, `ServeMux` matches host + path |
| 2 | `middleware/auth` | reads the `Authorization` header |
| 3 | `service/admission` | `Identify` → one store read |
| 4 | `repository/redis` | one script: `HGETALL key:…`, then each `model_group:…` it names |
| 5 | `domain` | `Evaluate` — pure, no I/O |
| 6 | `middleware/body` | bounded read, JSON decode, model out |
| 7 | `service/routing` | `Pick` — sticky read, in-flight counts, health |
| 8 | `domain` | `PickRoute` — pure again; the whole selection rule |
| 9 | `service/routing` | claim the slot |
| 10 | `middleware/transform` | body rewrites, re-encode if changed |
| 11 | `transport/http/proxy` | forward, stream back, scrape the usage frame |
| 12 | `middleware/meter` (deferred) | release the slot, record usage and outcome |

Two things fall out of that. The decisions — steps 5 and 8 — are pure functions over data someone
else fetched, which is why they are the parts with real test coverage. And nothing below step 1
knows it is speaking HTTP.

### Streaming

The response is never buffered. `FlushInterval: -1` pushes each write straight through, and the
usage scraper (`proxy/usagetee.go`) reads the bytes it is already copying and writes nothing back —
so the client sees exactly the stream the engine produced, at the engine's own pace.

One exception, at the end only. When an event stream breaks off — the upstream went silent for
`upstream_read_timeout`, or its body failed — while the client is still there, `proxy/streamend.go`
closes the event in progress and appends one error event in the surface's shape, the one its SDK
raises on, then ends the stream cleanly:

```
data: {"error":{"message":"upstream went silent","type":"api_error"}}                              (OpenAI surface)

event: error
data: {"error":{"message":"upstream went silent","type":"api_error"},"type":"error"}               (/anthropic)
```

`upstream went silent` for the timeout, `upstream broke off the stream` otherwise. Without it the
200 has long gone out and the client sees only a dropped connection. It sits outside the usage
scraper, so usage and the `cut` are read off the upstream's own bytes exactly as before. An event
already cut mid-line still reaches the client broken, ahead of the error. A body that is one JSON
document cannot be repaired this way and is still just dropped.

### The client interface

Clients speak one of two shapes, and a vendor is only ever reached on one of two paths:
`/v1/chat/completions` (OpenAI) and `/anthropic/v1/messages` (Anthropic). Nothing converts between
them. Engines we run take both shapes and their own extras, and are sent what the client sent; what
follows is about a hop to a vendor.

The fields the gateway knows on each shape:

| Shape | Fields |
|---|---|
| OpenAI chat | `model`, `messages`, `max_completion_tokens` (older name `max_tokens`), `temperature`, `top_p`, `n`, `stop`, `stream`, `stream_options`, `presence_penalty`, `frequency_penalty`, `logit_bias`, `logprobs`, `top_logprobs`, `seed`, `user`, `tools`, `tool_choice`, `parallel_tool_calls`, `response_format`, `reasoning_effort`, `metadata`, `store`, `service_tier`, `modalities`, `audio`, `prediction`, `prompt_cache_key`, `safety_identifier`, `verbosity`, `web_search_options` |
| Anthropic messages | `model`, `messages`, `max_tokens`, `system`, `metadata`, `stop_sequences`, `stream`, `temperature`, `top_p`, `top_k`, `tools`, `tool_choice`, `thinking`, `service_tier`, `output_config`, `container`, `mcp_servers`, `context_management` |
| both, the gateway's own | `fallbacks` — read here, never forwarded |

What a vendor is sent in place of what the client sent:

| Field | Vendor gets |
|---|---|
| `model` | its own id for the model (`modelmap`) |
| `max_completion_tokens` / `max_tokens` (OpenAI shape) | `max_completion_tokens` at OpenAI, `max_tokens` at every other vendor; sent under both names, the newer one's value (`vendorfields`) |
| `stream_options` (OpenAI shape) | `include_usage: true` added on a stream (`streamusage`). `continuous_usage_stats` is added for Baseten and for an engine of ours, not for OpenAI (400s on it), DeepSeek (ignores it) or a vendor the gateway does not know |
| a tool's `type` (Anthropic shape) | `"type": "custom"` removed at DeepSeek, which refuses it; a tool with no type means the same. Any other type stays (`vendorfields`) |
| `speed` (Anthropic shape) | removed for every vendor: fast mode is twice the rate and nothing here prices it. The caller is told: `X-Grove-Changed: speed=default` (`vendorfields`) |
| a `developer` message (OpenAI shape) | sent as a `system` message at DeepSeek, which knows no such role (`vendorfields`) |
| a past tool-call turn that came back without its reasoning | at DeepSeek an empty one is added: `reasoning_content: ""` on the OpenAI shape, an empty thinking block on the Anthropic one. Not when the caller disabled `thinking` (`vendorfields`) |
| `thinking`, when a tool is forced | at DeepSeek `{"type": "disabled"}` is added when `tool_choice` is `"required"` or names a function (`tool` on the Anthropic shape) and the caller sent no `thinking` or `reasoning_effort`. The caller is told: `X-Grove-Changed: thinking=disabled` (`vendorfields`) |
| `reasoning_effort`, when the request has tools (OpenAI shape) | at OpenAI `"none"` is added when the caller sent no `reasoning_effort`. The caller is told: `X-Grove-Changed: reasoning_effort=none` (`vendorfields`) |
| `temperature`, `top_p`, `logprobs`, `top_logprobs` (OpenAI shape) | at OpenAI, unless `reasoning_effort` is `"none"`: dropped, but for a `temperature` or `top_p` of 1. The caller is told: `X-Grove-Changed: temperature=default, top_p=default` (`vendorfields`) |
| `service_tier` | dropped (`servicetier`) |
| every other field, listed above or not | as the client sent it |

- **A field not on the list is forwarded**, not dropped and not refused: the list is what the
  gateway stands behind, not a gate, and a vendor's new field works the day it ships. The vendor
  refuses what it does not take.
- **Three rewrites change what the model does**, and the caller is told of each in
  `X-Grove-Changed`. A forced tool switches DeepSeek's thinking off, and tools switch OpenAI's
  reasoning off (`reasoning_effort: "none"`), since each refuses the two together: only when the
  caller said nothing about thinking or effort, and one who asked for both gets the vendor's
  refusal. The third overrides what the caller asked for: while an OpenAI model reasons, its
  `temperature`, `top_p`, `logprobs` and `top_logprobs` are dropped, since OpenAI takes none then.
- **A listed field a vendor cannot take is sent anyway**; dropping it would change what the caller
  asked for. On a fallback, that vendor's refusal moves the request on (see
  [Fallback models](#fallback-models)).
- **Where an upstream differs is one table**, `vendors` in `internal/service/transform/vendors.go`,
  keyed by the vendor's name as the control plane pushes it: the field it reads the output cap from
  (`outputCap`), whether it is asked for usage on every chunk of a stream (`usagePerChunk`), and
  four things DeepSeek refuses: a tool typed `"custom"` (`untypedTools`), a `developer` message
  (`developerAsSystem`), a past tool call with no reasoning (`blankReasoning`) and a forced tool
  while thinking (`unthinkForcedTool`). One is OpenAI's: a function tool while the model reasons
  (`unreasonWithTools`) and sampling of the caller's while it reasons (`defaultSampling`). One more is a header, not a field: an Anthropic front that
  reads the key as a Bearer (`bearerOnAnthropic`, Baseten), read by `upstreamauth`.
  [vendor_quirks.md](vendor_quirks.md) shows each with what the caller sent and what the vendor
  gets. It names OpenAI,
  DeepSeek and Baseten; an engine of ours and a vendor it does not name each have an entry of their
  own. `vendorfields` and `streamusage` read it per attempt on the client's own body, so a request
  that moves to another vendor is named afresh for that one. Add a line when the access log's
  `attempt` line, or a probe, shows a vendor differing — and the row below with it.
- Of an upstream's response headers only a named few reach the client (see [Headers](#headers)).
- Responses are not rewritten: same shape in, same shape out, with `model` swapped back to the name
  the client knows — whatever the vendor wrote there, since one asked by an alias answers under the
  name behind it.

What each upstream was seen to do, through a gateway, and where the gateway acts on it. This is the
one list of it: add a row or a column when a new difference shows up, with the date it was measured.

| | Engines we run (vLLM) | OpenAI | DeepSeek | Baseten | Handled by |
|---|---|---|---|---|---|
| Output cap field (OpenAI shape) | either name | `max_completion_tokens` only; 400 on `max_tokens` | `max_tokens` (2026-10-04: capped at 16) | `max_tokens` (2026-10-05: capped at 16) | `vendorfields`, from `vendors` |
| `stream_options.continuous_usage_stats` | taken | 400 (2026-10-05) | ignored (2026-10-05) | taken (2026-10-05) | `streamusage`, from `vendors`: sent to our engines and Baseten |
| Usage on every chunk of a stream | yes, when asked | no, the last chunk only | no, the last chunk only | yes, asked or not | asked all the same |
| A stream that is cut is metered | yes, by the last whole chunk | no; OpenAI bills nothing for it either (2026-09-30) | no | yes, by the last chunk received (2026-10-05: 44 + 33 tokens billed) | the usage tee |
| `model` in the answer | the id it was started under | the id it was asked by | may differ: `deepseek-v4-flash` answers as `deepseek-flash` (2026-10-04) | not checked | the response swap writes the client's id whatever came back |
| Anthropic shape | yes | no | yes, its own front | yes; it reads the key as a Bearer, and answers 401 `please check the api-key you provided` to `x-api-key` (2026-10-05) | the route's `dialect`; `upstreamauth` sends Baseten a Bearer, from `vendors` |
| A tool typed `"custom"` (Anthropic shape; Anthropic's API reference takes it, and litellm always sends it) | not checked | no such shape | 422 ``unknown variant `custom`, expected `web_search_20250305` or `web_search_20260209` ``; the same tool with no type is a 200 with a `tool_use` (2026-10-05) | taken (2026-10-05) | `vendorfields`, from `vendors`: DeepSeek gets the tool without the type |
| A server tool other than web search (Anthropic shape) | not checked | no such shape | 422 `unknown variant` for bash, text editor, web fetch and code execution (2026-10-05) | taken and not run: the answer is prose (2026-10-05) | the route's `denied_tools`, at `route`: a tool the control plane lists for the vendor is a 400 before the dial; the rest the vendor refuses |
| A forced tool: `tool_choice` `"required"` or a named function, `tool` on the Anthropic shape | not checked | not checked | 400 `Thinking mode does not support this tool_choice` while thinking is on, its default; taken with `thinking` disabled, and `any` on the Anthropic shape is taken (2026-10-05) | taken on both shapes (2026-10-05) | `vendorfields`, from `vendors`: DeepSeek gets `thinking` disabled, unless the caller spoke of thinking |
| A past tool-call turn sent back without its reasoning | not checked | not checked | 400 ``The `reasoning_content` in the thinking mode must be passed back to the API``, and ``The `content[].thinking` …`` on the Anthropic shape; taken with it, with it empty, or with `thinking` disabled (2026-10-05) | taken on both shapes; a thinking block with no `signature` is a 400 (2026-10-05) | `vendorfields`, from `vendors`: DeepSeek gets an empty one |
| A function tool while the model reasons (chat) | not checked | 400 `Function tools with reasoning_effort are not supported … use /v1/responses or set reasoning_effort to 'none'` on luna and sol; taken with `reasoning_effort: "none"` (2026-10-05) | taken (2026-10-05) | taken (2026-10-05) | `vendorfields`, from `vendors`: OpenAI gets `reasoning_effort: "none"` when the caller set none |
| A `temperature` or `top_p` other than 1, or `logprobs` (chat) | not checked | while the model reasons, its default: 400 `'temperature' does not support 0.7 with this model. Only the default (1) value is supported.`, 400 `'top_p' is not supported with this model` and the same for `logprobs`, on luna and sol; 1 is taken for the first two, and all of them with `reasoning_effort: "none"` (2026-10-06) | not checked | not checked | `vendorfields`, from `vendors`: OpenAI gets none of them unless reasoning is off |
| `speed: "fast"` (Anthropic shape) | not checked | no such shape | not checked | not checked | `vendorfields`: dropped for every vendor, the caller told `speed=default`; ours to refuse to pay, not a quirk |
| A `developer` message | not checked | followed (2026-10-05) | 422 ``unknown variant `developer`, expected one of `system`, `user`, `assistant`, `tool`, `latest_reminder` `` (2026-10-05) | followed (2026-10-05) | `vendorfields`, from `vendors`: DeepSeek gets it as `system` |
| A past tool call with no `type` | not checked | 400 `Missing required parameter` (2026-10-05) | 422 ``missing field `type` `` (2026-10-05) | taken (2026-10-05) | not handled: the vendor refuses |
| Fields only OpenAI reads: `prediction`, `prompt_cache_key`, `prompt_cache_retention`, `verbosity`, `store`, `web_search_options` | not checked | its own | all six ignored (2026-10-05) | 400 `Extra inputs are not permitted` on `prediction` and `web_search_options`; the other four ignored (2026-10-05) | `web_search_options` is a 400 before the dial where the route's `denied_tools` lists it (OpenAI's does); the rest not handled |
| A remote image URL | not checked | not checked | downloads it itself. A Pexels photo was a 400 `unsupported image` 9 times in 12: Pexels serves AVIF to an `Accept` that offers it and JPEG otherwise, and DeepSeek takes JPEG, PNG, GIF and WebP only, judged by the file's bytes ([its guide](https://api-docs.deepseek.com/guides/vision)). With `fm=jpg` in the URL, 8 of 8 were taken. A Wikimedia PNG is a 400 `Failed to download image`. The image as base64 is taken. Only flash reads images: pro answers 200 and says it cannot see one (2026-10-05) | the Pexels photo is taken, the Wikimedia PNG a 500; base64 is taken (2026-10-05) | not handled: the gateway downloads nothing |

What Baseten bills for a stream that was cut has not been compared with its own usage report.

---

### Headers

What a client sees on an answer:

| Header | Whose | When |
|---|---|---|
| `X-Request-Id`, `Request-Id` | the gateway's | every answer. One id under both names; the second is what Anthropic's SDK reads |
| `X-Grove-Fallback` | the gateway's | only when a fallback model served: that model's key |
| `X-Grove-Changed` | the gateway's | only when the gateway changed what the model does for the vendor that answered: `reasoning_effort=none`, `thinking=disabled`, or a dropped field as `temperature=default`, comma-separated (see [vendor_quirks.md](vendor_quirks.md)) |
| `Retry-After` | the gateway's on its own 429 and 503, else the upstream's, relayed | when either says when to come back |
| `Content-Type`, `Content-Length`, `Content-Encoding`, `Content-Disposition`, `Cache-Control` | the upstream's, relayed | as it sent them |
| `Upgrade`, `Connection`, `Sec-WebSocket-*` | the upstream's, relayed | an upgrade's handshake |

Nothing else an upstream sends reaches the client. The list is `relayed` in
`internal/transport/http/proxy/proxy.go`; a header joins it by being added there. What stops at the
gateway: a vendor's ids and timestamps, its cookies, `alt-svc`, `via`, `strict-transport-security`,
its own request ids, and the rate limits of the account we call it with (`x-ratelimit-*`), which
are ours and not the caller's. Before the list (2026-10-05) a Baseten answer carried twelve
`x-baseten-*` and four `x-ratelimit-*` headers to the client.

Which model served has no header of its own: it is the body's `model` on every answer, and
`X-Grove-Fallback` when it is not the model that was asked for.

Every `X-Grove-*` header, and who it is between:

| Header | From → to | For |
|---|---|---|
| `X-Grove-Session` | client → gateway | names the caller's session (see [Session affinity](#session-affinity)). Taken off before the hop |
| `X-Grove-Metadata` | client → gateway | the caller's own tags for the access line (see [Correlation](#correlation)). Taken off before the hop |
| `X-Grove-Fallback` | gateway → client | the fallback that served |
| `X-Grove-Changed` | gateway → client | what the gateway changed in the request that alters what the model does, as `field=value` |
| `X-Grove-Model`, `X-Grove-Session-Key` | gateway → ingress | the model to pick a replica of, and the session to keep on it. Taken off a request to anything that is not an ingress |
| `X-Grove-Engine` | ingress → gateway | the replica that served: how usage reaches a placement the gateway never picked. Read here, not relayed to the client |
| `X-Grove-Reason` | ingress → gateway | why the ingress refused; `no-replica` keeps that 503 from counting against it. Read here, not relayed |
| `X-Grove-Admin-Token` | control plane → gateway | gates every admin endpoint |

## Routing

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
   Counts are per `engine_url` in Redis and shared by every gateway, so a vendor's rows on one
   base URL share one cap.
4. **Stickiness.** If the caller's session is pinned to a row still in the set, that row wins.
   Stickiness loses to capacity by construction — a warm prefix cache is not worth queueing behind
   a full engine when a replica is idle.
5. **Least in flight.** Otherwise the row with the fewest live requests. A tie keeps the first.

### Session affinity

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

### Passive ejection

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

### Fallback models

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
one whose vendor row denies a tool the request names (`denied_tools`, see [The records themselves](#the-records-themselves)); there the model asked for is held to it
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
[The client interface](#the-client-interface)): a request sent with `max_tokens` reaches DeepSeek
under that name and OpenAI as `max_completion_tokens`.

A response that has begun is never moved: the decision is made on the status line, and once a 200
has gone out the client holds part of one model's answer. A stream that breaks after that ends in
the error event of [Streaming](#streaming), on the model that began it, billed for what it
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
[Correlation](#correlation)).

---

## Admission

Two records, resolved in order — `key:` → `model_group:`. A key names any number of groups; their
grants are unioned into one before the gates run.

**The key is the policy.** Its team (a Central Team) is the ledger the control plane bills, but
everything a gate reads — models, geography, rate limits, and the cap it spends against — sits on
the key itself. That is what lets a team spread keys across geographies: a key lives on one
geography's store, that store holds the key's whole cap and counts all its spend, so the gate is
exact however many other keys the team holds elsewhere. A revoked key dies alone; the team's
balance running out flips `limited` on every key it holds, one field each.

`Evaluate` then runs three gates, in this order:

| Gate | Status | Why in this position |
|---|---|---|
| key is `active` | 401 | Checked first: a revoked key is 401 even when it is also over quota, because the key is the thing that is wrong |
| key has credit | 402 | `limited` is a pushed flag (the team's balance is gone); `prepaid && spent >= budget` is this Redis's own counter against the key's cap. Either → 402 |
| `CanUse(model)` | 403 | |

`CanUse` is the whole access rule and fails closed:

```
deny wins over everything          key.Deny[model]        → false
otherwise, every group's grant ∪ key's own allow           → true
nothing granted it                                        → false
```

`/v1/models` filters its list with the *same function*, so the catalogue can never advertise
something the inference path would refuse.

### Credit gates

Two, both read off the key record before the body is, both answering 402
`credit balance exhausted`. The control plane's: the team's balance priced from the pull, flipped as
`limited` and pushed onto every key of the team. This box's own: `prepaid && spent >= budget`, the
key's cap against the one counter this process keeps (see *The records themselves*). A client
cannot tell which one refused it.

### Rate limiting

Per key, pushed on its record as `limits`: a comma list of `<metric>:<window>:<value>`,
e.g. `requests:1m:200,total_tokens:1h:50000`. No entry = uncapped. Checked in `quota`, after the
credit gates, so a key with no balance does not use up a request.

| | |
|---|---|
| Metrics | `requests` — counted as the request is admitted. `total_tokens` — prompt + completion as the answer reports them, cache reads included |
| Windows | `1m` `1h` `1d` `1M`. They reset on the UTC clock (top of the minute, the hour, midnight, the 1st), not from the key's first request |
| Counter | `lim:<key prefix>:<metric>:<window>:<bucket>`, kept two windows. Bucket = `floor(unix / seconds)`, or `2026-10` for the month |
| Refusal | 429 `rate limit exceeded: 200 requests per 1m` (`rate_limit_error`), `Retry-After` = seconds to the window's end. Over several limits at once, the one that resets last is named |

**Requests are exact, tokens are check-then-debit.** One Lua script reads every counter and, only
when all have room, counts the request — so a refusal counts nothing and two requests cannot both
take the last slot. Tokens are unknown until the answer: `meter` adds them to the window the answer
ended in. The request that crosses a token limit completes; the overshoot is the requests in flight
times their size. A request with no token usage (audio seconds) moves only the request limits.

What counts: every request `quota` admits, whatever happens below it (a 403 on the model, a full
engine). `fallback` and `retry` sit below, so a fallback model or a vendor key rotation is still
one request. A response cut short
is debited what usage it reported. A debit the store refuses is logged and dropped, never spooled:
replayed later, it would land in a window the tokens were not used in.

Scope: the counters live in this gateway's store, so a limit is exact across every gateway sharing
it and separate on a gateway with its own Redis. The limit store failing is a 503 for a key with
limits; one without never reads it. A push carrying a limit this binary cannot read is refused 400,
naming the key.

---

## Metering

One usage record per request, written in a single transaction so a control-plane drain never sees
half of one.

**Where the numbers come from.** The response body, read one of two ways by its `Content-Type`:

- **An event stream** (`text/event-stream`) is read by the line: the last line containing
  `"usage"`, the final frame of an OpenAI stream. Streaming requests only have one because the
  `streamusage` transform forces `stream_options.include_usage` on the way in. An engine of ours
  and Baseten are also sent `continuous_usage_stats`, so every chunk carries the count so far and
  a stream that is cut is metered by the last chunk that arrived whole; OpenAI's and DeepSeek's
  cut stream has reported nothing by then.
- **Any other body** is one document and is kept whole. It is never split into lines: OpenAI
  prints a body that is not streamed over many lines, and the line that names `"usage"` is then
  `  "usage": {` and holds none of it.

Either way, what is held past 1 MiB is its tail only, which is no longer a document, so
`ParseUsage` falls back to the last `"usage":` object in it — a long body, or one carrying
logprobs, still meters.

The first such line is kept beside the last, because an Anthropic stream splits its counts:
`message_start` carries the prompt and its cache buckets, `message_delta` the output. The two are
merged field by field, the larger winning (`domain.MergeUsage`), and `Total` is raised to
`Prompt + Completion`. A first line with nothing to read, such as OpenAI's `"usage":null` chunks,
is ignored. First and last only: a vendor that splits three ways needs a per-event merge.

The upstream is never asked to compress: `Accept-Encoding` is dropped on the way out, since a
gzip body is one the tee cannot read and the request would meter as zero tokens.

**Two engine shapes, one meaning.** vLLM answers OpenAI-shaped on `/v1/chat/completions` and
Anthropic-shaped on `/v1/messages`, and they disagree about what "input tokens" means:

| | OpenAI | Anthropic |
|---|---|---|
| prompt | `prompt_tokens` — **includes** cache | `input_tokens` — **excludes** it |
| cached | `prompt_tokens_details.cached_tokens` | `cache_read_input_tokens` |
| total | `total_tokens` | absent |
| written | `prompt_tokens_details.cache_write_tokens` — **inside** the prompt | `cache_creation_input_tokens`, the hour part under `cache_creation.ephemeral_1h_input_tokens` |

Both are normalised to: `Prompt` = the full input processed, `Total` = `Prompt + Completion`,
`Cached ⊆ Prompt`. Cache **creation** is a write, so it lands in `Prompt` but not in `Cached`.
Billable is then `Total - Cached` on either shape.

`cached_tokens` is only non-zero when the engine was started with
`--enable-prompt-tokens-details`. Without it every request bills as uncached.

**Each metric is written three times** into the same hash — flat, `m:<metric>:<model>`, and
`m:<metric>:<deployment>` — so one drain carries the aggregate and both breakdowns. Zero values are
skipped entirely; a field that never moved should not appear.

**What gets priced.** Beside the display fields, the counters the control plane's table names.
The table rides inside every pricing (`pricing.counters`, `domain.CounterTable`): a **root** row is a
bucket the response fills — `prompt_tokens` (the whole prompt), `cached_tokens`, `cache_write_tokens`
(the five-minute writes on the Anthropic shape, every write on the OpenAI one),
`cache_write_1h_tokens`, `audio_tokens`, `completion_tokens` (the whole completion),
`completion_audio_tokens`, `request_count` — and a **derived** row is a root plus a prompt-size
threshold (`base`, `min_prompt_tokens`), today the four `*_above_272k` counters. The bucket → root
mapping is code (`metering.UsageFields`); everything else about a counter is the pushed row. A
pricing pushed without a table, or no pricing at all, is an empty table: every bucket is counted
under its root and nothing is priced — the control plane always pushes one, so a pricing without
it is logged once as an error.

Counting records what the vendor reports; charging subtracts (`CounterTable.Cost`). A counter's rate
is charged on what its parts left of it, and each part at its own rate, so a token bills once. A root
row's `part_of` names its container (`cached_tokens`, `cache_write_tokens`, `cache_write_1h_tokens`
and `audio_tokens` are parts of `prompt_tokens`; `completion_audio_tokens` of `completion_tokens`); a
derived row's parts are its base's parts at the same threshold.

`audio_tokens` is the audio part of the prompt — `prompt_tokens_details.audio_tokens`
on a chat, `input_token_details.audio_tokens` on a token-shaped transcription — capped at what the
cache left. `completion_audio_tokens` is the audio part of the completion,
`completion_tokens_details.audio_tokens`, capped at the completion. `audio_seconds` is a display
field, not priced: a transcription's `{"type":"duration","seconds":N}` usage, or the top-level
`duration` of a `verbose_json` body, which carries no usage at all. A duration-shaped
transcription, a translation and a realtime session therefore bill `request_count` only. Cache buckets
that exceed the prompt cannot be credited: the whole prompt bills as plain, logged once per model.

**Above a threshold.** A vendor may charge the whole request more once the prompt passes some size
(OpenAI: 272 000 tokens). Two rules, one on each side, and neither knows the other:

- **Counting** (`metering.landed`). For a request whose prompt — plain, cached, written and audio
  together — is strictly past a derived row's threshold, each root bucket lands under its derived
  row at the highest such threshold, **instead of** the root, flat, per model and per pricing alike.
  The pricing's rates are not consulted, only its table. A root with no derived row at that
  threshold stays where it is: a container's derived row takes what is left after the parts that
  stayed. Hour writes and audio have one rate at any prompt size and no derived row, so a long
  request's stay with the base counters: its hour writes and prompt audio are counted in
  `prompt_tokens`, its audio output in `completion_tokens`, and only the rest above 272k. A 300 000
  prompt with 10 000 hour writes is `prompt_tokens_above_272k` 290 000 and `prompt_tokens` 10 000.
  `total_tokens` holds every request.
- **Charging** (`CounterTable.Rate`). A counter is charged at its own rate. A derived counter the
  pricing holds no rate for is charged at its base counter's: no `prompt_tokens_above_272k` rate
  means the `prompt_tokens` rate. So a pricing without those rates charges as it always did.

The control plane prices a drain from the same table and the same fallback, so both sides price
the same amounts at the same rates. A new bracket is rows in the push, not a release here; a new
root (a vendor fee reported in `usage`) is one parser line that fills its bucket, plus its row.

**Cost lands with its counters, tagged by the pricing that charged it.** The route carries the
pricing in force; a price change lands with the push that carries it, and a request is charged by
the pricing its gateway held. One Lua script per request: HINCRBY every counter, each priced counter again as `p:<pricing id>:<counter>`,
then `cost` and `p:<pricing id>:cost` — Σ counter × rate, nano-USD, truncated per counter, 0 on an
unpriced route — then, for a prepaid key only, `HINCRBY key:<hash> spent cost` and `HSET key_spent
key_balance` (`budget − spent`, negative once overspent; last writer wins) on the usage hash. A free
team's usage is counted and priced the same, but tagged `f:<pricing id>:` instead of `p:`; its keys'
`spent` never moves and the drain carries no balance for them, so turning the team prepaid later
starts it at what it loads. The tag is set per request, so a drain that spans a flip carries both and the
pull bills only the `p:` part. The pull prices each
`p:<pricing id>` or `f:<pricing id>` group with that same pricing's table, so the two sides can only disagree when they
hold different rates for one pricing id. A drain therefore never sees a counter without its cost, or a cost without the spend it
moved. The spend moves only on a key the control plane has pushed.

### Correlation

Every request — admitted or refused, a health probe, a scrape, an admin call — gets a `uuid v7`
where the access line starts: time-ordered, opaque, canonical and overriding whatever the client
sent. It is answered under both `X-Request-Id` and `Request-Id`, so an OpenAI SDK's and an Anthropic
SDK's `_request_id` are both ours whatever the route. An ingress adopts the gateway's id rather than
minting one, and vLLM adopts `X-Request-Id` as its own request id, so one grep for it crosses the
gateway log, the ingress log and the engine log.

Where it went — key, model, deployment, engine, upstream — is on the access line under `rid`, not
in the id. So is how it ended, as `cut`: `-` for a response that finished, else who ended it —
`client_left`, `upstream_idle` (silent for the read timeout) or `upstream` (its body broke off).
An upstream that ends a stream early but cleanly cannot be told from one that finished, and reads
`-`. On a provider route the vendor's own id is `upstream_rid` on that line and never reaches
the client; ours never reaches the vendor. `attempts` is how many times an upstream was dialled
for the request — 0 when it was refused before any, more than 1 when `retry` moved it to another
vendor key or `fallback` to another model. `model` is the one the client asked for; `fallback` is
the model that served instead, `-` when the one asked for did.

A caller tags its own requests with `X-Grove-Metadata: app=hrms, trace=7f3a-91`, and the tags land
on the access line as `meta: {"app": "hrms", "trace": "7f3a-91"}`: which app spent a key several
apps share, or the caller's own correlation id. Up to 16 pairs in 2 KB; keys are 1-32 of `a-z 0-9 _ . -`,
values 1-128 printable ASCII without a comma, and a repeated key keeps its last value. Anything
else is a 400 before the request goes further. The tags are logged, never counted, and never sent
upstream. They do not pin routing; `X-Grove-Session` does that.

The access line describes the last attempt only. Every attempt before it — one the client never
saw, because `retry` or `fallback` held its answer and moved the request on — leaves a line of its
own in the same log, `msg="attempt"`, under the same `rid`:

| Field | Is |
|---|---|
| `attempt` | which dial failed, from 1; `0` for a model `route` could not place, so nothing was dialled |
| `model` | the model this attempt was on |
| `upstream`, `deployment`, `engine`, `upstream_rid`, `upstream_status`, `reason`, `cut` | as on the access line, for this attempt; `reason` is the pick's refusal when nothing was dialled, `upstream unavailable` or `upstream timed out` when the dial gave no status |
| `upstream_key` | id of the vendor credential dialled; `-` on an engine |
| `rt` | seconds this attempt took |
| `moved`, `to` | what the request moved to: `key` and the next credential's id, or `model` and the fallback |

A request that dialled three times is two `attempt` lines and one `access` line. A fallback passed
over without a dial (not granted, no route on this surface, takes less than the request carries)
is in the process log, `fallback skipped`.

---

## How to do things

### Add a middleware

One file in `transport/http/middleware/`, one `Register`, one name in the chain.

```go
func init() { Register("guardrails", newGuardrails) }

func newGuardrails(deps Deps) (Middleware, error) {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state := From(r)          // Identity, Model, Body, Decision — whatever ran above
			if bad(state.Body) {
				deny(w, r, domain.Deny(http.StatusForbidden, "blocked by policy"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}
```

Then name it in `config.json` → `middleware`, and SIGUSR1. Nothing else changes. A name that is not
registered is refused and the running chain is kept.

If it needs something not on `Deps`, that is the signal to add it there — and to ask whether the
thing it needs belongs on the request path at all.

### Add a request transform

Body rewrites specifically. Same shape, in `service/transform/`:

```go
func init() { Register(alias{}) }

type alias struct{}

func (alias) Name() string        { return "alias" }
func (alias) Endpoints() []string { return nil }  // nil = every path
func (alias) Apply(ctx Context, body Body) (bool, error) { … }  // bool = "I changed something"
```

Each transform declares its own endpoint gate, so adding one for `/v1/messages` does not mean
editing a shared check. Return `false` when nothing changed — a body no transform touched is
forwarded byte-for-byte rather than re-encoded.

Registered is not running: a transform runs only while the `transforms` list names it, and the
default list is `config.Defaults()`.

**A dropped field is logged for you.** A transform that removes a field just deletes it; the chain
compares what the caller sent with what is left and writes one Warn per user and field —
`request field dropped user=… field=… transform=…` — since nothing upstream errors on a field that
is not there. The name only, never the value. Top-level fields; a rewritten one is not a drop.

| transform | what it does |
|---|---|
| `modelmap` | rewrites `model` to the route's `upstream_model` |
| `streamusage` | forces `stream_options.include_usage` on a streaming completion, and `continuous_usage_stats` beside it for an upstream the `vendors` table says takes it (our engines, Baseten) |
| `servicetier` | drops a caller's `service_tier` on every hop: it picks the vendor's price class, which is never the caller's to choose |
| `vendorfields` | on a vendor hop, sends the output cap under the one name that vendor reads, from the `vendors` table of where an upstream differs from the shape (see [The client interface](#the-client-interface)). Today the output cap: `max_completion_tokens` for OpenAI (400s on the old name), `max_tokens` for every other (they ignore the new one, uncapping output). Logged as a drop of the other name |
| `cachesalt` | prefixes a caller's `cache_salt` with their tenant, strips it on a vendor hop. Not in the default list |

### Add a storage backend

Implement the interface in `repository/repository.go` and wire it in `cmd/pathway/main.go`.
Nothing in `service/` knows which one it got. For a second destination (say a local SQLite archive
of usage), a decorator holding two `repository.Usage` values is the whole change — the primary's
error is returned, the archive's is logged.

### Offload a decision to another service

A middleware, not a new abstraction: `offload("route")` above `route`, POSTing the decision inputs
under a deadline. On 200 it puts the answer in the state and `route` no-ops; on timeout or error it
logs and falls through. Fallback is chain order.

**The invariant if you build this: the remote decides, the gateway always claims.** A slow-but-alive
remote that answers after you gave up would otherwise claim a slot nothing ever releases, and that
engine leaves rotation permanently.

---

## Configuration

Split by **lifetime**, and disjoint — nothing appears in both halves.

### Environment — identity, secrets, sockets, paths (`/etc/pathway/agent.env`)

| | |
|---|---|
| `GROVE_ADMIN_TOKEN` | **required**; the process refuses to start without it |
| `GROVE_GATEWAY_ID` / `GROVE_INGRESS_ID` | which plane; both set is a refusal |
| `GROVE_INGRESS_TOKEN` | required on an ingress; blank there refuses every gateway |
| `GROVE_GATEWAY_REGION` | this gateway's region, which `PickRoute` prefers |
| `GROVE_GATEWAY_GEOGRAPHY` | this gateway's geography; a user pinned to another (any pin, when blank) gets 403 `this key is restricted to geography <g>` on inference and `/v1/models` |
| `GROVE_REDIS_ADDR` | default `127.0.0.1:6379`; the store's private address when shared |
| `GROVE_REDIS_PASSWORD` | the shared store's `requirepass`; blank = no AUTH |
| `GROVE_LISTEN_HTTP` / `GROVE_LISTEN_HTTPS` | the data path; at least one is required |
| `GROVE_PUBLIC_HOST` | the shared name customer traffic arrives on |
| `GROVE_SELF_HOST` | this box's own name — carries `/grove-admin` and the scrape |
| `GROVE_TLS_CERT` / `GROVE_TLS_KEY` | fleet wildcard; hot-reloaded from disk on change |
| `GROVE_HTPASSWD` | bcrypt htpasswd for `/metrics/node` |
| `GROVE_NODE_EXPORTER_URL` | default `http://127.0.0.1:9100/metrics` |
| `GROVE_ACCESS_LOG` | file for the per-request line; blank → stdout |
| `GROVE_ERROR_LOG` | file mirroring Warn and above out of the process log; blank → stdout only |
| `GROVE_PAYLOAD_LOG` | file for prompts and outputs of users flagged `log_payloads`; customer content, so its own file and retention. Blank turns `payloadlog` off box-wide |
| `GROVE_CONFIG` | tunables path; default `/etc/pathway/config.json` |
| `GROVE_PID_FILE` | optional |

### File — tunables, re-read on **SIGUSR1**

```json
{
  "log_level": "info",
  "middleware": ["recover", "accesslog", "drain", "auth", "quota", "body", "modelaccess",
                 "payloadlog", "route", "meter", "fallback", "retry", "transform", "upstreamauth"],
  "transforms": ["modelmap", "streamusage", "servicetier", "vendorfields"],
  "synthetic_session_ttl": "0s",
  "capacity_wait": "0s",
  "max_body_bytes": 33554432,
  "upstream_read_timeout": "600s",
  "upstream_tls_verify": false,
  "drain_timeout": "630s",
  "lame_duck": "5s",
  "upgrade_timeout": "30s",
  "usage_retention": "168h",
  "usage_spool": "/var/lib/pathway/usage-spool.jsonl",
  "usage_spool_max_bytes": 1073741824,
  "maintenance": false
}
```

Every field is optional; an omitted one keeps its default, and a missing file is all defaults.

**A rejected file changes nothing.** Bad JSON, an unknown key, a bad duration, or an unregistered
middleware name → one error line and the running configuration is kept, applied whole or not at all.
Parsing is strict on purpose: a knob that silently became its default is a knob the operator
believes they turned.

`upstream_read_timeout` bounds an upstream's silence, not a request's length: the wait for its
headers, and every wait for more of its body after them. A model that thinks for ten minutes before
its first token needs it raised; a stream that talks for an hour does not. Running out before the
headers is a **504** `upstream timed out`; after them, an event stream ends in an error event (see
[Streaming](#streaming)).

`synthetic_session_ttl` is the one worth knowing. `0s` balances every caller that names no session
of its own; `30m` pins each API key to one engine, which is what a single-placement fleet always
did and the lever to pull if balancing goes wrong.

`capacity_wait` is how long a request waits for a slot when every upstream of its model is at its
`capacity`, before the 429. `0s` refuses at once. Nothing is claimed or billed while it waits, and a
client that leaves stops the wait. A vendor row is never counted and has no cap here: the vendor's
own 429 is its cap.

`maintenance: true` refuses every new data request with 503 `maintenance` (`Retry-After: 30`) and
fails `/healthz`, while requests already running finish. It lives in the file, so a box restarted
in maintenance comes back in it. `GET /grove-admin/in-flight` → `{"maintenance", "in_flight"}` is
how the control plane knows the box has gone idle. An older binary refuses a file carrying this key
(unknown field), so it is written only once this release is deployed.

---

## Signals

| | |
|---|---|
| `SIGHUP` | **upgrade** — fork a child on the current binary, hand it the listening sockets |
| `SIGUSR1` | **reload** — re-read `config.json` |
| `SIGTERM` | **drain** — stop serving, finish what is in flight, exit |


### Upgrade

The child inherits the listening file descriptors and starts accepting immediately, while the parent
drains behind it. No connection is refused and none is queued unanswered — which matters here
because a streaming completion runs for minutes, so a stop/start would leave clients waiting that
long.

**A bad binary is a no-op.** If the child fails to start or never signals ready, the parent logs it
and keeps serving on the old binary.

Two things to respect:
- Replace the binary by **rename**, never by truncation — overwriting a running executable fails
  with `text file busy`. `ansible.builtin.copy` already writes-then-renames.
- Start it by **absolute path**. The child is spawned from `os.Args[0]`; a relative one resolves
  against the working directory. The process warns once at startup if it sees one.

### Drain

The flag flips first and the process keeps *accepting* for `lame_duck` (default 5s), answering 503.
That window is what lets a health check notice: `Server.Shutdown` closes the listener at once, so
without it a fresh connection gets refused rather than an answer, and only an already-open keep-alive
connection would ever see the 503. Then live handlers get `drain_timeout` (default 630s — longer
than the upstream read timeout, so a stream the engine would have finished is never cut here first),
and anything past that is closed. Closed handlers still run their defers, so every in-flight slot is
released and whatever usage was captured is recorded.

No lame-duck on an *upgrade* handover: the child is already accepting on the same socket.

Size `lame_duck` to at least one health-check interval of whatever sits in front: shorter, and the
balancer can still be sending when the socket closes, and those clients get connection refused
instead of a retryable 503. `0` skips the window and closes the socket at once. Requests already
running are never refused by it — only new ones are.

---

## The contract with the control plane

Grove pushes state; the gateway projects it into local Redis and never calls back. **These shapes
are the interface — changing one means changing `agent_sync.py` and `usage_pull.py` too.**

| Key | Type | Written by |
|---|---|---|
| `key:<sha256(secret)>` | hash | state push, `keys` section; `spent` by the gateway |
| `model_group:<Model Group>` | hash | state push, `groups` section |
| `deploy:<model>` | JSON array of routes | state push, `routes` section |
| `grove:state_hash` | hash | state push — per-section/bucket fingerprints of what this box holds |
| `lim:<key prefix>:<metric>:<window>:<bucket>` | counter, kept two windows | the gateway — see *Rate limiting* |
| `usage:<key prefix>` | hash | the gateway; set aside by `GET /grove-admin/usage` |
| `drained:<drain id>:<key prefix>` | hash, kept `usage_retention` once acked | `GET /grove-admin/usage` renames a live counter here |
| `drain:unacked` | set of `<drain id>:<key prefix>` | every counter set aside and not yet acked |
| `adjust:<id>` | string, 7 days | `POST /grove-admin/spend-adjust` — ids already applied |
| `accrued:<request id>` | string, 7 days | the spool's replay — requests already landed from it |
| `sticky:<session>` | string, 30m | the gateway |
| `inflight:<engine>` | sorted set, member = request id; never a vendor's URL | the gateway |
| `health:<target>` | counter, 60s | the gateway |
| `pk:<key id>` | hash, lifetime | the gateway — what each vendor credential answered; read by `GET /grove-admin/provider-keys` |

Nothing the control plane pushes carries a TTL. The push is hash-gated, so a record that expired
would stay missing until its section changed; only gateway-owned keys expire
(`TestPushedStateNeverExpires`).

### How the push works

`POST /grove-admin/state` is desired state, whole, and **absence prunes**. The body carries any
subset of three sections — groups, keys, routes — each stamped with a
hash Grove computed. The agent applies the whole body in ONE Redis MULTI: HSET every named
record, DEL every record in a pushed section the payload does not name, then store the hashes in
`grove:state_hash`. A Redis error is a 500 and none of it lands — the hashes never claim state
that did not arrive. Only `model_group:/key:/deploy:` are ever pruned; usage,
sticky, inflight and health keys are the gateway's own.

Every admin body is decoded strictly: a field this binary does not know is a 400 naming it
(`bad body: json: unknown field "credentials"`), nothing is stored and no hash is written, so the
Pathway Sync row says which field and the next tick pushes again. The alternative — keep the
record without the field under the hash of the full payload — is drift no later push would see,
which is what happened once. Consequence: a new route field ships in the binary before Grove
pushes it.

`GET /grove-admin/state-hash` returns that stored map. Grove diffs its computed hashes against it
every 2 minutes and pushes only what differs — an in-sync box costs one GET. `keys` is split into
256 buckets (`domain.BucketOf` = `sha256(id)[:2]`, same rule Grove uses) hashed
independently, so one minted key ships one bucket, not the population. A wiped Redis has no
hashes, reads as total drift, and is fully rebuilt on the next tick — that is the only repair
path and the only one needed. Both planes mount these endpoints; an ingress only ever receives
the routes section. Contract: `plan_agent_state_sync.md` in the infer-plat tree. The old
per-section `PUT`/`DELETE` endpoints remain for one release after the control plane cuts over.

Every admin endpoint is gated on `X-Grove-Admin-Token`, compared in constant time, and mounted on
`GROVE_SELF_HOST` only — a push has to reach **one** gateway, and the public name means all of them.

`GET /grove-admin/in-flight` is read-only: `{"maintenance": bool, "in_flight": n}`, where
`in_flight` counts data requests past the `drain` stage, realtime sessions until they close.

`GET /grove-admin/usage[?keys=p1,p2]` answers `{"drains": {"<drain id>": {prefix: hash}}}` and
deletes nothing. Each live `usage:<prefix>` — every one, or only the listed prefixes, with no SCAN —
is RENAMEd to `drained:<id>:<prefix>` under a new drain id and added to `drain:unacked`, in one Lua
call, so a request metered mid-drain lands wholly in the drain or wholly on a fresh counter. The
answer is every pair still in `drain:unacked` (only the listed prefixes' when `keys` is given),
old drains included. `POST /grove-admin/usage/ack {"acks": {"<drain id>": [prefix, ...]}}` takes
the pairs the control plane recorded out of `drain:unacked` and keeps them for `usage_retention`
(default a week), answering `{"ok": true, "count": n}`; a pair not waiting is a no-op. A key it never
acks is answered again, under its own id, on every pull, while the rest move on: a control plane
that failed to record one user loses nothing and holds nobody else back, and one that recorded it
but lost the ack dedupes on the (drain id, prefix) pair. Nothing live and nothing waiting answers
`{"drains": {}}`.

**The spool.** A request that finishes while the store is down (new ones already 503 at admission)
cannot land its usage. It is appended as one JSON line — `{"id": "<request id>", "prefix", "fields",
"cost", "user", "budget", "at"}` — to `usage_spool` and fsynced. At start and every 5 s while it is
non-empty the gateway PINGs the store; once it answers, each line is replayed through the accrue
script behind `SET accrued:<request id> NX EX 604800`, so a pass that dies half way lands nothing
twice, and the file is rewritten (tmp + rename) with only the lines still failing. A line that fails
five passes while the store answers is set aside in `<usage_spool>.dead` as `{"id", "line",
"error"}`; one that does not parse goes there at once. Past `usage_spool_max_bytes` (1 GiB) usage is
logged and dropped. A store still down counts no tries. Only losing the box's disk loses spooled
usage.

The pull hands the dead lines over: `GET /grove-admin/usage` also answers `"dead": [{"id", "line",
"error"}]` and `"spool": {"depth", "replayed", "dropped", "dead"}` (lines waiting now, and counts
since the process started). The control plane lands or records each dead line and names it in the
ack — `"dead": ["<request id>", ...]` beside `"acks"` — which removes it; `count` includes them.

`GET /grove-admin/provider-keys?ids=a,b` answers `{id: {requests, ok, rate_limited, rejected,
failed, last_used, last_rate_limited}}` per vendor credential, lifetime and never drained; an id
never dialled is zeros. The control plane sums it across stores for the operator.

`POST /grove-admin/spend-adjust {"key", "delta", "id"}` corrects one key's `spent` on this store
by `delta` nano-USD, once per `id` (`key` is the record id, sha256 hex) — a retry answers
`{"spent", "applied": false}` and moves nothing. A key this store does not hold is a 404; nothing
is invented.

### The records themselves

```
key:<sha256(secret)>       status  team  prefix  group (comma list)  allow  deny  limited  log_payloads
                           geography  prepaid  budget  spent  limits
model_group:<Model Group>  models
usage:<key prefix>         request_count  prompt_tokens  completion_tokens  total_tokens  cached_tokens
                           cache_write_tokens  cache_write_1h_tokens  audio_tokens  completion_audio_tokens
                           prompt_tokens_above_272k  cached_tokens_above_272k
                           cache_write_tokens_above_272k  completion_tokens_above_272k
                           audio_seconds
                           cost  key_spent  key_balance
                           m:<metric>:<model>  m:<metric>:<deployment>
                           p:<pricing id>:<priced counter>  p:<pricing id>:cost
```

`group` / `allow` / `deny` / `models` are comma lists; blank parses to a map that answers false
to everything, which is the fail-closed default. `group` holds every group the key is in, so a
name containing a comma would split — Grove refuses one. `team` is the Central Team the key bills
to: the cache-salt namespace and the payload log's join field, read by no gate.

`prepaid` / `budget` / `spent` are the credit gate. `budget` is the key's cap — the slice of its
team's balance the control plane allotted it, nano-USD; Grove keeps Σ caps within the balance.
`spent` is this Redis's own lifetime counter: every metered request of a prepaid key moves it, a
push never does (the push is an HSET of the fields it names), and only `spend-adjust` corrects it.
This box's balance for the key is `budget − spent`; Grove keeps the team's own from the same
credits and audits the two every pull. `prepaid && spent >= budget` → 402 `credit balance
exhausted` (`billing_error` on the Anthropic surface), at `quota` before the body is read, and
`/v1/models` refuses through the same `Evaluate`. `limited` is the control plane's verdict on the
team and refuses alike. A key pinned by `geography` lives on one store, so its gate is exact —
every gateway on that store moves the one counter — and a team's keys elsewhere spend their own
caps; `limited` from the pull is the backstop once the team as a whole is out.

`deploy:<model>` is a JSON array, replaced whole:

```json
[{
  "engine_url":   "https://10.0.0.9/e/md-00007",
  "internal_key": "<the engine's own key>",
  "healthy":      true,
  "region":       "ap-south-1",
  "capacity":     1024,
  "deployment":   "MD-00007",
  "server":       "INF-1",
  "kind":         "direct",
  "pricing":      {"id": "mp-a1", "rates": {"prompt_tokens": 3000000000, "completion_tokens": 15000000000}}
}]
```

`engine_url` is a **base**; the path the client asked for is appended to it. `in_flight` is
computed here and never pushed — the control plane has no view of what is running right now.
`input_modalities` and `output_modalities` are what the model takes and gives (`["text","image"]`,
`["text"]`), stamped on every row of the model. The outputs are the path gate: chat and messages
need `text`, `/v1/embeddings` needs `embeddings`, `/v1/audio/transcriptions` and `/translations`
need `transcription`; a model that does not give it is our own 404 before anything is dialled, and
any other path is nobody's to refuse. The inputs are read only for a fallback, which is passed over
when it lacks what the request carries (see [Fallback models](#fallback-models)). A row that declares nothing — one pushed before
the control plane said — is unrestricted and not judged. There is no `modality` field: a push
that carries one is refused like any unknown field.
`pricing` is the Model Pricing in force — its id and its sell price per counter, nano-USD per unit
(Mtok, minute, request) — stamped on every row of the model. Absent on an unpriced model, and such
a request costs 0.

A `kind: "provider"` row dials a third-party vendor instead of anything we run, and carries its
own fields:

```json
[{
  "engine_url":     "https://api.anthropic.com",
  "internal_key":   "",
  "credentials":    [{"id": "<key row id>", "secret": "<the vendor's API key>"}, ...],
  "key_selection":  "round_robin",
  "healthy":        true,
  "capacity":       0,
  "vendor":         "anthropic",
  "kind":           "provider",
  "upstream_model": "claude-sonnet-4-5-20250929",
  "api_version":    "2023-06-01",
  "dialect":        "anthropic",
  "denied_tools":   ["web_search_20250305", "web_search_20260209", "code_execution_20260521"]
}]
```

`denied_tools` is what the vendor would run on its own side and bill outside the token counts,
which nothing here meters: a `tools[].type` or a top-level request field, as the control plane's
Denied Tool rows list them for that vendor. `route` reads every `tools[].type` and every top-level
field off the client's own bytes and drops a row that denies one of them; when no row of the model
is left, the request is a 400 `<model> does not run <name>` in the surface's own envelope, before
any dial and above `meter`, so it bills nothing. A row that denies nothing runs everything, which
is every row we run ourselves, so a model with an engine of ours beside the vendor row goes to the
engine. A fallback whose rows deny the tool is passed over like one that lacks an input. Deleting
a Denied Tool row is how a tool is let through once it is priced. The list is a deny list, so a
tool a vendor adds tomorrow runs until its row exists.

`upstream_model` is what the `modelmap` transform puts in `body.model`; blank means send the
caller's unchanged, which is every route we run ourselves — an engine is started under the Grove
id. A realtime upgrade carries its model in the query string and has no body for a transform to
rewrite, so the same substitution is applied to `?model=` in the transform stage. Access, routing, metering and `/v1/models` all key on the id the caller sent, so the rewrite
cannot desync a grant from a route. On a provider hop the client's `Authorization` is deleted
rather than replaced, the vendor's own key goes in the header its `dialect` expects (`x-api-key`
plus `anthropic-version` for an Anthropic front, a Bearer for an OpenAI-compatible one and for an
Anthropic front the `vendors` table says reads one), and the
certificate is verified whatever `upstream_tls_verify` says — that hop leaves our network carrying
someone else's key.

`credentials` is every key the control plane holds with the vendor, each under the id it is
counted by, in ring order; `internal_key` stays blank on a provider row and is the one-key spelling
an engine or ingress row keeps (`Keyring()` reads either). `route` takes the keys in turn, one
cursor per ring in this process — a vendor's ring is the same on every model it serves, so its keys
share the turn across models, which is how the limits they share are drawn on evenly; two gateways
each take even turns without agreeing on whose it is. `key_selection` is carried for the day there
is a second strategy. A session still changes key between requests, so a vendor's prompt cache is
per credential. The `retry` stage then walks on round the ring from the loser, on the key's own
failures only: a **429** spends the key — each is tried once, then the rate-limited ones are opened
once more, since a quota window may have slid — and a **401/402/403** retires it for the rest of the
request. The second exhaustion is the
client's answer, headers and body as the vendor sent them. Nothing else is retried: a 502 on one
key is a 502 on the next. A held attempt leaks nothing to the client; the request is billed once
whatever it walked, and the access line's `attempts` counts the dials. Every attempt lands in
`pk:<key id>` (`requests`, `ok`, `rate_limited`, `rejected`, `failed`, `last_used`,
`last_rate_limited`), lifetime, which `GET /grove-admin/provider-keys?ids=a,b` answers.

`dialect` is the API shape the row speaks, `openai` or `anthropic`, and nothing translates between
them: a request reaches only rows of its surface's dialect. A vendor with both fronts is two rows. On
a row we run, blank means both, since vLLM answers both natively; on a provider row, blank is
malformed and the row serves nothing.

A provider route is also **deny-by-default on the surface check**. `Serves` is generous to an engine
because an engine answers on more than the OpenAI core (`/tokenize`, `/v1/rerank`), but a vendor is a
closed set we already know, so `ServesRoute` holds it to its dialect's paths — `/v1/chat/completions`
for `openai`, `/v1/messages` for `anthropic`. Anything else is our 404 at `routing.go`, above
`meter`, instead of a round trip that comes back as theirs. The paths are one table,
`vendorPaths` in `domain/dialect.go`, path → dialect: serving another (`/v1/responses` for
`openai`, say) is a row there.

The surface is not read off the path. The `/anthropic` alias strips its prefix before the chain
runs, so below it `/anthropic/v1/messages` and a stray root `/v1/messages` look the same; the alias
records the surface on the request as it strips (`respond.WithDialect`), and everything below —
`routing.Request.Dialect`, the refusal's shape, the proxy's own errors — reads `respond.Dialect`.
Unmarked is OpenAI, because root is the OpenAI surface.

### Backwards compatibility that is still load-bearing

- A push strips the fields older control planes wrote on a key (`user`, `can_read_balance`,
  `models`, `priority`), so a record never shows the current policy beside a stale pointer. A
  control plane still sending a `users` section is refused 400 by name: the policy moved onto the
  key in one cut, and this binary must land on a box before that control plane pushes to it.
- A route with no `kind` is `direct`, which is what every route pushed before the split was.
- A route with no `upstream_model` sends the caller's `model` unchanged, which is what every route
  pushed before the vendor split did.

### Public endpoints

| | |
|---|---|
| `POST /v1/*` | the data path for OpenAI clients. `/v1/messages` here is a 404 pointing at `/anthropic` |
| `POST /anthropic/v1/*` | the data path for Anthropic clients (`ANTHROPIC_BASE_URL=<gateway>/anthropic`): `/v1/messages` only — anything else under it is a 404 in Anthropic's shape — keyed by `x-api-key` or a Bearer |
| `GET /v1/models` | answered here, never forwarded — an engine only knows its own model. With a key: what that key may use through the OpenAI surface. Without one: 401 |
| `GET /anthropic/v1/models` | the same, in Anthropic's list shape, for what that key may use through the Anthropic surface |
| `GET /v1/credits` | answered here: what the key has left of its own cap on this store, in US dollars — `{"balance", "spent", "is_free_user"}`. `balance` is `budget − spent`, the figure `quota` gates on, negative once overspent and still readable then. Nothing of the team's balance is exposed, so every key may read it. A key of a free team reads zeros and `"is_free_user": true` |
| any other method on either | 405 `Allow: GET`, not forwarded — a chat body POSTed at the list would otherwise reach the proxy and be refused as a model that "does not serve" the path |
| `GET /healthz` | 200, or 503 while draining or in maintenance |
| `GET /metrics/node` | node_exporter behind bcrypt basic auth |

---

## When things break

The rule everywhere: **degrade toward serving.** A gateway that refuses traffic because a counter
was unreadable turns one broken dependency into an outage.

| What fails | What happens |
|---|---|
| Redis unreachable at **startup** | refuses to start — a gateway that cannot read its keys serves nothing, and finding out on the first customer request would report it as a routing fault |
| `key:` / `model_group:` read fails | **503**, never 401 — "we cannot read your key" must not send someone to rotate a credential that was fine |
| in-flight counts unreadable | every count reads 0, so the pick degrades to first-healthy. Balancing is an optimisation on a table that is already correct |
| health counters unreadable | every route stays as the control plane pushed it. Ejection is an optimisation too |
| sticky read/write fails | one cold prefix cache, not a wrong answer |
| usage write fails | the request already succeeded, so it is not failed retroactively: its usage goes to the **spool** (below) and lands once the store answers. `spent` did not move either, so the gate runs loose by that request until then |
| the engine is dead | **502** `upstream unavailable`, and the hop counts against the target — three in a row and it leaves rotation |
| the engine accepts the connection but sends no headers | **504** `upstream timed out` after `upstream_read_timeout` (a dial that runs out is a 504 too); counts against the target |
| the client leaves before the upstream answers | the upstream call is cancelled at once; the access line reads **499** `cut=client_left`, and the hop does not count against the target |
| the client leaves in the middle of a response | the upstream call is cancelled at once; `cut=client_left`. What the upstream had reported by then is metered, the rest is not |
| the upstream goes silent after its headers | cut after `upstream_read_timeout` of silence, `cut=upstream_idle`, and the hop counts against the target. An event stream ends in an `upstream went silent` error event; any other body is dropped. A stream that keeps talking is never cut, however long it runs |
| an HTTP/2 upstream connection dies without closing | pinged after 15s with nothing received and closed 90s later: a request waiting on it gets **502** `upstream unavailable` and counts against the target, and the next one dials a fresh connection |
| pathway outgrows the unit's `MemoryMax` | the kernel kills it inside its own cgroup and systemd starts it again 2s later: live streams drop, the rest of the box is untouched. Without a cap the whole box runs out and the kernel picks what dies |
| the upstream's body breaks off | `cut=upstream`, and the hop counts against the target. An event stream ends in an `upstream broke off the stream` error event; any other body is dropped |
| the client drips its body | **408** once 60s pass without the part `body` reads; nothing is routed or billed |
| every replica is full | waits up to `capacity_wait`, then **429**, distinct from 503 on purpose: the model is up. Nothing is claimed or billed while waiting |
| the model has no placement | **503** |
| the control plane is down | nothing happens. The gateway serves the last table it was pushed, indefinitely |
| the tunables file is bad | the running configuration is kept, one error line says why |
| a new binary will not start | the old process keeps serving; the upgrade is a no-op |
| the certificate file is unreadable | the certificate already in memory keeps being served |

### Status vocabulary

Every refusal speaks its surface's shape — `{"error":{"message":…,"type":…}}` for OpenAI clients,
`{"type":"error","error":{…}}` under `/anthropic` — so a client parses them the same way whichever
gate produced it. `type` is the error's class, named off the status as the
Anthropic API names it (`authentication_error`, `rate_limit_error`, …), except `maintenance` and
`draining`, which a 503 alone does not say.

| | Means | Client should |
|---|---|---|
| 400 | the body is not a JSON object, names no `model`, or its `fallbacks` is not a list of at most 3 names | fix the request |
| 401 | no key, unknown key, revoked key | fix the credential |
| 403 | the key exists but may not use this model | ask for access |
| 408 | the body did not arrive within 60s of the headers | resend on a working connection |
| 413 | body over `max_body_bytes` | send less |
| 402 | out of prepaid credit | top up; nothing to retry |
| 429 | every replica at capacity, or the key is over a rate limit | back off and retry — `Retry-After` is set for a rate limit |
| 499 | the client left before the upstream answered. Only ever in the access line: nobody is there to receive it | – |
| 502 | the engine could not be reached or failed the hop | retry; another replica may take it |
| 504 | the engine sent no headers within `upstream_read_timeout`, or could not be dialled in time | retry; another replica may take it |
| 503 | the model has no healthy placement, a store is unreadable, or the gateway is draining or in maintenance | retry with backoff — `Retry-After` is set when draining or in maintenance |

---

## Running it locally

```sh
go build ./... && go vet ./... && go test ./...
go test -race ./internal/transport/http
```

The tests need nothing running: `repository/memory` is a behaving in-memory store, and
`internal/transport/http/dataplane_test.go` drives the whole chain against a fake vLLM over
`httptest`. That is the file to read first — it shows the request as the engine receives it.

Against a real Redis:

```sh
redis-server --port 6399 --save '' --daemonize yes
GROVE_ADMIN_TOKEN=tok GROVE_GATEWAY_ID=gw-dev \
GROVE_REDIS_ADDR=127.0.0.1:6399 GROVE_LISTEN_HTTP=127.0.0.1:8080 \
go run ./cmd/pathway
```

Seed it the way the control plane does, then call it:

```sh
curl -XPUT localhost:8080/grove-admin/groups -H 'X-Grove-Admin-Token: tok' \
  -d '{"groups":[{"name":"acme","models":"qwen3-4b"}]}'
curl -XPUT localhost:8080/grove-admin/keys -H 'X-Grove-Admin-Token: tok' \
  -d "{\"keys\":[{\"key_hash\":\"$(printf gr_demo | sha256sum | cut -d' ' -f1)\",\"prefix\":\"dev\",\"team\":\"you\",\"group\":\"acme\",\"status\":\"active\"}]}"
curl -XPUT localhost:8080/grove-admin/routes -H 'X-Grove-Admin-Token: tok' \
  -d '{"routes":{"qwen3-4b":[{"engine_url":"http://127.0.0.1:8000","internal_key":"k","healthy":true,"deployment":"MD-1","kind":"direct"}]}}'

curl localhost:8080/v1/chat/completions -H 'Authorization: Bearer gr_demo' \
  -d '{"model":"qwen3-4b","messages":[]}'
redis-cli -p 6399 HGETALL usage:dev
```

With nothing listening on `:8000` that last call is a 502 and only `request_count` accrues — which
is itself the correct behaviour, and worth seeing once: an abandoned or failed request still counts
as a request, and the failure counts against the target. Point `engine_url` at a real vLLM (or the
fake in `dataplane_test.go`) for token counts.

---

## Deploying

Tagged versions publish a static `linux/amd64` and `linux/arm64` binary (`CGO_ENABLED=0`,
`-trimpath`) plus a `sha256sums.txt`. A control plane downloads the checksummed asset, installs it to
`/usr/local/bin/pathway`, and runs it under systemd as an unprivileged user with `PIDFile=` inside a
`RuntimeDirectory=` — the PID changes on an upgrade and systemd follows the child through the file
tableflip writes there (`Type=notify` + `NotifyAccess=all` is the other way to do it). Grove drives this
from its `install_gateway_agent` role.

- **New binary** → copy + `systemctl reload` (SIGHUP). No dropped connections.
- **Changed tunable** → write `config.json` + SIGUSR1. No restart.
- **Changed `agent.env`** → restart. The child reads the environment from systemd, so a reload would
  not pick it up. This is the only case that drains, and it is rare.
- **Renewed certificate** → copy the file. Nothing else: the loader watches its mtime.

### Release from a branch or a fork

A `v*` tag push releases that commit. For any other branch or name, dispatch the same workflow:

```sh
gh workflow run release.yml -f ref=dialect -f tag=dialect-2026-09-15   # add -R owner/fork for a fork
```

It builds `ref`, creates `tag` on that commit, and publishes the same assets as a pre-release (pass
`-f prerelease=false` to let it become Latest). In Grove, set **Pathway Release** and **Pathway Repo**
in Grove Settings, then create a **Pathway Update**, tick Gateways and/or Ingresses, and Start: it
deploys each server in turn and stops at the first failure. A fork must enable Actions once, and its
releases must be public: boxes download them unauthenticated.

### Build a binary locally

The release build, minus the tag — so what you ship is what CI would have shipped:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.version=dev-$(git rev-parse --short HEAD)" \
  -o dist/pathway-linux-amd64 ./cmd/pathway
./dist/pathway-linux-amd64 --version    # dev-<sha>
```

Static (`CGO_ENABLED=0`), so it runs on any Linux box whatever its libc; `GOARCH=arm64` for a
Graviton front box. Without `-X main.version` it reports plain `dev`.

### Dev deploy

A build off your machine instead of a release, through the same play, with the same tracking
(an Ansible Play under the Gateway Server). From the Grove bench root:

```sh
bench --site grove.localhost execute frappe.enqueue_doc --kwargs \
  '{"doctype":"Gateway Server","name":"gw1-ap-south-1","method":"_deploy_agent","now":True,"agent_binary":"/abs/path/to/pathway-linux-amd64"}'
```

`agent_binary` is an extra-var of Grove's `install_gateway_agent` role: set, that file is copied to
the box in place of the checksummed download, and nothing else about the deploy changes — `agent.env`
and `config.json` are still written whole from the doc, and the binary lands with a `systemctl reload`
(SIGHUP) unless `agent.env` moved. The Setup job (`provision`) takes it too, for a box that needs the
unit shipped as well. `now: True` runs it in this process rather than the queue, so a worker on
stale code is not in the way. Gateway Server only for now; the Ingress Server jobs do not take it.

The doc keeps reporting the pinned release: a box on a dev build reads as that release until the
next **Deploy Agent** click puts it back — which is the point. A dev deploy is a temporary state,
and the button is how it ends.

---

## Things worth knowing before changing something

- **`domain/` must not grow an import.** Its purity is what lets a remote router, or a second
  process, run the identical rule.
- **Anything that claims an in-flight slot must release it.** `meter` does it in a `defer` for
  exactly this reason. One owner for that counter, always.
- **The usage scraper must never modify the response.** It reads what it is already copying. A
  metering feature that reordered or buffered a token stream would be worse than no metering.
- **`upstream_tls_verify` is per target, not per listener.** That is the thing nginx could not do —
  `proxy_ssl_verify` is a per-location directive, so one self-signed box pinned verification off for
  every target sharing that location. Defaulted `false` for parity; it is a default, not a ceiling —
  and it is not a floor either: a `kind: "provider"` hop verifies whatever it is set to, because
  that request leaves our network carrying a vendor's own API key. The transport pool is keyed on
  the setting as well as the host, so the two can never share a connection.
- **Least-in-flight balances concurrent traffic, not sequential.** A sequential caller releases its
  slot before the next pick, so both replicas read zero and the tie takes the first. Fine for one
  chatty client — it keeps a prefix cache warm — but a fleet of many sequential clients pins them
  all to one engine.
- **The HTTP/2 ping is what stops a dead upstream connection eating requests.** One that dies
  quietly (no FIN, no RST) keeps taking new requests for as long as traffic keeps it busy: the 90s
  idle close only runs on a connection with no request on it. Without the ping each request hangs
  until `upstream_read_timeout`, and the connection goes only when the kernel gives up
  retransmitting (about 15 minutes at Linux defaults). It is set through `Transport.HTTP2`, which
  is why `go.mod` asks for Go 1.24.
- **The memory limits live in the unit, not in this code.** Grove writes a drop-in with `MemoryMax`
  and `GOMEMLIMIT` at 90% of it, so the collector works harder before the kernel kills anything.
  `MemoryMax` moves under the running process; `GOMEMLIMIT` is environment, and a SIGHUP child
  copies its parent's, so a changed one is only read at a full restart. During a handover the
  draining parent shares the cgroup, each with its own 90%.
