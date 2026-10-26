# The proxy

## Upstream connections

Kept open and reused. `proxy.Proxy` holds one `http.Transport` per target host and verification
setting, with Go's keep-alive: up to 64 idle connections per host, each closed after 90s unused.
`ForceAttemptHTTP2` is on, so a target that offers h2 — OpenAI, Anthropic and Baseten all do —
carries its concurrent requests as streams on one connection, whichever of the vendor's keys each
one dials with: the key is a header, not a connection. An HTTP/1.1 target takes one connection per
request in flight, out of the same pool.

A dial and a TLS handshake are paid only by the first request to a host after:

- a start or a binary upgrade;
- 90s with no traffic to that host;
- a SIGUSR1 that changed any tunable, because `Reconfigure` drops the whole pool.

Each of those is a full handshake: no TLS session cache is set. Neither number is a tunable; both
are set in `proxy.transportFor`.

Each hop's events — connect, TLS handshake, request written, first response byte, body done — are
stamped off `net/http/httptrace` into `Outcome.Hop` and reach the access line as `hop`. The trace
rides the hop's context, which ReverseProxy clones into the outbound request and composes with
its own. Two callbacks can fire after `Forward` has returned (a dial outlives a cancelled request;
a body is still being written when the server answers early), so `Hop` is locked and held by
pointer.

An HTTP/2 connection that has delivered nothing for 15s is pinged, and closed if no answer comes
within another 90s (`PingAfter` and `PingTimeout` in `proxy.Options`). A quiet one that answers is
pinged again every 15s; one that does not gets the single ping, which TCP keeps retransmitting, so
a blip shorter than 90s costs nothing. That is what finds a connection that died without a FIN:
under traffic it is never idle, so the 90s idle close never reaches it.

## Streaming

The response is never buffered. `FlushInterval: -1` pushes each write straight through, and the
usage scraper (`proxy/usagetee.go`) reads the bytes it is already copying and writes nothing back —
so the client sees exactly the stream the engine produced, at the engine's own pace.

One exception, at the end only. When an event stream breaks off — the upstream went silent for
`upstream_read_timeout`, or its body failed — while the client is still there, `proxy/streamend.go`
closes the event in progress and appends one error event in the surface's shape, the one its SDK
raises on, then ends the stream cleanly:

```
data: {"error":{"message":"upstream went silent","type":"api_error"}}                              (OpenAI surface)

event: error
data: {"error":{"message":"upstream went silent","type":"api_error"},"type":"error"}               (/anthropic)
```

`upstream went silent` for the timeout, `upstream broke off the stream` otherwise. Without it the
200 has long gone out and the client sees only a dropped connection. It sits outside the usage
scraper, so usage and the `cut` are read off the upstream's own bytes exactly as before. An event
already cut mid-line still reaches the client broken, ahead of the error. A body that is one JSON
document cannot be repaired this way and is still just dropped.

## Before changing something

- **The usage scraper must never modify the response.** It reads what it is already copying. A
  metering feature that reordered or buffered a token stream would be worse than no metering.
- **`upstream_tls_verify` is per target, not per listener.** That is the thing nginx could not do —
  `proxy_ssl_verify` is a per-location directive, so one self-signed box pinned verification off for
  every target sharing that location. Defaulted `false` for parity; it is a default, not a ceiling —
  and it is not a floor either: a `kind: "provider"` hop verifies whatever it is set to, because
  that request leaves our network carrying a vendor's own API key. The transport pool is keyed on
  the setting as well as the host, so the two can never share a connection.
- **The HTTP/2 ping is what stops a dead upstream connection eating requests.** One that dies
  quietly (no FIN, no RST) keeps taking new requests for as long as traffic keeps it busy: the 90s
  idle close only runs on a connection with no request on it. Without the ping each request hangs
  until `upstream_read_timeout`, and the connection goes only when the kernel gives up
  retransmitting (about 15 minutes at Linux defaults). It is set through `Transport.HTTP2`, which
  is why `go.mod` asks for Go 1.24.
