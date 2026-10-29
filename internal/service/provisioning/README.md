# The contract with the control plane

Grove pushes state; the gateway projects it into local Redis and never calls back. **These shapes
are the interface — changing one means changing `agent_sync.py` and `usage_pull.py` too.**
What each key holds is in [repository/redis](../../repository/redis/README.md).

## How the push works

`POST /grove-admin/state` is desired state, whole, and **absence prunes**. The body carries any
subset of four sections — groups, teams, keys, routes — each stamped with a
hash Grove computed. The agent applies the whole body in ONE Redis MULTI: HSET every named
record, DEL every record in a pushed section the payload does not name, then store the hashes in
`grove:state_hash`. A Redis error is a 500 and none of it lands — the hashes never claim state
that did not arrive. Only `model_group:/team:/key:/deploy:` are ever pruned; usage,
sticky, inflight and health keys are the gateway's own.

Every admin body is decoded strictly: a field this binary does not know is a 400 naming it
(`bad body: json: unknown field "credentials"`), nothing is stored and no hash is written, so the
Pathway Sync row says which field and the next tick pushes again. The alternative — keep the
record without the field under the hash of the full payload — is drift no later push would see,
which is what happened once. Consequence: a new route field ships in the binary before Grove
pushes it.

`GET /grove-admin/state-hash` returns that stored map, with two stamps for an operator: `pushed_at`,
when a push last landed on this process (any section, any endpoint), and `contact_at`, when the
control plane last called any admin endpoint with the right token, this one included; each left out
until it has happened since the process started. They live in memory, not the store: a fresh
process knows nothing until the next tick, and the hashes are diffed, so nothing rides with them.
`contact_at` is the liveness signal — a quiet fleet pushes nothing for hours — and `/healthz`
carries it as `X-Grove-Contact-Age` in seconds. Grove diffs its computed hashes against it
every 2 minutes and pushes only what differs — an in-sync box costs one GET. `teams` and `keys` are
split into 256 buckets (`domain.BucketOf` = `sha256(id)[:2]`, same rule Grove uses) hashed
independently, so one minted key or one suspended team ships one bucket, not the population. A wiped Redis has no
hashes, reads as total drift, and is fully rebuilt on the next tick — that is the only repair
path and the only one needed. Both planes mount these endpoints; an ingress only ever receives
the routes section. Contract: `plan_agent_state_sync.md` in the infer-plat tree. The old
per-section `PUT`/`DELETE` endpoints remain for one release after the control plane cuts over.

Every admin endpoint is gated on `X-Grove-Admin-Token`, compared in constant time, and mounted on
`GROVE_SELF_HOST` only — a push has to reach **one** gateway, and the public name means all of them.

`GET /grove-admin/in-flight` is read-only: `{"maintenance": bool, "in_flight": n}`, where
`in_flight` counts data requests past the `drain` stage, realtime sessions until they close.

`GET /grove-admin/spent?ids=a,b` answers `{"<id>": nano-USD}`: each key's spend this month on this
store, by the id the control plane pushed it under, off the counter the cap is gated on
(`spent:<month>` on `key:<id>`). A key not held here is left out. One pipelined read, the drains
untouched: the figure a cap check is made on.

`GET /grove-admin/usage[?keys=p1,p2]` answers `{"drains": {"<drain id>": {"<prefix>": {"<YYYY-MM-DD>":
hash}}}}` and deletes nothing. A live counter is one key's usage for one UTC day — the day this box
served it, off the clock that names the key's `spent:<month>` — so the day and the month the
control plane records are this store's own. Each live `usage:<prefix>:<day>` — every one, or only
the listed prefixes', found by one SCAN either way — is RENAMEd to `drained:<id>:<prefix>:<day>`
under a new drain id and added to `drain:unacked`, in one Lua call, so a request metered mid-drain
lands wholly in the drain or wholly on a fresh counter. The answer is every pair still in
`drain:unacked` (only the listed prefixes' when `keys` is given), old drains included.
A day of `""` is a counter from before this build bucketed by day, which the control plane lands on
the day it pulls. `POST /grove-admin/usage/ack {"acks": {"<drain id>": {"<prefix>": ["<day>", ...]}}}`
takes the counters the control plane recorded out of `drain:unacked` and keeps them for
`usage_retention` (default three days), answering `{"ok": true, "count": n}`; one not waiting is a
no-op. A bucket it never acks
is answered again, under its own id, on every pull, while the rest move on: a control plane that
failed to record one user loses nothing and holds nobody else back, and one that recorded it but
lost the ack dedupes on the (drain id, prefix, day) triple. Nothing live and nothing waiting
answers `{"drains": {}}`.

The pull hands [the spool](../metering/README.md#the-spool)'s dead lines over: `GET /grove-admin/usage` also answers `"dead": [{"id", "line",
"error"}]` and `"spool": {"depth", "replayed", "dropped", "dead"}` (lines waiting now, and counts
since the process started). The control plane lands or records each dead line and names it in the
ack — `"dead": ["<request id>", ...]` beside `"acks"` — which removes it; `count` includes them.

`GET /grove-admin/provider-keys?ids=a,b` answers `{id: {requests, ok, rate_limited, rejected,
failed, last_used, last_rate_limited}}` per vendor credential since the last call, and deletes
each `pk:<key id>` in the same transaction, so the next call starts from zero; an id never dialled
is zeros. A pull that does not land is lost — these are stats, not usage.

`POST /grove-admin/spend-adjust {"key", "delta", "id"}` corrects one key's `spent:<YYYY-MM>` for
the current month on this store by `delta` nano-USD, once per `id` (`key` is the record id, sha256 hex) — a retry answers
`{"spent", "applied": false}` and moves nothing. A key this store does not hold is a 404; nothing
is invented.
