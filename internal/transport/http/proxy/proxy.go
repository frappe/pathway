// Package proxy forwards an admitted request to the engine that was picked for it, and reads the
// usage frame out of the response on the way back without touching a byte of it — save two: on a
// route whose upstream answers under its own model id, that id is swapped back to the client's; and
// an event stream the upstream abandons gets one error event appended, so the client is told why.
package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/transport/respond"
)

// Outcome is what the proxy learned, for metering and passive ejection.
type Outcome struct {
	Status int    // the upstream's status; 0 means the hop never produced one
	Usage  string // the captured usage line, empty if the response carried none
	// UsageStart is the first usage line of a response that carried more than one: where an
	// Anthropic stream reports its prompt.
	UsageStart string
	// UpstreamRID is the upstream's own request id off x-request-id or request-id: what a ticket
	// to a vendor quotes. Blank on our engines, which run without request-id headers.
	UpstreamRID string
	// Deployment is the placement an ingress chose, off its response header. On a direct route the
	// gateway already knows; this is the only way usage reaches a placement it never picked.
	Deployment string
	// Reason is why the hop gave no usable answer: an ingress's X-Grove-Reason — a no-replica 503
	// means the ingress answered correctly and must not count against it — or, on a hop that
	// produced no status, what the gateway told the client instead.
	Reason string
	// Cut names who ended a response that did not finish: one of domain's Cut values. Blank on a
	// response that finished, and on a hop that never produced one.
	Cut string
}

// statusClientClosed is what the access line shows for a client that left before the upstream
// answered. Nobody receives it; it keeps those requests out of the 502s.
const statusClientClosed = 499

// errUpstreamIdle is why the hop was cancelled when the upstream went silent after its headers.
var errUpstreamIdle = errors.New("the upstream sent nothing for the read timeout")

// Options are the dials that used to be nginx directives.
type Options struct {
	// ReadTimeout bounds the upstream's silence: the wait for its headers, and every wait for
	// more of its body after them. Long: a model may think for minutes before its first token.
	ReadTimeout time.Duration
	DialTimeout time.Duration
	// VerifyUpstream turns on certificate verification for engine and ingress hops. Per target here,
	// so it is a fleet default rather than the ceiling nginx's per-location directive imposed.
	// It does not reach an external hop, which always verifies.
	VerifyUpstream bool
	// PingAfter is how long an HTTP/2 upstream connection may deliver nothing before it is pinged,
	// PingTimeout how long the answer may take before the connection is closed. A connection that
	// died without a FIN is never idle under traffic, and would keep taking requests without it.
	PingAfter   time.Duration
	PingTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = 600 * time.Second
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.PingAfter <= 0 {
		o.PingAfter = 15 * time.Second
	}
	if o.PingTimeout <= 0 {
		o.PingTimeout = 90 * time.Second
	}
	return o
}

// Proxy holds the transports. One per target host and verification setting rather than one for the
// fleet: it is what makes verification a property of the hop instead of a property of the whole
// listener, and what stops an external hop borrowing a pooled connection dialled without checks.
type Proxy struct {
	log *slog.Logger

	mu         sync.RWMutex
	opts       Options
	transports map[string]http.RoundTripper
}

func New(opts Options, log *slog.Logger) *Proxy {
	return &Proxy{opts: opts.withDefaults(), log: log, transports: map[string]http.RoundTripper{}}
}

// Reconfigure replaces the options and DROPS the transport pool. Verification and the header
// timeout are baked in at build time, so keeping it would leave engines already dialled on the old
// setting and new ones on the new. Live connections finish on the old transport, then it is garbage.
func (p *Proxy) Reconfigure(opts Options) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opts = opts.withDefaults()
	p.transports = map[string]http.RoundTripper{}
}

