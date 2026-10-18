# Status vocabulary

Every refusal speaks its surface's shape — `{"error":{"message":…,"type":…}}` for OpenAI clients,
`{"type":"error","error":{…}}` under `/anthropic`, `{"error":{…}}` under `/openai` — so a client parses them the same way whichever
gate produced it. `type` is the error's class, named off the status as the
Anthropic API names it (`authentication_error`, `rate_limit_error`, …), except `maintenance` and
`draining`, which a 503 alone does not say.

| | Means | Client should |
|---|---|---|
| 400 | the body is not a JSON object, names no `model`, or its `fallbacks` is not a list of at most 3 names | fix the request |
| 401 | no key, unknown key, revoked key | fix the credential |
| 403 | the key exists but may not use this model | ask for access |
| 408 | the body did not arrive within 60s of the headers | resend on a working connection |
| 413 | body over `max_body_bytes` | send less |
| 402 | the key's monthly cap is spent, or its team is limited | a higher cap; nothing to retry |
| 429 | every replica at capacity, or the key is over a rate limit | back off and retry — `Retry-After` is set for a rate limit |
| 499 | the client left before the upstream answered. Only ever in the access line: nobody is there to receive it | – |
| 502 | the engine could not be reached or failed the hop | retry; another replica may take it |
| 504 | the engine sent no headers within `upstream_read_timeout`, or could not be dialled in time | retry; another replica may take it |
| 503 | the model has no healthy placement, a store is unreadable, or the gateway is draining or in maintenance | retry with backoff — `Retry-After` is set when draining or in maintenance |
