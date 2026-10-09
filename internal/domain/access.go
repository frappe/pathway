package domain

import "errors"

import "strings"

// KeyRecord is key:<sha256(secret)>: the credential and the whole policy behind it. The team is
// the ledger the control plane bills; the KEY is what every gate reads — its models, its
// geography, its rate limits and the cap it was handed out of the team's balance. One key lives
// in one geography, so the store it lands on gates that cap exactly however many keys the team
// holds elsewhere. A key the control plane has not pushed reads back as the zero value, which
// grants nothing.
type KeyRecord struct {
	Status    string          // "active" | "revoked"
	Team      string          // Central Team doc name — the tenant boundary the cache salt and payload log key on
	KeyPrefix string          // display id, for logs and usage attribution
	Groups    map[string]bool // Model Group names; empty = ungrouped (grants nothing by itself)
	Allow     map[string]bool // models this key may call on top of its groups'
	Deny      map[string]bool // models this key may not call, whatever granted them
	// Limited is the control plane's verdict that the TEAM's balance is spent → 402 on every
	// key it holds, whatever this store's own counter says.
	Limited bool
	// LogPayloads opts this key's prompts and outputs into the payload log — the one piece of
	// customer CONTENT the platform may retain, so it is off unless the control plane says
	// otherwise, and a record from before the field existed reads as off.
	LogPayloads bool
	// Geography pins this key to one geography's gateways; blank serves anywhere.
	Geography string
	// Prepaid bills this key against a balance. Budget is the key's cap (nano-USD), the slice of
	// the team's balance the control plane allotted it; Spent is this store's own lifetime
	// counter, moved by every metered request and never by a push. This box's balance for the
	// key is Budget − Spent. The control plane keeps the team's own from the same credits.
	Prepaid bool
	Budget  int64
	Spent   int64
	// Limits caps the key's requests and tokens per reset window; none = uncapped.
	Limits []Limit
}

// GroupRecord is what a Model Group grants everyone in it, stored under model_group:<name>. A group
// the control plane has not pushed reads back as the zero value, which grants nothing.
type GroupRecord struct {
	Models map[string]bool // models the group grants
}

// Exhausted reports whether the key has run out: the control plane's verdict on its team, or this
// box's own view of the key's cap.
func (k KeyRecord) Exhausted() bool { return k.Limited || (k.Prepaid && k.Spent >= k.Budget) }

// ExhaustedDenial is the 402 for an exhausted key, nil otherwise. The control plane's verdict and
// this box's own balance answer alike: a client cannot tell, and should not need to, which side
// noticed first.
func ExhaustedDenial(key KeyRecord) error {
	if key.Exhausted() {
		return Deny(402, "credit balance exhausted")
	}
	return nil
}

// CanUse is the access decision: the grant of every group the key is in, plus its own Allow,
// minus its Deny. The union is already merged into grp by the time it gets here. Deny wins over
// every grant. Fails closed — no group and no Allow reaches nothing.
func CanUse(key KeyRecord, grp GroupRecord, model string) bool {
	if key.Deny[model] {
		return false
	}
	return grp.Models[model] || key.Allow[model]
}

// Evaluate is the pure admission decision: an HTTP status (200 admits) and a reason. The
// credential is checked first, so a revoked key is 401 even when it is also over quota.
func Evaluate(key KeyRecord, grp GroupRecord, model string) (int, string) {
	if key.Status != "active" {
		return 401, "key revoked or inactive"
	}
	if err := ExhaustedDenial(key); err != nil {
		var denial Denial
		errors.As(err, &denial)
		return denial.Status, denial.Reason
	}
	if !CanUse(key, grp, model) {
		return 403, "access not allowed for model " + model
	}
	return 200, ""
}

// GeographyDenial refuses a key pinned to a geography other than this gateway's. A gateway with
// no geography refuses every pinned key: fail closed.
func GeographyDenial(key KeyRecord, geography string) error {
	if key.Geography == "" || key.Geography == geography {
		return nil
	}
	return Deny(403, "this key is restricted to geography "+key.Geography)
}

// ModelSet parses one of the comma-joined lists the control plane writes — models, or the group
// names on a key record. Blank → nil, which is a map that answers false to everything, the
// fail-closed default.
func ModelSet(csv string) map[string]bool {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil
	}
	out := map[string]bool{}
	for _, m := range strings.Split(csv, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out[m] = true
		}
	}
	return out
}
