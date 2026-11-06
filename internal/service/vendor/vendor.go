// Package vendor is what the gateway knows of each upstream it dials that the shape it speaks does
// not say: where the credential goes, which fields it reads differently, what its answer says of
// the hop, and what its refusals mean. One type per upstream, in the file of its name, by the name
// the control plane pushes; a push naming a vendor the gateway has not met is refused, and an
// engine we run gets Engine. What an upstream is reached at, speaks, and answers to is the route
// row, not this.
package vendor

import (
	"encoding/json"
	"io"
	"maps"
	"slices"

	"github.com/frappe/pathway/internal/domain"
)

// Vendor is everything the gateway knows of one upstream that the shape it speaks does not say.
// base is each of these as the shape says; a vendor embeds base and overrides what it does
// differently. Every method is on the interface so the whole of what a vendor may differ in is
// read here, and so a driver reaches the override: a base method called on base is base's.
type Vendor interface {
	// Headers carries the credential the way the vendor reads it, by the front's dialect. A blank
	// secret sends no credential header; an Anthropic front's version header goes regardless.
	Headers(dialect, secret string) map[string]string
	Chat
	Messages
	Cache
	// UsagePerChunk asks a stream for usage on every chunk (`continuous_usage_stats`), so one that
	// is cut is still billed for what it had generated.
	UsagePerChunk() bool
	// Quirks names each of the rewrites above this vendor has, by the names QuirkNames lists: what
	// the control plane offers to switch off per model. The rest are the shape's, and this
	// vendor's methods for them change nothing.
	Quirks() []string
	Response
}

// Chat is each rewrite on the OpenAI chat shape, one method per quirk. Each reports whether it
// changed the body, and leaves a body it cannot read to the upstream to refuse. Every value was
// measured through a gateway: README.md here, "What each upstream was seen to do", says when, and
// vendor_quirks.md shows each rewrite.
type Chat interface {
	// NameOutputCap moves the output cap under the one name the vendor reads.
	NameOutputCap(*Request) (bool, error)
	// DeveloperAsSystem sends a `developer` message as a `system` one.
	DeveloperAsSystem(*Request) (bool, error)
	// UnthinkForcedFunction switches thinking off when the caller forces a tool and said nothing
	// about thinking.
	UnthinkForcedFunction(*Request) (bool, error)
	// BlankReasoningContent gives a past tool-call turn that came back without its reasoning an
	// empty `reasoning_content`.
	BlankReasoningContent(*Request) (bool, error)
	// UnreasonWithTools switches reasoning off (`reasoning_effort: "none"`) when the request has
	// tools and the caller set no effort.
	UnreasonWithTools(*Request) (bool, error)
	// DefaultSampling drops `temperature`, `top_p`, `logprobs` and `top_logprobs` unless reasoning
	// is off.
	DefaultSampling(*Request) (bool, error)
}

// Messages is each rewrite on the Anthropic messages shape.
type Messages interface {
	// UntypeCustomTools sends a caller's own tool without `"type":"custom"`. A tool with no type
	// means the same.
	UntypeCustomTools(*Request) (bool, error)
	// UnthinkForcedTool is UnthinkForcedFunction on this shape, where only a named tool is forced.
	UnthinkForcedTool(*Request) (bool, error)
	// BlankThinkingBlock is BlankReasoningContent on this shape: an empty thinking block in front.
	BlankThinkingBlock(*Request) (bool, error)
}

// Cache is where an upstream partitions its prompt cache, one method per shape. Not a quirk: the
// partition is the tenant's, so there is no slot in chat or messages and no name a row switches
// off. Run by the cachekey and cachesalt transforms when isolate_caches is on, never by vendorfields.
type Cache interface {
	// CacheKey sets the field this upstream keys its prompt cache by, on the OpenAI chat shape, to
	// the tenant (Request.Team), or `<team>:<caller's value>` when the caller sent one. Nothing on
	// an upstream with no such field, or with no tenant to name.
	CacheKey(*Request) (bool, error)
	// MetadataCacheKey is CacheKey on the Anthropic messages shape, where the field sits inside
	// `metadata`.
	MetadataCacheKey(*Request) (bool, error)
}

// Response is what a vendor's answer says that the shape does not, and what is put right on it.
type Response interface {
	// Answered reads the vendor's own account of the hop off its headers, for the access line.
	Answered(status int, header Header) Answer
	// Peeks says which statuses are read to their end before they are classed: a refusal whose
	// meaning is in its body.
	Peeks(status int) bool
	// Classify names what an answer says went wrong, one of domain's failure classes, blank on an
	// answer that was no failure. body is the vendor's error as sent, nil when only the status is
	// known.
	Classify(status int, body []byte) string
	// Relay wraps the body on its way to the client, after usage was read off the raw bytes and
	// before the model id is swapped back. The body as it came when nothing differs.
	Relay(status int, stream bool, body io.ReadCloser) io.ReadCloser
}

// Header is what a response's headers answer. net/http's Header satisfies it; this package may not
// import net/http.
type Header interface{ Get(name string) string }

