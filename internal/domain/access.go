package domain

import "strings"

// KeyRecord is key:<sha256(secret)>, deliberately thin: a credential's only fact of its own is
// whether it is revoked. Who holds it and what they may call belong to the PERSON, in user:<name>
// — so one leaked key dies alone, and a budget flip is one write however many keys they hold.
type KeyRecord struct {
	Status    string // "active" | "revoked"
	User      string // Grove User doc name — the pointer to the UserRecord below
	KeyPrefix string // display id, for logs and usage attribution

	// Read only when user:<User> is absent, i.e. a record written before access moved off the
	// credential. SynthUser turns it into a UserRecord so the decision path stays single.
	Legacy LegacyKey
}

// LegacyKey is quarantined so the whole pre-split projection can be deleted in one go, once every
// site has resynced against a control plane that pushes user records.
type LegacyKey struct {
	HasGroup bool            // a pre-GROUP record wrote no `group` field at all, blank included
	Group    string          //
	Allow    map[string]bool //
	Deny     map[string]bool //
	Limited  bool            // the monthly-budget flag, which used to ride on Status
	Models   map[string]bool // pre-group: access already resolved to one flat set
}

// HasProjection reports whether this key was written by a control plane that put access on the
// credential. A current one writes none of these fields, so a current key with no user record
// resolves to nothing rather than falling back to something — fail closed.
func (l LegacyKey) HasProjection() bool {
	return l.HasGroup || l.Models != nil
}

// UserRecord is the holder's policy, stored under user:<Grove User name>. One record however many
// keys they hold. A user the control plane has not pushed reads back as the zero value, which
// grants nothing.
type UserRecord struct {
	Email   string          // denormalized, for humans reading Redis; no decision reads it
	Groups  map[string]bool // Model Group names; empty = ungrouped (grants nothing by itself)
	Allow   map[string]bool // models this user may call on top of their groups'
	Deny    map[string]bool // models this user may not call, whatever granted them
	Limited bool            // over their monthly token budget → 429
	// LogPayloads opts this user's prompts and outputs into the payload log — the one piece of
	// customer CONTENT the platform may retain, so it is off unless the control plane says
	// otherwise, and a record from before the field existed reads as off.
	LogPayloads bool
	// Geography pins this user's keys to one geography's gateways; blank serves anywhere.
	Geography string

	// Set only by SynthUser off a pre-group key, where the control plane had already resolved
	// access down to one model set. Nothing read from Redis sets it.
	Flattened bool
	Models    map[string]bool
}

// GroupRecord is what a Model Group grants everyone in it, stored under model_group:<name>. A group
// the control plane has not pushed reads back as the zero value, which grants nothing.
type GroupRecord struct {
	Models map[string]bool // models the group grants
}

// SynthUser builds the UserRecord a pre-split key record implies. Called only when user:<name> is
// missing, which is what makes the control plane and the agent deployable in either order.
func SynthUser(rec KeyRecord) UserRecord {
	return UserRecord{
		Groups:    ModelSet(rec.Legacy.Group),
		Allow:     rec.Legacy.Allow,
		Deny:      rec.Legacy.Deny,
		Limited:   rec.Legacy.Limited,
		Flattened: !rec.Legacy.HasGroup,
		Models:    rec.Legacy.Models,
	}
}

// CanUse is the access decision: the grant of every group the user is in, plus their own Allow,
// minus their Deny. The union is already merged into grp by the time it gets here. Deny wins over
// every grant. Fails closed — no group and no Allow reaches nothing.
func CanUse(usr UserRecord, grp GroupRecord, model string) bool {
	if usr.Flattened {
		return usr.Models[model] // legacy record: the control plane already resolved it
	}
	if usr.Deny[model] {
		return false
	}
	return grp.Models[model] || usr.Allow[model]
}

// Evaluate is the pure admission decision: an HTTP status (200 admits) and a reason. The only limit
// is a monthly token budget the control plane flags on the USER; the gateway keeps no counters. The
// credential is checked first, so a revoked key is 401 even for someone also over quota.
func Evaluate(rec KeyRecord, usr UserRecord, grp GroupRecord, model string) (int, string) {
	if rec.Status != "active" {
		return 401, "key revoked or inactive"
	}
	if usr.Limited {
		return 429, "monthly token quota exhausted"
	}
	if !CanUse(usr, grp, model) {
		return 403, "access not allowed for model " + model
	}
	return 200, ""
}

// GeographyDenial refuses a user pinned to a geography other than this gateway's. A gateway with
// no geography refuses every pinned user: fail closed.
func GeographyDenial(usr UserRecord, geography string) error {
	if usr.Geography == "" || usr.Geography == geography {
		return nil
	}
	return Deny(403, "this key is restricted to geography "+usr.Geography)
}

// ModelSet parses one of the comma-joined lists the control plane writes — models, or the group
// names on a user record. Blank → nil, which is a map that answers false to everything, the
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
