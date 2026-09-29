package domain

import (
	"strconv"
	"strings"
)

// Passive health, from traffic already flowing: every admitted request reports how its hop went and
// a target that keeps failing stops being chosen. Better informed than a probe, which tests a path
// no customer is on.

const (
	// EjectAfter is how many consecutive failures retire a target. One is noise — a client
	// disconnect, a single unlucky reload. Three in a row against live traffic is a target that is
	// not serving.
	EjectAfter = 3
)

// Who ended a response that did not finish. Blank on one that did.
const (
	// CutClientLeft: the client closed the connection, before the answer or in the middle of it.
	CutClientLeft = "client_left"
	// CutUpstreamIdle: the upstream sent its headers, then nothing for the read timeout.
	CutUpstreamIdle = "upstream_idle"
	// CutUpstream: the upstream's body broke off, by a reset or a close in the middle of a chunk.
	CutUpstream = "upstream"
)

// IsHopFailure reports whether the TARGET is broken rather than the request. A connection error,
// 502 or 504 is the hop. A 503 with X-Grove-Reason: no-replica is not — the ingress answered fine
// and one model has nowhere to go, and counting it would eject the ingress for every other model.
// Nor is a hop the client walked away from before the upstream answered: that says nothing about
// the upstream, and three impatient clients in a row would otherwise retire a healthy one.
func IsHopFailure(upstreamStatus, reason, cut string) bool {
	// A no-replica 503 is the ingress working correctly. Checked before the status, because the
	// status alone cannot tell it from an ingress that is down.
	if strings.TrimSpace(reason) == "no-replica" {
		return false
	}
	status := firstStatus(upstreamStatus)
	// Blank: no response was recorded. The connection failed or timed out, which is the target's
	// fault, unless it was the client that left.
	if status == 0 {
		return cut != CutClientLeft
	}
	return status == 502 || status == 503 || status == 504
}

func IsHopSuccess(upstreamStatus string) bool {
	status := firstStatus(upstreamStatus)
	return status > 0 && status < 500
}

// firstStatus reads an upstream status. nginx wrote a comma-separated list when a request touched
// several upstreams; the Go proxy reports one, so the split is vestigial and costs one IndexAny.
// ponytail: the list form dies with the last OpenResty box; drop the split then.
func firstStatus(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if i := strings.IndexAny(raw, ",:"); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	status, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return status
}
