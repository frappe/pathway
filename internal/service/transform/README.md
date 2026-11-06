# Transforms

## Add a request transform

Body rewrites specifically. Same shape as [a middleware](../../transport/http/middleware/README.md#add-a-middleware), in `service/transform/`:

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
default list is `config.Defaults()`. Two are registered and off by default, the per-tenant cache
partitions `cachesalt` and `cachekey`: the `isolate_caches` tunable appends both.

**A dropped field is logged for you.** A transform that removes a field just deletes it; the chain
compares what the caller sent with what is left and writes one Warn per user and field —
`request field dropped user=… field=… transform=…` — since nothing upstream errors on a field that
is not there. The name only, never the value. Top-level fields; a rewritten one is not a drop.

| transform | what it does |
|---|---|
| `modelmap` | rewrites `model` to the route's `upstream_model` |
| `streamusage` | forces `stream_options.include_usage` on a streaming completion, and `continuous_usage_stats` beside it for an upstream the `vendor` package says takes it (our engines, Baseten) |
| `servicetier` | drops a caller's `service_tier` on every hop, and says so in `X-Grove-Changed`: it picks the vendor's price class, which is never the caller's to choose |
| `vendorfields` | runs each rewrite of the upstream's type in the `vendor` package, in that package's order (`vendor.RewriteChat` on `/v1/chat/completions`, `vendor.RewriteMessages` on `/v1/messages`), and drops `speed` on a vendor hop (see [The client interface](../vendor/README.md#the-client-interface) and [vendor_quirks.md](../vendor/vendor_quirks.md)). An engine we run gets the body as sent. A rename is logged as a drop of the old name |
| `cachesalt` | on an engine hop, sets the field the engine's type names (`vendor.Engine.CacheKey`: vLLM's and SGLang's `cache_salt`) to the tenant, the caller's own salt prefixed `<team>:` when one was sent; strips it from a vendor hop, whose partition is `cachekey`'s. On with `isolate_caches` |
| `cachekey` | on a vendor hop, sets the vendor's prompt-cache partition to the tenant, in the field its type names (`CacheKey`, `MetadataCacheKey` on `/v1/messages`: `prompt_cache_key` at OpenAI, `user_id` at DeepSeek, `metadata.user_id` on `/v1/messages`), the caller's own value prefixed `<team>:` when one was sent. Anthropic has no such field, Baseten's affinity is a header, an engine of ours is `cachesalt`'s. On with `isolate_caches` |
