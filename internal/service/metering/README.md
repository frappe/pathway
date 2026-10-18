# Metering

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
unpriced route — then `HINCRBY key:<hash> spent:<YYYY-MM> cost` (the UTC month the request lands
in) and `HSET key_spent key_balance` (`budget − spent`, negative once overspent; last writer wins)
on the usage hash. The pull prices each `p:<pricing id>` group with that same pricing's table, so
the two sides can only disagree when they hold different rates for one pricing id. A drain therefore never sees a counter without its cost, or a cost without the spend it
moved. The spend moves only on a key the control plane has pushed.

## The spool

A request that finishes while the store is down (new ones already 503 at admission)
cannot land its usage. It is appended as one JSON line — `{"id": "<request id>", "prefix", "fields",
"cost", "user", "budget", "at"}` — to `usage_spool` and fsynced. At start and every 5 s while it is
non-empty the gateway PINGs the store; once it answers, each line is replayed through the accrue
script behind `SET accrued:<request id> NX EX 604800`, so a pass that dies half way lands nothing
twice, and the file is rewritten (tmp + rename) with only the lines still failing. A line that fails
five passes while the store answers is set aside in `<usage_spool>.dead` as `{"id", "line",
"error"}`; one that does not parse goes there at once. Past `usage_spool_max_bytes` (1 GiB) usage is
logged and dropped. A store still down counts no tries. Only losing the box's disk loses spooled
usage.