// Forward proxies to target (a base URL; the client's path is appended) and fills outcome with what
// the hop did. The Outcome is filled even on a dial failure — a hop with no status is itself the
// signal that ejects a dead engine.
//
// The caller owns the Outcome instead of taking it as a return value: ReverseProxy panics with
// http.ErrAbortHandler when a client hangs up mid-stream, and the unwind discards a return value
// along with whatever usage the tee had already captured.
func (p *Proxy) Forward(w http.ResponseWriter, r *http.Request, target string, external bool, swap ModelSwap, outcome *Outcome) {
	base, err := url.Parse(target)
	if err != nil || base.Host == "" {
		p.log.Error("unroutable target", "target", target, "err", err)
		return
	}

	// The hop has a context of its own under the client's, so an upstream gone silent can be cut
	// while the client is still there, and the two told apart afterwards.
	hop, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)

	var tee *usageTee
	defer func() {
		if tee != nil {
			outcome.Usage = tee.Usage()
			outcome.UsageStart = tee.UsageStart()
		}
		outcome.Cut = cutBy(hop, r.Context(), tee)
	}()

	reverse := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = base.Scheme
			pr.Out.URL.Host = base.Host
			pr.Out.URL.Path = singleJoin(base.Path, pr.In.URL.Path)
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			// SNI and virtual hosts: the target's name, not the one the client asked us for.
			pr.Out.Host = base.Host
			// The tee reads the body as it passes, so the upstream is never asked to compress it.
			pr.Out.Header.Del("Accept-Encoding")
			// ReverseProxy drops inbound X-Forwarded-* whenever Rewrite is set, so a rewrite cannot
			// carry a spoofed header. Ours are not the client's — upstreamauth just wrote them from
			// the peer address — so they go back explicitly, not via SetXForwarded, which appends.
			for _, header := range []string{"X-Forwarded-For", "X-Real-IP", "X-Forwarded-Proto"} {
				if value := pr.In.Header.Get(header); value != "" {
					pr.Out.Header.Set(header, value)
				}
			}
		},
		Transport: p.transportFor(base, external),
		// -1 flushes every write immediately, which is what streams a token the moment it arrives.
		// Any positive interval would batch an SSE stream into chunks and make the response feel
		// slower than the engine actually is.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			outcome.Status = resp.StatusCode
			outcome.Deployment = resp.Header.Get("X-Grove-Engine")
			outcome.Reason = resp.Header.Get("X-Grove-Reason")
			// A vendor's id is kept for the access line; an engine or ingress only echoes ours.
			if external {
				outcome.UpstreamRID = resp.Header.Get("X-Request-Id")
				if outcome.UpstreamRID == "" {
					outcome.UpstreamRID = resp.Header.Get("Request-Id")
				}
			}
			keepRelayed(resp.Header)
			// A 101 hands the connection to ReverseProxy, which needs the body to stay an
			// io.ReadWriteCloser to write back to the engine. The tee is read-only and would fail
			// the handshake — and a hijacked stream has no usage frame to scrape anyway.
			if resp.StatusCode == http.StatusSwitchingProtocols {
				return nil
			}
			idle := newIdleBody(resp.Body, p.readTimeout(), func() { cancel(errUpstreamIdle) })
			tee = newUsageTee(idle, isEventStream(resp))
			resp.Body = tee
			if swap.active() {
				// The tee stays innermost so usage is scraped off the raw upstream bytes. The two
				// ids differ in length, so the declared length cannot survive the rewrite.
				resp.Header.Del("Content-Length")
				resp.ContentLength = -1
				resp.Body = newModelSwapReader(tee, swap)
			}
			if isEventStream(resp) {
				// Outermost, and the declared length dropped: the closing event adds bytes.
				resp.Header.Del("Content-Length")
				resp.ContentLength = -1
				resp.Body = newStreamEnd(resp.Body, r.Context(), hop, respond.Dialect(r.Context()) == domain.DialectAnthropic)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A client that left before the upstream answered: nobody is there to answer, and the
			// hop says nothing about the upstream.
			if r.Context().Err() != nil {
				p.log.Debug("client left before the upstream answered", "target", target)
				w.WriteHeader(statusClientClosed)
				return
			}
			// Status stays 0, which is what marks the hop failed: the connection never got far
			// enough to have one.
			p.log.Warn("upstream hop failed", "target", target, "err", err)
			// A dial or header wait that ran out is a timeout and says so; anything else is a
			// connection that was refused or broken.
			status, message := http.StatusBadGateway, "upstream unavailable"
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				status, message = http.StatusGatewayTimeout, "upstream timed out"
			}
			outcome.Reason = message
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if respond.Dialect(r.Context()) == domain.DialectAnthropic {
				_, _ = w.Write(append(domain.AnthropicError(status, []byte(message)), '\n'))
				return
			}
			_, _ = w.Write([]byte(`{"error":{"message":"` + message + `","type":"api_error"}}` + "\n"))
		},
	}

	reverse.ServeHTTP(w, r.WithContext(hop))
}

