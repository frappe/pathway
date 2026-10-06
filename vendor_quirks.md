# Vendor quirks the gateway fixes

A vendor sometimes refuses, or misreads, a request that is right for the shape it claims to speak.
Where the request can be rewritten without changing what the caller asked for, the gateway rewrites
it for that vendor only. This file is each such rewrite, with what the caller sent and what the
vendor gets.

- The table that turns a rewrite on is `vendors` in
  [`internal/service/transform/vendors.go`](internal/service/transform/vendors.go), one flag per
  quirk, keyed by the vendor's name.
- The code is the `vendorfields` and `streamusage` transforms in the same folder, and
  `upstreamauth` in `internal/transport/http/middleware/builtin.go` for the one that is a header.
- Every quirk here was measured through a gateway on the date given. What was seen and is *not*
  rewritten is in the README, "What each upstream was seen to do".
- A rewrite runs per attempt on the caller's own body, so a request that falls back to another
  vendor is written afresh for that one.
- A rewritten list (`tools`, `messages`) has its objects' keys in alphabetical order. Nothing inside
  them is touched.

| Quirk | Vendor | Flag | Shape |
|---|---|---|---|
| [The output cap's name](#1-the-output-caps-name) | OpenAI, and every other vendor the other way | `outputCap` | OpenAI |
| [Usage on every chunk](#2-usage-on-every-chunk-of-a-stream) | OpenAI refuses the option | `usagePerChunk` | OpenAI |
| [A tool typed `custom`](#3-a-tool-typed-custom) | DeepSeek | `untypedTools` | Anthropic |
| [A `developer` message](#4-a-developer-message) | DeepSeek | `developerAsSystem` | OpenAI |
| [A past tool call with no reasoning](#5-a-past-tool-call-with-no-reasoning) | DeepSeek | `blankReasoning` | both |
| [A forced tool while thinking](#6-a-forced-tool-while-thinking) | DeepSeek | `unthinkForcedTool` | both |
| [An Anthropic front that reads a Bearer](#7-an-anthropic-front-that-reads-a-bearer) | Baseten | `bearerOnAnthropic` | Anthropic |
| [A function tool while the model reasons](#8-a-function-tool-while-the-model-reasons) | OpenAI | `unreasonWithTools` | OpenAI |
| [A temperature while the model reasons](#9-a-temperature-while-the-model-reasons) | OpenAI | `defaultSampling` | OpenAI |

Three of these change what the model does: 6, 8 and 9. The caller is told of each on the answer,
in the `X-Grove-Changed` header. 6 and 8 are not applied when the caller said what they wanted. 9
is the one that overrides what the caller said.

---

## 1. The output cap's name

**What the vendor does.** OpenAI answers 400 to `max_tokens` and reads only
`max_completion_tokens`. DeepSeek and Baseten read `max_tokens`; sent the newer name alone, the
output is not capped by it. Measured 2026-10-04 and 2026-10-05.

**The rewrite.** The cap goes under the one name the vendor reads. Sent under both, the newer
name's value is the caller's word. An engine of ours reads either and gets the body as sent.

The caller sends, to an OpenAI model:

```json
{"model": "openai/gpt-6-luna", "max_tokens": 64, "messages": [{"role": "user", "content": "hi"}]}
```

OpenAI gets:

```json
{"model": "gpt-6-luna", "max_completion_tokens": 64, "messages": [{"role": "user", "content": "hi"}]}
```

The caller sends, to a DeepSeek model:

```json
{"model": "deepseek/deepseek-v4-flash", "max_tokens": 64, "max_completion_tokens": 8, "messages": [{"role": "user", "content": "hi"}]}
```

DeepSeek gets:

```json
{"model": "deepseek-v4-flash", "max_tokens": 8, "messages": [{"role": "user", "content": "hi"}]}
```

---

## 2. Usage on every chunk of a stream

**What the vendor does.** A stream that is cut has no last chunk, and the last chunk is where
usage normally comes. `stream_options.continuous_usage_stats` asks for the running count on every
chunk. Our engines and Baseten take it. OpenAI answers 400 to it. DeepSeek ignores it. Measured
2026-10-05.

**The rewrite.** `include_usage` is added to every stream. `continuous_usage_stats` is added only
where it is taken.

The caller sends:

```json
{"model": "baseten/deepseek-v4.1-flash", "stream": true, "messages": [{"role": "user", "content": "hi"}]}
```

Baseten gets:

```json
{"model": "deepseek-ai/DeepSeek-V4.1-Flash", "stream": true, "stream_options": {"include_usage": true, "continuous_usage_stats": true}, "messages": [{"role": "user", "content": "hi"}]}
```

OpenAI and DeepSeek get `"stream_options": {"include_usage": true}`.

---

## 3. A tool typed `custom`

**What the vendor does.** DeepSeek's Anthropic front answers 422 to a tool that carries
`"type": "custom"`:

```
Failed to deserialize the JSON body into the target type: tools[0]: unknown variant `custom`,
expected `web_search_20250305` or `web_search_20260209`
```

Anthropic's API takes that type: it is the written-out name of a caller's own tool, and a tool with
no `type` means the same. litellm always writes it. Measured 2026-10-05.

**The rewrite.** `"type": "custom"` is removed from each tool. Any other type is a tool the vendor
runs, and stays.

The caller sends, on `/anthropic/v1/messages`:

```json
{
  "model": "deepseek/deepseek-v4-flash",
  "max_tokens": 256,
  "messages": [{"role": "user", "content": "What is the weather in Paris?"}],
  "tools": [
    {"type": "custom", "name": "get_weather", "description": "Get the weather for a city",
     "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}},
    {"type": "web_search_20250305", "name": "web_search", "max_uses": 1}
  ]
}
```

DeepSeek gets these tools:

```json
[
  {"description": "Get the weather for a city",
   "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}, "name": "get_weather"},
  {"max_uses": 1, "name": "web_search", "type": "web_search_20250305"}
]
```

---

## 4. A `developer` message

**What the vendor does.** DeepSeek answers 422 to a message whose role is `developer`:

```
Failed to deserialize the JSON body into the target type: messages[0].role: unknown variant
`developer`, expected one of `system`, `user`, `assistant`, `tool`, `latest_reminder`
```

OpenAI and Baseten follow such a message. Newer OpenAI SDKs write `developer` where older ones
wrote `system`. Measured 2026-10-05.

**The rewrite.** The role becomes `system`.

The caller sends:

```json
{
  "model": "deepseek/deepseek-v4-flash",
  "messages": [
    {"role": "developer", "content": "Answer in one word."},
    {"role": "user", "content": "What is the capital of France?"}
  ]
}
```

DeepSeek gets these messages:

```json
[
  {"content": "Answer in one word.", "role": "system"},
  {"content": "What is the capital of France?", "role": "user"}
]
```

---

## 5. A past tool call with no reasoning

**What the vendor does.** DeepSeek thinks by default, and returns its reasoning beside a tool
call. When that turn is sent back in the history without the reasoning, DeepSeek answers 400:

```
The `reasoning_content` in the thinking mode must be passed back to the API.
```

and on its Anthropic front:

```
The `content[].thinking` in the thinking mode must be passed back to the API.
```

Most clients do not send reasoning back, so the second turn of a tool conversation fails. A plain
past turn needs no reasoning. A tool-call turn with *empty* reasoning is taken, and DeepSeek goes on
thinking. Baseten asks for none. Measured 2026-10-05.

**The rewrite.** A past assistant turn that called a tool and has no reasoning gets an empty one.
A turn that has its reasoning is left alone. Nothing is added when the caller disabled `thinking`:
DeepSeek asks for no reasoning then.

On the OpenAI shape, the caller sends:

```json
{
  "model": "deepseek/deepseek-v4-flash",
  "messages": [
    {"role": "user", "content": "What is the weather in Paris?"},
    {"role": "assistant", "content": "",
     "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"Paris\"}"}}]},
    {"role": "tool", "tool_call_id": "call_1", "content": "18C and sunny"}
  ]
}
```

DeepSeek gets the assistant turn as:

```json
{"content": "", "reasoning_content": "", "role": "assistant",
 "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\": \"Paris\"}"}}]}
```

On the Anthropic shape, the caller sends:

```json
{
  "model": "deepseek/deepseek-v4-flash",
  "max_tokens": 256,
  "messages": [
    {"role": "user", "content": "What is the weather in Paris?"},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "Paris"}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": "18C and sunny"}]}
  ]
}
```

DeepSeek gets the assistant turn as:

```json
{"content": [
   {"type": "thinking", "thinking": "", "signature": ""},
   {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "Paris"}}
 ], "role": "assistant"}
```

---

## 6. A forced tool while thinking

**What the vendor does.** DeepSeek refuses a forced tool while it thinks, and it thinks by
default:

```
Thinking mode does not support this tool_choice
```

On the OpenAI shape that is `tool_choice` `"required"` or one naming a function. On the Anthropic
shape it is `{"type": "tool", …}`; `{"type": "any"}` is taken. With thinking disabled each is
answered with the tool call. Baseten takes all of them. Measured 2026-10-05.

**The rewrite.** `thinking` is disabled for that request. This changes what the model does: the
tool is called, without reasoning first. So it is applied only when the caller said nothing about
thinking, and the answer says so:

```
X-Grove-Changed: thinking=disabled
```

A caller who sent `thinking` (or `reasoning_effort` on the OpenAI shape) and a forced tool asked
for both, and gets DeepSeek's refusal.

The caller sends:

```json
{
  "model": "deepseek/deepseek-v4-flash",
  "messages": [{"role": "user", "content": "What is the weather in Paris?"}],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}}],
  "tool_choice": "required"
}
```

DeepSeek gets the same body with one field more:

```json
{"thinking": {"type": "disabled"}}
```

On the Anthropic shape, the caller sends `"tool_choice": {"type": "tool", "name": "get_weather"}`
and DeepSeek gets the same field added.

Sent as is, and refused by DeepSeek:

```json
{"tool_choice": "required", "thinking": {"type": "enabled"}}
```

---

## 7. An Anthropic front that reads a Bearer

**What the vendor does.** Anthropic's API reads the key from the `x-api-key` header, and so does
DeepSeek's Anthropic front. Baseten's Anthropic front answers 401 to it:

```json
{"error": "please check the api-key you provided"}
```

It reads the key as `Authorization: Bearer`, the header its OpenAI front takes. The message reads
as if the caller's key were wrong; it is the vendor's key in a header it does not read. Measured
2026-10-05, at Baseten directly and through a gateway.

**The rewrite.** A header, not a field: the vendor's key goes out as a Bearer. `anthropic-version`
is sent as for any Anthropic front.

The caller sends, on `/anthropic/v1/messages`:

```
x-api-key: <the caller's gateway key>
anthropic-version: 2023-06-01
```

Baseten gets:

```
Authorization: Bearer <Baseten's key>
anthropic-version: 2023-06-01
```

DeepSeek and Anthropic get `x-api-key: <the vendor's key>`.

---

## 8. A function tool while the model reasons

**What the vendor does.** OpenAI's luna and sol reason by default, and on chat completions they
refuse a function tool while they do:

```
Function tools with reasoning_effort are not supported for gpt-6-luna in /v1/chat/completions.
To use function tools, use /v1/responses or set reasoning_effort to 'none'.
```

So a plain tool request, with no word about reasoning, is a 400. With `reasoning_effort: "none"`
it is answered with the tool call. DeepSeek and Baseten take tools while thinking. Measured
2026-10-05.

**The rewrite.** `reasoning_effort: "none"` is added when the request has tools and the caller
sent no `reasoning_effort`. This changes what the model does: it answers without reasoning. The
answer says so:

```
X-Grove-Changed: reasoning_effort=none
```

A caller who set an effort and sent tools asked for both, and gets OpenAI's refusal. The way to
have both is OpenAI's Responses API, which the gateway does not serve.

The caller sends:

```json
{
  "model": "openai/gpt-6-luna",
  "messages": [{"role": "user", "content": "What is the weather in Paris?"}],
  "tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}}]
}
```

OpenAI gets the same body with one field more:

```json
{"reasoning_effort": "none"}
```

Sent as is, and refused by OpenAI:

```json
{"reasoning_effort": "high", "tools": [{"type": "function", "function": {"name": "get_weather"}}]}
```

It is applied to every OpenAI model, so each one routed must take `"none"`. Luna and sol do.

---

## 9. A temperature while the model reasons

**What the vendor does.** OpenAI's luna and sol reason by default, and while they do they take no
sampling of the caller's:

```
Unsupported value: 'temperature' does not support 0.7 with this model. Only the default (1) value is supported.
Unsupported parameter: 'top_p' is not supported with this model.
Unsupported parameter: 'logprobs' is not supported with this model.
```

A temperature of 0, 0.7 or 1.5 is a 400, and so is a `top_p` of 0.5, and `logprobs`. A temperature
or `top_p` of 1 is taken. With `reasoning_effort: "none"` every one of them is taken. Many clients
send a temperature on every request, so they cannot use these models at all. Measured 2026-10-06.

**The rewrite.** Unless reasoning is off, `temperature`, `top_p`, `logprobs` and `top_logprobs` are
dropped (a `temperature` or `top_p` of 1 is left). It is what Bifrost does. Unlike 6 and 8, this
overrides something the caller asked for: the other way to make the request work would be to
switch the reasoning off, and the reasoning is what these models are for. The answer names each
field that was dropped:

```
X-Grove-Changed: temperature=default, top_p=default
```

Reasoning is off, and the caller's sampling goes as sent, when the caller sent
`reasoning_effort: "none"`, or when the request has tools and no effort (quirk 8 switched it off).

The caller sends:

```json
{"model": "openai/gpt-6-sol", "temperature": 0.2, "top_p": 0.9, "messages": [{"role": "user", "content": "hi"}]}
```

OpenAI gets:

```json
{"model": "gpt-6-sol", "messages": [{"role": "user", "content": "hi"}]}
```

Sent as is, and taken:

```json
{"model": "openai/gpt-6-sol", "temperature": 0.2, "reasoning_effort": "none", "messages": [{"role": "user", "content": "hi"}]}
```

It is applied to every OpenAI model, so each one routed must reason by default, as luna and sol do.
An OpenAI model that does not would have its temperature taken from it for nothing: it needs a
per-model switch before it is routed.

---

## Adding one

1. Measure it through a gateway: the vendor's status and words, and the request that is taken
   instead. A rewrite goes in only if that request means what the caller's did.
2. Add a row to the README's "What each upstream was seen to do", with the date.
3. Add a flag to `upstream` in `vendors.go`, written out on every entry, and the rewrite beside the
   others in `vendorfields.go` with a test.
4. Add a section here: what the vendor does, the rewrite, what is sent and what the vendor gets.
