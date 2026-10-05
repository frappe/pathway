// Package transform rewrites a request body on its way to an engine. Each rule is registered by
// name and declares which endpoints it applies to, so adding one is a new file and a name in the
// configured list — never an edit to a shared endpoint check.
package transform

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

// Body is a decoded request body. json.RawMessage values, not `any`: a transform that touches one
// field must not re-encode the rest, which is how a large float loses precision on the way past.
type Body map[string]json.RawMessage

// Context is what a transform may know about the request beyond its body.
type Context struct {
	Path string
	// UpstreamModel is what the chosen route says this upstream answers to, pushed by the control
	// plane. Blank means send the caller's `model` unchanged, which is every route we run ourselves.
	UpstreamModel string
	// User is the Grove user this request is billed to — the tenant boundary cachesalt keys on.
	User string
	// Provider is true when the hop leaves our network for a vendor.
	Provider bool
	// Vendor is the provider's name on a vendor hop ("openai"), blank on anything we run.
	Vendor string
}

// Request is one body rewrite. Apply reports whether it changed anything, so a body that no
// transform touched is forwarded byte-for-byte rather than re-encoded for nothing.
type Request interface {
	Name() string
	// Endpoints is the exact paths this applies to. Empty means every path — deliberately explicit,
	// because a field one vLLM schema accepts another rejects.
	Endpoints() []string
	Apply(Context, Body) (bool, error)
}

var (
	mu       sync.RWMutex
	registry = map[string]Request{}
)

// Register adds a transform under its name. Called from init, so a file's presence in the build is
// what makes it available and the configured list is what makes it run.
func Register(t Request) {
	mu.Lock()
	defer mu.Unlock()
	registry[t.Name()] = t
}

// Registered is every transform the binary knows about, sorted. For the startup log, so an operator
// can see what a name in the configured list could have meant.
func Registered() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Default is what runs when nothing is configured. Order matters only in that a later transform
// sees an earlier one's output.
var Default = []string{"modelmap", "streamusage", "cachesalt", "servicetier", "vendorfields"}

// Chain is an ordered, resolved set of transforms, replaceable in place under its own lock — the
// middleware holds a pointer, and rebuilding the whole chain to change which rewrites run would be
// a much larger swap for a much smaller change.
type Chain struct {
	mu         sync.RWMutex
	transforms []Request
	// warned is each user and field already logged as dropped: an SDK that sends one on every
	// request is a line, not a flood.
	warned sync.Map
}

// NewChain resolves names to transforms. An unknown name is a startup error, not a warning: a
// misspelt one would silently stop rewriting bodies with nothing downstream looking wrong.
func NewChain(names []string) (*Chain, error) {
	mu.RLock()
	defer mu.RUnlock()
	resolved, err := resolveLocked(names)
	if err != nil {
		return nil, err
	}
	return &Chain{transforms: resolved}, nil
}

// Replace swaps what this chain runs. On error nothing moves — the running set is kept, which is
// what makes a misspelt name in the tunables file a log line rather than a body that silently stops
// being rewritten.
func (c *Chain) Replace(names []string) error {
	mu.RLock()
	resolved, err := resolveLocked(names)
	mu.RUnlock()
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.transforms = resolved
	c.mu.Unlock()
	return nil
}

// resolveLocked maps names to transforms. Caller holds the registry lock.
func resolveLocked(names []string) ([]Request, error) {
	resolved := make([]Request, 0, len(names))
	for _, name := range names {
		t, ok := registry[name]
		if !ok {
			return nil, fmt.Errorf("unknown request transform %q; registered: %v", name, sortedLocked())
		}
		resolved = append(resolved, t)
	}
	return resolved, nil
}

// Apply runs every transform whose endpoints match, and reports whether the body changed. A field
// the caller sent that a transform removed is logged here, so a transform only ever deletes.
func (c *Chain) Apply(ctx Context, body Body) (bool, error) {
	c.mu.RLock()
	transforms := c.transforms
	c.mu.RUnlock()

	sent := make([]string, 0, len(body))
	for field := range body {
		sent = append(sent, field)
	}
	changed := false
	for _, t := range transforms {
		if !applies(t, ctx.Path) {
			continue
		}
		did, err := t.Apply(ctx, body)
		if err != nil {
			return changed, fmt.Errorf("transform %s: %w", t.Name(), err)
		}
		if did {
			c.warnDropped(ctx.User, t.Name(), sent, body)
		}
		changed = changed || did
	}
	return changed, nil
}

// warnDropped logs each field sent and no longer there, once per user and field: nothing upstream
// errors on a missing field, so this line is the only sign the caller asked for it. The name
// only, never the value — that is the caller's, and a cache salt is a secret.
// ponytail: top-level fields only, and the seen set is bounded because transforms name what they
// drop; one that strips whatever it does not know needs a cap here and a diff that recurses.
func (c *Chain) warnDropped(user, transform string, sent []string, body Body) {
	for _, field := range sent {
		if _, kept := body[field]; kept {
			continue
		}
		if _, seen := c.warned.LoadOrStore(user+"\x00"+field, true); !seen {
			slog.Warn("request field dropped", "user", user, "field", field, "transform", transform)
		}
	}
}

// Names is what this chain will run, for the startup log.
func (c *Chain) Names() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	names := make([]string, 0, len(c.transforms))
	for _, t := range c.transforms {
		names = append(names, t.Name())
	}
	return names
}

func applies(t Request, path string) bool {
	endpoints := t.Endpoints()
	if len(endpoints) == 0 {
		return true
	}
	for _, e := range endpoints {
		if e == path {
			return true
		}
	}
	return false
}

func sortedLocked() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