// relayed is what a client is shown of an upstream's response headers: what it needs to read the
// body, to back off, and to finish an upgrade. Everything else stops here — a vendor's ids, its
// cookies, its `alt-svc`, the rate limits of the account we call it with, and our own ingress's
// X-Grove-* — and so do its request ids: ReverseProxy ADDS upstream headers onto the ones the edge
// set, so a vendor's would stand beside ours or replace it.
var relayed = map[string]bool{
	"Content-Type": true, "Content-Length": true, "Content-Encoding": true, "Content-Disposition": true,
	"Cache-Control": true, "Retry-After": true,
	"Upgrade": true, "Connection": true,
	"Sec-Websocket-Accept": true, "Sec-Websocket-Protocol": true, "Sec-Websocket-Extensions": true,
}

func keepRelayed(header http.Header) {
	for name := range header {
		if !relayed[name] {
			delete(header, name)
		}
	}
}

// cutBy names who ended a response that did not finish, blank for one that did. In this order: a
// silent upstream is cancelled by us, and a client that left cancels everything under it, so
// either can also look like a body that broke off.
func cutBy(hop, client context.Context, tee *usageTee) string {
	switch {
	case context.Cause(hop) == errUpstreamIdle:
		return domain.CutUpstreamIdle
	case client.Err() != nil:
		return domain.CutClientLeft
	case tee != nil && tee.failed:
		return domain.CutUpstream
	}
	return ""
}

func (p *Proxy) readTimeout() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.opts.ReadTimeout
}

// isEventStream reports whether the upstream answered with server-sent events, which is what
// decides how the usage is read out of the body.
func isEventStream(resp *http.Response) bool {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return mediaType == "text/event-stream"
}

// transportFor keeps one transport per target host and verification setting, so connections are
// pooled per engine and the TLS settings are the hop's own.
//
// An external hop verifies whatever the fleet default is. That default is off, because an engine
// inside the VPC answers on a certificate we signed ourselves — but a vendor is reached over the
// public internet carrying its own API key, and an unverified hop there is a key handed to whoever
// answers the address.
func (p *Proxy) transportFor(base *url.URL, external bool) http.RoundTripper {
	verify := external || p.opts.VerifyUpstream
	// The key carries `verify`: two hops to one host that disagree about it must not share a
	// connection pool dialled under the weaker one.
	host := fmt.Sprintf("%s://%s|verify=%t", base.Scheme, base.Host, verify)

	p.mu.RLock()
	existing, ok := p.transports[host]
	p.mu.RUnlock()
	if ok {
		return existing
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.transports[host]; ok {
		return existing
	}
	opts := p.opts
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: opts.DialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: opts.ReadTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   64,
		// Streaming responses must not be buffered on the way in either.
		DisableCompression: true,
		ForceAttemptHTTP2:  true,
		HTTP2:              &http.HTTP2Config{SendPingTimeout: opts.PingAfter, PingTimeout: opts.PingTimeout},
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: !verify, //nolint:gosec // see Options.VerifyUpstream
			MinVersion:         tls.VersionTLS12,
		},
	}
	p.transports[host] = transport
	return transport
}

// singleJoin appends the client's path to the target's base without doubling or dropping a slash.
// The base carries the engine's location prefix (https://box/e/md-00007), so this is what makes
// /v1/chat/completions land on /e/md-00007/v1/chat/completions.
func singleJoin(base, requested string) string {
	switch {
	case base == "" || base == "/":
		return requested
	case strings.HasSuffix(base, "/") && strings.HasPrefix(requested, "/"):
		return base + strings.TrimPrefix(requested, "/")
	case !strings.HasSuffix(base, "/") && !strings.HasPrefix(requested, "/"):
		return base + "/" + requested
	default:
		return base + requested
	}
}
