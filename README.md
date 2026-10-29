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
6. Records the hop's outcome on every request, and the tokens on every one the upstream took — a
   2xx that was then abandoned or cut short included; a refusal, a 5xx or no answer is not a request.
7. Answers each base's `/v1/models` and the root `/v1/credits` itself, and `/metrics/node` behind basic auth.
8. Upgrades its own binary and reloads its own configuration without dropping a connection.

Requirements: Go 1.24.1 or later to build (sonic/loader does not link on 1.24.0), Redis (loopback, or the Network's shared store), a certificate on disk, and an admin token. It refuses to start
without the last one, and refuses to start as an ingress without a data token.

**Reading order** if you are new to it: *Where it sits* → *Design* → *The request path*, then
[`transport/http/middleware`](internal/transport/http/middleware/README.md) for each stage. If you
are about to change something, start from *Where things live*: each package's README holds its
rules and how-tos.

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
  usage write is a transaction so a drain never sees half a request. A key push that moves the key
  to another team closes its books in the same transaction: the live buckets are set aside stamped
  with the team charged, so a request is wholly the old team's or wholly the new one's.

Nothing takes a lock across an I/O call, and no request-scoped value is shared between requests.

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

| Question | File | Doc |
|---|---|---|
| Which engine gets this request? | `domain/route.go` — `PickRoute` | [service/routing](internal/service/routing/README.md) |
| Engine vs vendor routing | `service/routing/engine.go`, `service/routing/vendor.go`; their auth and key ring in `transport/http/middleware/engine.go`, `vendor.go` | [service/routing](internal/service/routing/README.md) |
| What a vendor reads differently | `service/vendor/vendor.go` — `known`, one `Vendor` per third party: its `Headers` and `Quirks` | [service/vendor](internal/service/vendor/README.md), [vendor_quirks.md](internal/service/vendor/vendor_quirks.md) |
| Body rewrites on the way to an upstream | `service/transform/` | [service/transform](internal/service/transform/README.md) |
| May this caller use this model? | `domain/access.go` — `CanUse`, `Evaluate` | [service/admission](internal/service/admission/README.md) |
| How many tokens did that cost? | `domain/usage.go` — `ParseUsage` | [service/metering](internal/service/metering/README.md) |
| Is this target broken? | `domain/health.go` | [Passive ejection](internal/service/routing/README.md#passive-ejection) |
| What does Redis look like? | `repository/redis/` — every key name and TTL | [repository/redis](internal/repository/redis/README.md) |
| What the control plane pushes and pulls | `service/provisioning/`, `transport/http/admin.go` | [service/provisioning](internal/service/provisioning/README.md) |
| The request pipeline | `transport/http/middleware/builtin.go` | [transport/http/middleware](internal/transport/http/middleware/README.md) |
| The proxy itself | `transport/http/proxy/` | [transport/http/proxy](internal/transport/http/proxy/README.md) |
| Endpoints, headers, signals | `transport/http/server.go`, `lifecycle.go` | [transport/http](internal/transport/http/README.md) |
| What a refusal looks like | `transport/respond/` | [Status vocabulary](internal/transport/respond/README.md) |
| Tunables and environment | `config/` | [config](internal/config/README.md) |
| Shutdown, drain, binary upgrade | `transport/http/lifecycle.go` | [Signals](internal/transport/http/README.md#signals) |
| How a reload reaches running state | `cmd/pathway/main.go` — `built.reload` | |

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

Each stage, and what it reads and writes: [transport/http/middleware](internal/transport/http/middleware/README.md).

### A request, layer by layer

What each layer contributes, for one `POST /openai/v1/chat/completions`:

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
| usage write fails | the request already succeeded, so it is not failed retroactively: its usage goes to the [spool](internal/service/metering/README.md#the-spool) and lands once the store answers. `spent` did not move either, so the gate runs loose by that request until then |
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

What each status means, and the shape a refusal takes: [Status vocabulary](internal/transport/respond/README.md).

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

curl localhost:8080/openai/v1/chat/completions -H 'Authorization: Bearer gr_demo' \
  -d '{"model":"qwen3-4b","messages":[]}'
redis-cli -p 6399 HGETALL usage:dev
```

With nothing listening on `:8000` that last call is a 502 and nothing accrues — which is itself the
correct behaviour, and worth seeing once: a request nobody served is not the caller's request, and
the failure counts against the target. Point `engine_url` at a real vLLM (or the fake in
`dataplane_test.go`) for token counts.

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

**The memory limits live in the unit, not in this code.** Grove writes a drop-in with `MemoryMax`
and `GOMEMLIMIT` at 90% of it, so the collector works harder before the kernel kills anything.
`MemoryMax` moves under the running process; `GOMEMLIMIT` is environment, and a SIGHUP child
copies its parent's, so a changed one is only read at a full restart. During a handover the
draining parent shares the cgroup, each with its own 90%.
