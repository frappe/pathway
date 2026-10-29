# The request path

The chain, top to bottom. The diagram is in the [root README](../../../../README.md#the-request-path);
the order is `GatewayChain` in `middleware.go`.

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
| `fallback` | runs the stages below again on the next model in the body's `fallbacks` when the one serving cannot (5xx, or a key failure `retry` could not rotate away). See [Fallback models](../../../service/routing/README.md#fallback-models) |
| `retry` | runs the stages below again with another credential when a vendor refuses the one dialled (429, 401/402/403); counts every attempt against its key |
| `transform` | the registered body rewrites; re-encodes only if one changed something |
| `upstreamauth` | sets the forwarding headers, then the target's own auth: an engine's internal key and the ingress headers (`engineAuth`), or a vendor's key in its scheme (`vendorAuth`) |

Order is load-bearing in two places. `drain` sits above `auth`, so a restarting gateway answers the
same way whether or not the caller's key is any good. `meter` sits directly below `route`, because
`route` claims a slot and everything below it must give that slot back.

The ingress chain is the same machinery, six entries: `recover`, `accesslog`, `drain`,
`ingressauth`, `pick`, `upstreamauth`.

## Bodies that are not JSON

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

## Realtime sessions

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

## The payload log

The one place the gateway keeps customer content rather than metadata, so it is off twice over: the
box needs `GROVE_PAYLOAD_LOG`, and the key needs `log_payloads` (pushed on `key:<hash>` from its
team, read per request, so turning it takes effect on the next one). One JSON line per request, written when the
request ends — a client that hung up mid-stream still gets one, with what it had received:

`rid` (joins the access line), `key`, `team`, `model`, `fallback` (the model whose output this is,
when not the one asked for; else `-`), `path` (after the base's `/openai` or `/anthropic` strip),
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

## Correlation

Every request — admitted or refused, a health probe, a scrape, an admin call — gets a `uuid v7`
where the access line starts: time-ordered, opaque, canonical and overriding whatever the client
sent. It is answered as `X-Request-Id` and travels the hop under the same name: an ingress adopts
the gateway's id rather than minting one, and vLLM adopts `X-Request-Id` as its own request id, so
one grep for it crosses the gateway log, the ingress log and the engine log. Anthropic's SDK reads
`Request-Id`, which is never answered; its `_request_id` is blank.

Where it went — key, team, model, deployment, engine, upstream — is on the access line under
`rid`, not in the id. So is how it ended, as `cut`: `-` for a response that finished, else who ended it —
`client_left`, `upstream_idle` (silent for the read timeout) or `upstream` (its body broke off).
An upstream that ends a stream early but cleanly cannot be told from one that finished, and reads
`-`. On a provider route the vendor's own id is `upstream_rid` on that line and never reaches
the client; ours never reaches the vendor. `attempts` is how many times an upstream was dialled
for the request — 0 when it was refused before any, more than 1 when `retry` moved it to another
vendor key or `fallback` to another model. `model` is the one the client asked for; `fallback` is
the model that served instead, `-` when the one asked for did. `served_model` is the id the
upstream answered under when that is not the one the client knows the serving model by — a dated
snapshot, a vendor's own spelling of an alias — read off its usage line; `-` when it is the same,
or the answer named none.

What the vendor said of the hop in its own headers is on the line too, and nowhere else — the
proxy strips every upstream header a client has no use for before the response goes on.
`upstream_ms` is the hop as OpenAI timed it (`openai-processing-ms`); the rest of `ttft` is the
wire and the gateway. `upstream_attempts` is how many times Baseten ran the request on its side
(`X-Baseten-Model-Prediction-Attempts`); more than 1 is where a slow `ttft` went. On a refusal,
`retry_after` is how long the vendor asked for, and `remaining_requests` / `remaining_tokens`
which limit ran out (`x-ratelimit-remaining-*`, `anthropic-ratelimit-*-remaining`). `0` or `-`
from an engine, and from a vendor that sends none.

Where the time went is on the line as two groups, neither of which reaches the client. `stage` is
each stage's own time in seconds, from when the request reached it to when it reached the next,
`handler` being the proxy at the bottom: the capacity wait is `stage.route`, and `stage.retry` /
`stage.fallback` hold the attempts those stages discarded, each of which has an attempt line of
its own. `hop` is the upstream hop as `net/http` traced it: `reused` (a pooled connection),
`connect` and `tls` (only when one was dialled), `send` (our request going out), `wait` (the
upstream's silence before its first byte — what `upstream_ms` is the vendor's own account of),
`respond` (ours, from the upstream's first byte to the client's), `stream` (from the upstream's
first byte to its last). A leg that did not happen is absent; `hop` itself is absent when nothing
was dialled.

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
| `upstream`, `deployment`, `engine`, `served_model`, `upstream_rid`, `upstream_status`, `upstream_ms`, `upstream_attempts`, `retry_after`, `remaining_requests`, `remaining_tokens`, `reason`, `cut` | as on the access line, for this attempt; `reason` is the pick's refusal when nothing was dialled, `upstream unavailable` or `upstream timed out` when the dial gave no status |
| `upstream_key` | id of the vendor credential dialled; `-` on an engine |
| `stage`, `hop` | as on the access line, for this attempt; `hop.respond` absent, the client never received this answer |
| `rt` | seconds this attempt took |
| `moved`, `to` | what the request moved to: `key` and the next credential's id, or `model` and the fallback |

A request that dialled three times is two `attempt` lines and one `access` line. A fallback passed
over without a dial (not granted, no route on this surface, takes less than the request carries)
is in the process log, `fallback skipped`.

## Add a middleware

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

## Offload a decision to another service

A middleware, not a new abstraction: `offload("route")` above `route`, POSTing the decision inputs
under a deadline. On 200 it puts the answer in the state and `route` no-ops; on timeout or error it
logs and falls through. Fallback is chain order.

**The invariant if you build this: the remote decides, the gateway always claims.** A slow-but-alive
remote that answers after you gave up would otherwise claim a slot nothing ever releases, and that
engine leaves rotation permanently.

## Before changing something

- **Anything that claims an in-flight slot must release it.** `meter` does it in a `defer` for
  exactly this reason. One owner for that counter, always.