// Answer is one vendor's account of the hop, off its response headers. Blank from an engine, which
// runs without them. The fields are proxy.Outcome's, which embeds it.
type Answer struct {
	// UpstreamRID is the upstream's own request id: what a ticket to a vendor quotes.
	UpstreamRID string
	// UpstreamMS is the hop as the vendor timed it: what of ttft was the vendor's, the rest being
	// the wire and the gateway. 0 from a vendor that sends none.
	UpstreamMS int
	// UpstreamAttempts is how many times the vendor ran the request on its side: more than 1 is
	// where a slow ttft went. 0 from one that does not say.
	UpstreamAttempts int
	// RetryAfter is the vendor's Retry-After as sent, seconds, on a refusal that carried one.
	RetryAfter string
	// RemainingRequests and RemainingTokens are the vendor's rate-limit headroom when it refused:
	// which of the two ran out on a 429. Blank on an answer, where they would only say how close
	// the key is, on every line.
	RemainingRequests, RemainingTokens string
}

// Request is one attempt's body on its way to the upstream, and what the caller is told of what
// changed. json.RawMessage values: a rewrite that touches one field must not re-encode the rest.
type Request struct {
	Body map[string]json.RawMessage
	// Changed is each rewrite that alters what the model does, as `field=value`, for the caller to
	// be told in X-Grove-Changed. A rename is not one of these.
	Changed []string
	// Skip is each quirk this model is not given, by name, as the control plane lists them on the
	// route row (skip_quirks): a vendor's type says what the vendor does, and the row says which
	// of its models does otherwise.
	Skip []string
	// Team is the tenant the request is billed to: what a cache partition is keyed by (Cache).
	// Blank when nobody is.
	Team string
}

// Tell notes a change the caller is told of.
func (r *Request) Tell(change string) { r.Changed = append(r.Changed, change) }

// Skips reports whether the row switched the quirk named off for this model.
func (r *Request) Skips(name string) bool { return slices.Contains(r.Skip, name) }

// The two names of the output cap on the OpenAI chat shape. An upstream reads one of them.
const (
	MaxTokens           = "max_tokens"
	MaxCompletionTokens = "max_completion_tokens"
)

// quirk is one rewrite: the name the control plane switches it off by, its method in snake case,
// and the method itself.
type quirk struct {
	name string
	run  func(Vendor, *Request) (bool, error)
}

// The rewrites of each shape, in the one order they compose: thinking is switched off before a
// past turn is given blank reasoning, since an upstream that is not thinking asks for none;
// sampling is judged after the tools rule, so a request it switched reasoning off for keeps its
// own.
var (
	chat = []quirk{
		{"name_output_cap", Vendor.NameOutputCap},
		{"developer_as_system", Vendor.DeveloperAsSystem},
		{"unthink_forced_function", Vendor.UnthinkForcedFunction},
		{"blank_reasoning_content", Vendor.BlankReasoningContent},
		{"unreason_with_tools", Vendor.UnreasonWithTools},
		{"default_sampling", Vendor.DefaultSampling},
	}
	messages = []quirk{
		{"untype_custom_tools", Vendor.UntypeCustomTools},
		{"unthink_forced_tool", Vendor.UnthinkForcedTool},
		{"blank_thinking_block", Vendor.BlankThinkingBlock},
	}
)

// QuirkUsagePerChunk is the name of the one quirk that is not a rewrite of the body's own fields
// (UsagePerChunk), read by the streamusage transform, which the row may switch off the same way.
const QuirkUsagePerChunk = "usage_per_chunk"

// QuirkNames is every name a row may switch off, sorted: what a push is checked against. Which
// of them a vendor has is its own Quirks.
func QuirkNames() []string {
	names := []string{QuirkUsagePerChunk}
	for _, q := range slices.Concat(chat, messages) {
		names = append(names, q.name)
	}
	slices.Sort(names)
	return names
}

// IsQuirk reports whether the gateway knows the quirk named.
func IsQuirk(name string) bool { return slices.Contains(QuirkNames(), name) }

// RewriteChat runs every chat-shape rewrite of `v` the row did not switch off. Through the
// interface, so a vendor's override is the one run.
func RewriteChat(v Vendor, req *Request) (bool, error) { return run(v, req, chat) }

// RewriteMessages is RewriteChat on the Anthropic messages shape.
func RewriteMessages(v Vendor, req *Request) (bool, error) { return run(v, req, messages) }

// run reports whether any rewrite changed the body, and stops at the first error with what was
// changed so far.
func run(v Vendor, req *Request, quirks []quirk) (bool, error) {
	changed := false
	for _, q := range quirks {
		if req.Skips(q.name) {
			continue
		}
		did, err := q.run(v, req)
		if err != nil {
			return changed, err
		}
		changed = changed || did
	}
	return changed, nil
}

// Default is a third party the table does not name: the shape as documented, nothing a named
// vendor refuses.
var Default Vendor = base{}

// Engine is an upstream we run.
var Engine Vendor = engine{}

// known is each third party the gateway has met, by the provider's name as the control plane
// pushes it. One type per vendor, in the file of its name; what a type does not override is base.
var known = map[string]Vendor{
	"openai":    openai{},
	"deepseek":  deepseek{},
	"baseten":   baseten{},
	"anthropic": anthropic{},
	"morph":     morph{},
}

// Names is every vendor the gateway has met, sorted: what a push is checked against.
func Names() []string { return slices.Sorted(maps.Keys(known)) }

// IsVendor reports whether the gateway has met the vendor named.
func IsVendor(name string) bool { _, met := known[name]; return met }

// For is the vendor named, or Default for one the gateway has not met.
func For(name string) Vendor {
	if v, met := known[name]; met {
		return v
	}
	return Default
}

// Of is the upstream a route dials: Engine for anything we run, else the vendor the row names.
func Of(route domain.Route) Vendor {
	if !route.IsProvider() {
		return Engine
	}
	return For(route.Vendor)
}
