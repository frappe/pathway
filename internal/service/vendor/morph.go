package vendor

// morph speaks both shapes as base does: `x-api-key` (or a Bearer) on /v1/messages, its id under
// X-Request-Id, no server tool run (probed 2026-11-05). No CacheKey on purpose: one key shared
// across tenants funnels them all onto one worker, and 429s.
type morph struct{ base }
