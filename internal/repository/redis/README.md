# What Redis holds

Grove pushes state; the gateway projects it into local Redis and never calls back. **These shapes
are the interface — changing one means changing `agent_sync.py` and `usage_pull.py` too.**

| Key | Type | Written by |
|---|---|---|
| `key:<sha256(secret)>` | hash | state push, `keys` section; `spent:<YYYY-MM>` by the gateway |
| `team:<Grove Team>` | hash | state push, `teams` section |
| `model_group:<Model Group>` | hash | state push, `groups` section |
| `deploy:<model>` | JSON array of routes | state push, `routes` section |
| `grove:state_hash` | hash | state push — per-section/bucket fingerprints of what this box holds |
| `lim:<key prefix>:<metric>:<window>:<bucket>` | counter, kept two windows | the gateway — see [Rate limiting](../../service/admission/README.md#rate-limiting) |
| `usage:<key prefix>:<YYYY-MM-DD>` | hash, one per key per UTC day it was served | the gateway; set aside by `GET /grove-admin/usage` |
| `drained:<drain id>:<key prefix>:<YYYY-MM-DD>` | hash, kept `usage_retention` once acked | `GET /grove-admin/usage` renames a live counter here |
| `drain:unacked` | set of `<drain id>:<key prefix>:<YYYY-MM-DD>` | every counter set aside and not yet acked |
| `adjust:<id>` | string, 7 days | `POST /grove-admin/spend-adjust` — ids already applied |
| `accrued:<request id>` | string, 7 days | the spool's replay — requests already landed from it |
| `sticky:<session>` | string, 30m | the gateway |
| `inflight:<engine>` | sorted set, member = request id; never a vendor's URL | the gateway |
| `health:<target>` | counter, 60s | the gateway |
| `pk:<key id>` | hash, until taken | the gateway — what each vendor credential answered; read and deleted by `GET /grove-admin/provider-keys` |

Nothing the control plane pushes carries a TTL. The push is hash-gated, so a record that expired
would stay missing until its section changed; only gateway-owned keys expire
(`TestPushedStateNeverExpires`).

## The records themselves

```
key:<sha256(secret)>       status  team  prefix  group (comma list)  allow  deny  limited  log_payloads
                           geography  budget  budget_window  spent:<YYYY-MM>  spent:total  limits
team:<Grove Team>          limited  log_payloads  limits
model_group:<Model Group>  models
usage:<key prefix>:<day>   request_count  prompt_tokens  completion_tokens  total_tokens  cached_tokens
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
name containing a comma would split — Grove refuses one. `team` is the Grove Team the key bills
to: the cache-salt namespace, the payload log's join field, and the record whose `limited`,
`log_payloads` and default `limits` are applied onto the key at resolve. The same three
fields on the key record are what a control plane from before teams wrote, read only while the
store holds no team record.

`budget` / `spent:<YYYY-MM>` are the credit gate. `budget` is the key's cap, nano-USD, 0
until Grove sets one; Grove keeps Σ caps within the team's tier limit. `spent:<YYYY-MM>` is this
Redis's own counter for the UTC month: every metered request moves this month's field, a push
never does (the push is an HSET of the fields it names), and only `spend-adjust` corrects it. A new
month is a field not yet written, which reads 0, so nothing resets and nothing runs at midnight;
earlier months' fields stay behind (about 20 bytes a key a month) and nothing reads them.
`budget_window` picks the field: `month` (or blank) is the above; `total` lands and reads every
spend under the one field `spent:total`, which never turns over, so a key capped for life (a trial
key) stays spent once spent. The accrue and adjust scripts read the window off the record, so a
spooled line replayed after a push lands where the key now counts; a push naming a window this
binary cannot gate is refused whole (400). This
box's balance for the key is `budget − spent`; Grove keeps the team's own from the pulled usage and
audits the two every pull. `spent >= budget` → 402 `credit balance
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
when it lacks what the request carries (see [Fallback models](../../service/routing/README.md#fallback-models)). A row that declares nothing — one pushed before
the control plane said — is unrestricted and not judged. There is no `modality` field: a push
that carries one is refused like any unknown field.
`pricing` is the Model Pricing in force — its id and its sell price per counter, nano-USD per unit
(Mtok, minute, request) — stamped on every row of the model. Absent on an unpriced model, and such
a request costs 0.

A `kind: "provider"` row dials a third-party vendor instead of anything we run, and carries its
own fields. A model's rows are all provider rows or none; a mixed push is a 400:

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
is every row we run ourselves. A fallback whose rows deny the tool is passed over like one that lacks an input. Deleting
a Denied Tool row is how a tool is let through once it is priced. The list is a deny list, so a
tool a vendor adds tomorrow runs until its row exists.

`upstream_model` is what the `modelmap` transform puts in `body.model`; blank means send the
caller's unchanged, which is every route we run ourselves — an engine is started under the Grove
id. A realtime upgrade carries its model in the query string and has no body for a transform to
rewrite, so the same substitution is applied to `?model=` in the transform stage. Access, routing, metering and `/v1/models` all key on the id the caller sent, so the rewrite
cannot desync a grant from a route. On a provider hop the client's `Authorization` is deleted
rather than replaced, the vendor's own key goes in the header its `dialect` expects (`x-api-key`
plus `anthropic-version` for an Anthropic front, a Bearer for an OpenAI-compatible one and for an
Anthropic front whose `vendor.Headers` says it reads one), and the
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
request. A provider row's 400 or 429 is read to its end (64 KiB) before it is classed, because an
exhausted account hides under both — OpenAI's 429 `insufficient_quota`, Anthropic's 400 "credit
balance" — and reads as the 402 it is (`domain.ClassifyUpstream`). A second 429 exhaustion is the
client's answer, headers and body as the vendor sent them. A 401/402/403 the ring cannot move off —
every key refused, a single key, an engine's own — never is: the client gets the gateway's **502**
`api_error`, `upstream credential rejected` / `upstream account unpaid` /
`upstream credential lacks permission`, the access line says `reason="credential unauthorized|unpaid|forbidden"`
with the vendor's status in `upstream_status`, and the error log keeps the vendor's own words with
the key id (`upstream refused the credential`). Nothing else is retried: a 502 on one key is a 502
on the next. A held attempt leaks nothing to the client; the request is billed once whatever it
walked, and the access line's `attempts` counts the dials. Every attempt lands in `pk:<key id>`
(`requests`, `ok`, `rate_limited`, `unauthorized`, `unpaid`, `forbidden`, `rejected` — the three
before it together — `failed`, `last_used`, `last_rate_limited`) until
`GET /grove-admin/provider-keys?ids=a,b` takes it.

`dialect` is the API shape the row speaks, `openai` or `anthropic`, and nothing translates between
them: a request reaches only rows of its surface's dialect. A vendor with both fronts is two rows. On
a row we run, blank means both, since vLLM answers both natively; on a provider row, blank is
malformed and the row serves nothing.

A provider route is also **deny-by-default on the surface check**. `Serves` is generous to an engine
because an engine answers on more than the OpenAI core (`/tokenize`, `/v1/rerank`), but a vendor is a
closed set we already know, so `ServesRoute` holds it to its dialect's paths — `/v1/chat/completions`
for `openai`, `/v1/messages` for `anthropic`. Anything else is our 404 at `routing.Pick`, above
`meter`, instead of a round trip that comes back as theirs. The paths are one table,
`vendorPaths` in `domain/dialect.go`, path → dialect: serving another (`/v1/responses` for
`openai`, say) is a row there.

The surface is not read off the path. Each base (`/openai`, `/anthropic`) strips its prefix before
the chain runs, so below it `/openai/v1/chat/completions` and `/anthropic/v1/messages` are both
`/v1/…`; the base records the surface on the request as it strips (`respond.WithDialect`), and
everything below — `routing.Request.Dialect`, the refusal's shape, the proxy's own errors — reads
`respond.Dialect`. Unmarked is OpenAI, the shape the gateway's own root routes answer in. Root
serves no dialect: a data path there is a 404 naming the base it belongs under.

## Backwards compatibility that is still load-bearing

- A push strips the fields older control planes wrote on a key (`user`, `can_read_balance`,
  `models`, `priority`), so a record never shows the current policy beside a stale pointer. A
  control plane still sending a `users` section is refused 400 by name: the policy moved onto the
  key in one cut, and this binary must land on a box before that control plane pushes to it.
- A route with no `kind` is `direct`, which is what every route pushed before the split was.
- A route with no `upstream_model` sends the caller's `model` unchanged, which is what every route
  pushed before the vendor split did.

## Add a storage backend

Implement the interface in `repository/repository.go` and wire it in `cmd/pathway/main.go`.
Nothing in `service/` knows which one it got. For a second destination (say a local SQLite archive
of usage), a decorator holding two `repository.Usage` values is the whole change — the primary's
error is returned, the archive's is logged.
