# Admission

Three records, resolved in one call — `key:` → `team:` → `model_group:`. A key names one team and
any number of groups; the team's verdicts are applied onto the key (`KeyRecord.Under`) and the
groups' grants are unioned into one before the gates run, so every gate reads one record.

**The key is the policy; the team is the verdict.** Everything that is the key's — models,
geography, its own rate limits, and the cap it spends against — sits on the key. What is the
team's — `limited` (its balance is spent), `log_payloads`, and the rate limits a key with none of its own runs under — sits on `team:<Central
Team>`, one record however many keys the team holds, so suspending a team is one write. The cap and
its counter stay on the key: a key lives on one geography's store, that store holds the key's whole
cap and counts all its spend, so the gate is exact however many other keys the team holds
elsewhere. A key whose team record the store does not hold is read as pushed — the fields a control
plane from before teams wrote onto it — until the cleanup release, when it is a 503.

`Evaluate` then runs three gates, in this order:

| Gate | Status | Why in this position |
|---|---|---|
| key is `active` | 401 | Checked first: a revoked key is 401 even when it is also over quota, because the key is the thing that is wrong |
| key has credit | 402 | `limited` is a pushed flag (the control plane's verdict on the team); `spent >= budget` is this Redis's own counter against the key's cap — this UTC month's, or everything since the push when the key's `budget_window` is `total` — 0 until the control plane sets one. Either → 402 |
| `CanUse(model)` | 403 | |

`CanUse` is the whole access rule and fails closed:

```
deny wins over everything          key.Deny[model]        → false
otherwise, every group's grant ∪ key's own allow           → true
nothing granted it                                        → false
```

`/v1/models` filters its list with the *same function*, so the catalogue can never advertise
something the inference path would refuse.

## Credit gates

Two, both read off the key record before the body is, both answering 402
`credit balance exhausted`. The control plane's: its verdict on the team, flipped as `limited` and
pushed onto every key of the team. This box's own: `spent >= budget`, the key's monthly
cap against this month's counter on the record (see [The records themselves](../../repository/redis/README.md#the-records-themselves)). The cap's 402
says the key's `cap_message` when the control plane pushed one (a trial team's "upgrade to go on");
the team's verdict keeps the default text. Without a message a client cannot tell which one
refused it.

## Rate limiting

Per key, pushed on its record as `limits`: a comma list of `<metric>:<window>:<value>`,
e.g. `requests:1m:200,total_tokens:1h:50000`. No entry = uncapped. Checked in `quota`, after the
credit gates, so a key with no balance does not use up a request.

| | |
|---|---|
| Metrics | `requests` — counted as the request is admitted. `total_tokens` — prompt + completion as the answer reports them, cache reads included |
| Windows | `1m` `1h` `1d` `1M`. They reset on the UTC clock (top of the minute, the hour, midnight, the 1st), not from the key's first request |
| Counter | `lim:<key prefix>:<metric>:<window>:<bucket>`, kept two windows. Bucket = `floor(unix / seconds)`, or `2026-10` for the month |
| Refusal | 429 `rate limit exceeded: 200 requests per 1m` (`rate_limit_error`), `Retry-After` = seconds to the window's end. Over several limits at once, the one that resets last is named |

**Requests are exact, tokens are check-then-debit.** One Lua script reads every counter and, only
when all have room, counts the request — so a refusal counts nothing and two requests cannot both
take the last slot. Tokens are unknown until the answer: `meter` adds them to the window the answer
ended in. The request that crosses a token limit completes; the overshoot is the requests in flight
times their size. A request with no token usage (audio seconds) moves only the request limits.

What counts: every request `quota` admits, whatever happens below it (a 403 on the model, a full
engine). `fallback` and `retry` sit below, so a fallback model or a vendor key rotation is still
one request. A response cut short
is debited what usage it reported. A debit the store refuses is logged and dropped, never spooled:
replayed later, it would land in a window the tokens were not used in.

Scope: the counters live in this gateway's store, so a limit is exact across every gateway sharing
it and separate on a gateway with its own Redis. The limit store failing is a 503 for a key with
limits; one without never reads it. A push carrying a limit this binary cannot read is refused 400,
naming the key.
