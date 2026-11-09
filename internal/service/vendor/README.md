# The client interface

Clients speak one of two shapes, and a vendor is only ever reached on one of two paths:
`/openai/v1/chat/completions` (OpenAI) and `/anthropic/v1/messages` (Anthropic). Nothing converts between
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
| `stream_options` (OpenAI shape) | `include_usage: true` added on a stream (`streamusage`). `continuous_usage_stats` is added for Baseten and for an engine of ours, not for OpenAI (400s on it) or DeepSeek (ignores it) |
| a tool's `type` (Anthropic shape) | `"type": "custom"` removed at DeepSeek, which refuses it; a tool with no type means the same. Any other type stays (`vendorfields`) |
| `speed` (Anthropic shape) | removed for every vendor: fast mode is twice the rate and nothing here prices it. The caller is told: `X-Grove-Changed: speed=default` (`vendorfields`) |
| a `developer` message (OpenAI shape) | sent as a `system` message at DeepSeek, which knows no such role (`vendorfields`) |
| a past tool-call turn that came back without its reasoning | at DeepSeek an empty one is added: `reasoning_content: ""` on the OpenAI shape, an empty thinking block on the Anthropic one. Not when the caller disabled `thinking` (`vendorfields`) |
| `thinking`, when a tool is forced | at DeepSeek `{"type": "disabled"}` is added when `tool_choice` is `"required"` or names a function (`tool` on the Anthropic shape) and the caller sent no `thinking` or `reasoning_effort`. The caller is told: `X-Grove-Changed: thinking=disabled` (`vendorfields`) |
| `reasoning_effort`, when the request has tools (OpenAI shape) | at OpenAI `"none"` is added when the caller sent no `reasoning_effort`. The caller is told: `X-Grove-Changed: reasoning_effort=none` (`vendorfields`) |
| `temperature`, `top_p`, `logprobs`, `top_logprobs` (OpenAI shape) | at OpenAI, unless `reasoning_effort` is `"none"`: dropped, but for a `temperature` or `top_p` of 1. The caller is told: `X-Grove-Changed: temperature=default, top_p=default` (`vendorfields`) |
| `service_tier` | dropped: it picks the vendor's price class, which nothing here prices. The caller is told: `X-Grove-Changed: service_tier=default` (`servicetier`) |
| `prompt_cache_key` (OpenAI shape) | at OpenAI the tenant, or `<team>:<value>` when the caller sent one: a key's cache accounting is the team's, and no team can land in another's (`cachekey`, from the type's `CacheKey`) |
| `user_id` (OpenAI shape), `metadata.user_id` (Anthropic shape) | at DeepSeek the tenant, or `<team>:<value>` when the caller sent one: its KV cache isolation is per team (`cachekey`, from the type's `CacheKey` and `MetadataCacheKey`) |
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
  [Fallback models](../routing/README.md#fallback-models)).
- **Where an upstream differs is one type per upstream**, in `internal/service/vendor/`: a file
  of its name (`openai.go`, `deepseek.go`, `baseten.go`, `anthropic.go`, `morph.go`; `engine.go` for what we
  run), registered in `known` under the name the control plane pushes; a push naming a vendor
  not in it is a 400. The `Vendor` interface in
  `vendor.go` is everything one may differ in, and `base` is each as the shape says; a vendor's
  type overrides only what it does differently. One method per rewrite: the name the output cap
  goes under (`NameOutputCap`), whether a stream is asked for usage on every chunk
  (`UsagePerChunk`), four things DeepSeek refuses — a tool typed `"custom"` (`UntypeCustomTools`),
  a `developer` message (`DeveloperAsSystem`), a past tool call with no reasoning
  (`BlankReasoningContent`, `BlankThinkingBlock`) and a forced tool while thinking
  (`UnthinkForcedFunction`, `UnthinkForcedTool`) — and two of OpenAI's: a function tool while the
  model reasons (`UnreasonWithTools`) and sampling of the caller's while it reasons
  (`DefaultSampling`). Then the credential's headers (`Headers`: Baseten's Anthropic front reads
  a Bearer, set by `upstreamauth`), and the answer's side: what its headers say of the hop
  (`Answered`: Baseten's own request id and retries), which refusals are read to the end before
  they are classed and what they mean (`Peeks`, `Classify`), and a rewrite of the answer itself
  (`Relay`, which no vendor needs yet). Where it partitions its prompt cache is the type's too
  (`CacheKey`, `MetadataCacheKey`: an engine's `cache_salt`, OpenAI's `prompt_cache_key`,
  DeepSeek's `user_id`), set to the tenant by `cachesalt` and `cachekey` when `isolate_caches` is
  on — ours, not a quirk, so no row switches it off. [vendor_quirks.md](vendor_quirks.md) shows each rewrite
  with what the caller sent and what the vendor gets. `vendorfields` and `streamusage` run the
  rewrites per attempt on the client's own body, so a request that moves to another vendor is
  written afresh for that one. A model of a vendor that does not share one of its quirks is the
  control plane's to name: `skip_quirks` on the route row lists the rewrite by its method in
  snake case, and that model is not given it. Each type names the quirks it has (`Quirks`), which
  is what there is to switch off for its models; `vendor.QuirkNames()` is every name, and a push
  naming another is a 400. Add a method when the access log's `attempt` line, or a probe, shows a
  vendor differing — and the row below with it.
- Of an upstream's response headers only a named few reach the client (see [Headers](../../transport/http/README.md#headers)).
- Responses are not rewritten: same shape in, same shape out, with `model` swapped back to the name
  the client knows — whatever the vendor wrote there, since one asked by an alias answers under the
  name behind it. The one exception is a refusal of our own credential (401/402/403, or an account
  the vendor reports exhausted under a 400 or 429): that answer is the gateway's, under `retry` in [The records themselves](../../repository/redis/README.md#the-records-themselves).

What each upstream was seen to do, through a gateway, and where the gateway acts on it. This is the
one list of it: add a row or a column when a new difference shows up, with the date it was measured.

| | Engines we run (vLLM) | OpenAI | DeepSeek | Baseten | Morph | Handled by |
|---|---|---|---|---|------|
| Output cap field (OpenAI shape) | either name | `max_completion_tokens` only; 400 on `max_tokens` | `max_tokens` (2026-10-04: capped at 16) | `max_tokens` (2026-10-05: capped at 16) | not checked | `vendorfields`, from `vendor` |
| `stream_options.continuous_usage_stats` | taken | 400 (2026-10-05) | ignored (2026-10-05) | taken (2026-10-05) | not checked | `streamusage`, from `vendor`: sent to our engines and Baseten |
| Usage on every chunk of a stream | yes, when asked | no, the last chunk only | no, the last chunk only | yes, asked or not | not checked | asked all the same |
| A stream that is cut is metered | yes, by the last whole chunk | no; OpenAI bills nothing for it either (2026-09-30) | no | yes, by the last chunk received (2026-10-05: 44 + 33 tokens billed) | not checked | the usage tee |
| `model` in the answer | the id it was started under | the id it was asked by | may differ: `deepseek-v4-flash` answers as `deepseek-flash` (2026-10-04) | not checked | not checked | the response swap writes the client's id whatever came back |
| Anthropic shape | yes | no | yes, its own front | yes; it reads the key as a Bearer, and answers 401 `please check the api-key you provided` to `x-api-key` (2026-10-05) | yes, both shapes; `x-api-key` or a Bearer (2026-11-05) | the route's `dialect`; `upstreamauth` sends Baseten a Bearer, from `vendor` |
| A tool typed `"custom"` (Anthropic shape; Anthropic's API reference takes it, and litellm always sends it) | not checked | no such shape | 422 ``unknown variant `custom`, expected `web_search_20250305` or `web_search_20260209` ``; the same tool with no type is a 200 with a `tool_use` (2026-10-05) | taken (2026-10-05) | not checked | `vendorfields`, from `vendor`: DeepSeek gets the tool without the type |
| A server tool other than web search (Anthropic shape) | not checked | no such shape | 422 `unknown variant` for bash, text editor, web fetch and code execution (2026-10-05) | taken and not run: the answer is prose (2026-10-05) | not run (2026-11-05) | the route's `denied_tools`, at `route`: a tool the control plane lists for the vendor is a 400 before the dial; the rest the vendor refuses |
| A forced tool: `tool_choice` `"required"` or a named function, `tool` on the Anthropic shape | not checked | not checked | 400 `Thinking mode does not support this tool_choice` while thinking is on, its default; taken with `thinking` disabled, and `any` on the Anthropic shape is taken (2026-10-05) | taken on both shapes (2026-10-05) | not checked | `vendorfields`, from `vendor`: DeepSeek gets `thinking` disabled, unless the caller spoke of thinking |
| A past tool-call turn sent back without its reasoning | not checked | not checked | 400 ``The `reasoning_content` in the thinking mode must be passed back to the API``, and ``The `content[].thinking` …`` on the Anthropic shape; taken with it, with it empty, or with `thinking` disabled (2026-10-05) | taken on both shapes; a thinking block with no `signature` is a 400 (2026-10-05) | not checked | `vendorfields`, from `vendor`: DeepSeek gets an empty one |
| A function tool while the model reasons (chat) | not checked | 400 `Function tools with reasoning_effort are not supported … use /v1/responses or set reasoning_effort to 'none'` on luna and sol; taken with `reasoning_effort: "none"` (2026-10-05) | taken (2026-10-05) | taken (2026-10-05) | not checked | `vendorfields`, from `vendor`: OpenAI gets `reasoning_effort: "none"` when the caller set none |
| A `temperature` or `top_p` other than 1, or `logprobs` (chat) | not checked | while the model reasons, its default: 400 `'temperature' does not support 0.7 with this model. Only the default (1) value is supported.`, 400 `'top_p' is not supported with this model` and the same for `logprobs`, on luna and sol; 1 is taken for the first two, and all of them with `reasoning_effort: "none"` (2026-10-06) | not checked | not checked | not checked | `vendorfields`, from `vendor`: OpenAI gets none of them unless reasoning is off |
| `speed: "fast"` (Anthropic shape) | not checked | no such shape | not checked | not checked | not checked | `vendorfields`: dropped for every vendor, the caller told `speed=default`; ours to refuse to pay, not a quirk |
| A `developer` message | not checked | followed (2026-10-05) | 422 ``unknown variant `developer`, expected one of `system`, `user`, `assistant`, `tool`, `latest_reminder` `` (2026-10-05) | followed (2026-10-05) | not checked | `vendorfields`, from `vendor`: DeepSeek gets it as `system` |
| A past tool call with no `type` | not checked | 400 `Missing required parameter` (2026-10-05) | 422 ``missing field `type` `` (2026-10-05) | taken (2026-10-05) | not checked | not handled: the vendor refuses |
| Fields only OpenAI reads: `prediction`, `prompt_cache_key`, `prompt_cache_retention`, `verbosity`, `store`, `web_search_options` | not checked | its own | all six ignored (2026-10-05) | 400 `Extra inputs are not permitted` on `prediction` and `web_search_options`; the other four ignored (2026-10-05) | not checked | `web_search_options` is a 400 before the dial where the route's `denied_tools` lists it (OpenAI's does); `prompt_cache_key` is set to the tenant by `cachekey`; the rest not handled |
| The account behind our key is out of credit | n/a | 429 `insufficient_quota` | 402 `Insufficient Balance` | not checked | not checked | `retry`, from the vendor type's `Classify` (`domain.FailureClass`): read as the `quota` it is (Anthropic's is a 400 `credit balance`, read the same), the key retires, and the client gets the gateway's 502 `upstream unavailable`, which says nothing of our account; the vendor's words go to the error log. From the vendors' docs (2026-10-12), not yet measured through a gateway |
| A remote image URL | not checked | not checked | downloads it itself. A Pexels photo was a 400 `unsupported image` 9 times in 12: Pexels serves AVIF to an `Accept` that offers it and JPEG otherwise, and DeepSeek takes JPEG, PNG, GIF and WebP only, judged by the file's bytes ([its guide](https://api-docs.deepseek.com/guides/vision)). With `fm=jpg` in the URL, 8 of 8 were taken. A Wikimedia PNG is a 400 `Failed to download image`. The image as base64 is taken. Only flash reads images: pro answers 200 and says it cannot see one (2026-10-05) | the Pexels photo is taken, the Wikimedia PNG a 500; base64 is taken (2026-10-05) | not checked | not handled: the gateway downloads nothing |
| A `role: system` message inside `messages` with `tool_addition` / `tool_removal` blocks next to its text (Anthropic shape; Claude Code 2.1 adds a tool mid-conversation this way) | not checked | no such shape | not checked | not checked | 400 `system blocks must be of type 'text'` (2026-11-09); 200 once the blocks are removed and the text stays | `vendorfields`, from `vendor`: Morph's `fold_tool_changes` adds each definition to `tools`, takes a removed one out, and keeps the message's text |
| `defer_loading` on a tool, and the `tool_search_tool_regex_20251119` server tool (Anthropic shape) | not checked | no such shape | not checked | not checked | accepted and ignored: a deferred tool is in the prompt anyway, the model lists it when asked which tools it has (2026-11-09); the search tool is accepted and not run | not handled: there is nothing to defer to, so `fold_tool_changes` adds the tool outright and Morph's prompt cache misses on that one request |

What Baseten bills for a stream that was cut has not been compared with its own usage report.
