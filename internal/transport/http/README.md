# Endpoints, headers, signals

## Public endpoints

| | |
|---|---|
| `POST /openai/v1/*` | the data path for OpenAI clients (`OPENAI_BASE_URL=<gateway>/openai/v1` — that SDK's base carries the `/v1`, Anthropic's does not): everything an engine answers on that shape — chat, completions, embeddings, audio, the realtime upgrade. `/v1/messages` under it is a 404 pointing at `/anthropic` |
| `POST /anthropic/v1/*` | the data path for Anthropic clients (`ANTHROPIC_BASE_URL=<gateway>/anthropic`): `/v1/messages` only — anything else under it is a 404 in Anthropic's shape — keyed by `x-api-key` or a Bearer |
| `POST /v1/*` | nothing: a 404 naming the base the path belongs under (`/openai/v1/chat/completions`, `/anthropic/v1/messages`) |
| `GET /openai/v1/models` | answered here, never forwarded — an engine only knows its own model. With a key: what that key may use through the OpenAI surface. Without one: 401 |
| `GET /anthropic/v1/models` | the same, in Anthropic's list shape, for what that key may use through the Anthropic surface |
| `GET /v1/credits` | answered here: what the key has left of its own monthly cap on this store this month, in US dollars to the cent — `{"balance", "spent"}`. `balance` is `budget − spent`, the figure `quota` gates on, negative once overspent and still readable then. Nothing of the team's balance is exposed, so every key may read it. A key of a free team reads zeros |
| any other method on either | 405 `Allow: GET`, not forwarded — a chat body POSTed at the list would otherwise reach the proxy and be refused as a model that "does not serve" the path |
| `GET /healthz` | 200, or 503 while draining or in maintenance |
| `GET /metrics/node` | node_exporter behind bcrypt basic auth |

## Headers

What a client sees on an answer:

| Header | Whose | When |
|---|---|---|
| `X-Request-Id`, `Request-Id` | the gateway's | every answer. One id under both names; the second is what Anthropic's SDK reads |
| `X-Grove-Fallback` | the gateway's | only when a fallback model served: that model's key |
| `X-Grove-Changed` | the gateway's | only when the gateway changed what the model does for the vendor that answered: `reasoning_effort=none`, `thinking=disabled`, or a dropped field as `temperature=default` or `service_tier=default`, comma-separated (see [vendor_quirks.md](../../service/vendor/vendor_quirks.md)) |
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
| `X-Grove-Session` | client → gateway | names the caller's session (see [Session affinity](../../service/routing/README.md#session-affinity)). Taken off before the hop |
| `X-Grove-Metadata` | client → gateway | the caller's own tags for the access line (see [Correlation](middleware/README.md#correlation)). Taken off before the hop |
| `X-Grove-Fallback` | gateway → client | the fallback that served |
| `X-Grove-Changed` | gateway → client | what the gateway changed in the request that alters what the model does, as `field=value` |
| `X-Grove-Model`, `X-Grove-Session-Key` | gateway → ingress | the model to pick a replica of, and the session to keep on it. Taken off a request to anything that is not an ingress |
| `X-Grove-Engine` | ingress → gateway | the replica that served: how usage reaches a placement the gateway never picked. Read here, not relayed to the client |
| `X-Grove-Reason` | ingress → gateway | why the ingress refused; `no-replica` keeps that 503 from counting against it. Read here, not relayed |
| `X-Grove-Admin-Token` | control plane → gateway | gates every admin endpoint |

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
