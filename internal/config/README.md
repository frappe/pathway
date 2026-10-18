# Configuration

Split by **lifetime**, and disjoint — nothing appears in both halves.

## Environment — identity, secrets, sockets, paths (`/etc/pathway/agent.env`)

| | |
|---|---|
| `GROVE_ADMIN_TOKEN` | **required**; the process refuses to start without it |
| `GROVE_GATEWAY_ID` / `GROVE_INGRESS_ID` | which plane; both set is a refusal |
| `GROVE_INGRESS_TOKEN` | required on an ingress; blank there refuses every gateway |
| `GROVE_GATEWAY_REGION` | this gateway's region, which `PickRoute` prefers |
| `GROVE_GATEWAY_GEOGRAPHY` | this gateway's geography; a user pinned to another (any pin, when blank) gets 403 `this key is restricted to geography <g>` on inference and `/v1/models` |
| `GROVE_REDIS_ADDR` | default `127.0.0.1:6379`; the store's private address when shared |
| `GROVE_REDIS_PASSWORD` | the shared store's `requirepass`; blank = no AUTH |
| `GROVE_LISTEN_HTTP` / `GROVE_LISTEN_HTTPS` | the data path; at least one is required |
| `GROVE_PUBLIC_HOST` | the shared name customer traffic arrives on |
| `GROVE_SELF_HOST` | this box's own name — carries `/grove-admin` and the scrape |
| `GROVE_TLS_CERT` / `GROVE_TLS_KEY` | fleet wildcard; hot-reloaded from disk on change |
| `GROVE_HTPASSWD` | bcrypt htpasswd for `/metrics/node` |
| `GROVE_NODE_EXPORTER_URL` | default `http://127.0.0.1:9100/metrics` |
| `GROVE_ACCESS_LOG` | file for the per-request line; blank → stdout |
| `GROVE_ERROR_LOG` | file mirroring Warn and above out of the process log; blank → stdout only |
| `GROVE_PAYLOAD_LOG` | file for prompts and outputs of users flagged `log_payloads`; customer content, so its own file and retention. Blank turns `payloadlog` off box-wide |
| `GROVE_CONFIG` | tunables path; default `/etc/pathway/config.json` |
| `GROVE_PID_FILE` | optional |

## File — tunables, re-read on **SIGUSR1**

```json
{
  "log_level": "info",
  "middleware": ["recover", "accesslog", "drain", "auth", "quota", "body", "modelaccess",
                 "payloadlog", "route", "meter", "fallback", "retry", "transform", "upstreamauth"],
  "transforms": ["modelmap", "streamusage", "servicetier", "vendorfields"],
  "synthetic_session_ttl": "0s",
  "capacity_wait": "0s",
  "max_body_bytes": 33554432,
  "upstream_read_timeout": "600s",
  "upstream_tls_verify": false,
  "drain_timeout": "630s",
  "lame_duck": "5s",
  "upgrade_timeout": "30s",
  "usage_retention": "72h",
  "usage_spool": "/var/lib/pathway/usage-spool.jsonl",
  "usage_spool_max_bytes": 1073741824,
  "maintenance": false
}
```

Every field is optional; an omitted one keeps its default, and a missing file is all defaults.

**A rejected file changes nothing.** Bad JSON, an unknown key, a bad duration, or an unregistered
middleware name → one error line and the running configuration is kept, applied whole or not at all.
Parsing is strict on purpose: a knob that silently became its default is a knob the operator
believes they turned.

`upstream_read_timeout` bounds an upstream's silence, not a request's length: the wait for its
headers, and every wait for more of its body after them. A model that thinks for ten minutes before
its first token needs it raised; a stream that talks for an hour does not. Running out before the
headers is a **504** `upstream timed out`; after them, an event stream ends in an error event (see
[Streaming](../transport/http/proxy/README.md#streaming)).

`synthetic_session_ttl` is the one worth knowing. `0s` balances every caller that names no session
of its own; `30m` pins each API key to one engine, which is what a single-placement fleet always
did and the lever to pull if balancing goes wrong.

`capacity_wait` is how long a request waits for a slot when every upstream of its model is at its
`capacity`, before the 429. `0s` refuses at once. Nothing is claimed or billed while it waits, and a
client that leaves stops the wait. A vendor row is never counted and has no cap here: the vendor's
own 429 is its cap.

`maintenance: true` refuses every new data request with 503 `maintenance` (`Retry-After: 30`) and
fails `/healthz`, while requests already running finish. It lives in the file, so a box restarted
in maintenance comes back in it. `GET /grove-admin/in-flight` → `{"maintenance", "in_flight"}` is
how the control plane knows the box has gone idle. An older binary refuses a file carrying this key
(unknown field), so it is written only once this release is deployed.
