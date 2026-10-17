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
default list is `config.Defaults()`.

**A dropped field is logged for you.** A transform that removes a field just deletes it; the chain
compares what the caller sent with what is left and writes one Warn per user and field —
`request field dropped user=… field=… transform=…` — since nothing upstream errors on a field that
is not there. The name only, never the value. Top-level fields; a rewritten one is not a drop.

| transform | what it does |
|---|---|
| `modelmap` | rewrites `model` to the route's `upstream_model` |
| `streamusage` | forces `stream_options.include_usage` on a streaming completion, and `continuous_usage_stats` beside it for an upstream the `vendor` package says takes it (our engines, Baseten) |
| `servicetier` | drops a caller's `service_tier` on every hop, and says so in `X-Grove-Changed`: it picks the vendor's price class, which is never the caller's to choose |
| `vendorfields` | on a vendor hop, sends the output cap under the one name that vendor reads, from the `vendor` package of where an upstream differs from the shape (see [The client interface](../vendor/README.md#the-client-interface)). Today the output cap: `max_completion_tokens` for OpenAI (400s on the old name), `max_tokens` for every other (they ignore the new one, uncapping output). Logged as a drop of the other name |
| `cachesalt` | prefixes a caller's `cache_salt` with their tenant, strips it on a vendor hop. Not in the default list |
