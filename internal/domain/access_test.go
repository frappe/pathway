package domain

import "testing"

func set(models ...string) map[string]bool {
	m := map[string]bool{}
	for _, s := range models {
		m[s] = true
	}
	return m
}

// key builds a live key record in a group.
func key(group string) KeyRecord {
	return KeyRecord{Status: "active", Groups: ModelSet(group)}
}

func TestEvaluate(t *testing.T) {
	tier := GroupRecord{Models: set("a")}
	overBudget := KeyRecord{Status: "active", Groups: ModelSet("tier"), Limited: true}
	revoked := key("tier")
	revoked.Status = "revoked"
	revokedAndOver := overBudget
	revokedAndOver.Status = "revoked"
	cases := []struct {
		name   string
		rec    KeyRecord
		model  string
		status int
	}{
		{"group grant admits", key("tier"), "a", 200},
		{"model the group does not grant", key("tier"), "b", 403},
		{"revoked key", revoked, "a", 401},
		{"key out of credit", overBudget, "a", 402},
		// The credential is the thing that is wrong, so it is named first.
		{"revoked beats over-budget", revokedAndOver, "a", 401},
		// Fails closed: no group and no allow must not fall through to "everything".
		{"ungrouped with no allow reaches nothing", key(""), "a", 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grp := GroupRecord{}
			if len(tc.rec.Groups) > 0 {
				grp = tier
			}
			got, reason := Evaluate(tc.rec, grp, tc.model)
			if got != tc.status {
				t.Fatalf("status = %d (%q), want %d", got, reason, tc.status)
			}
		})
	}
}

// The key's own lists are deltas on top of the group — the precedence that used to be resolved in
// grove/access.py before the group moved into its own Redis record.
func TestCanUseResolvesTheKeysDeltas(t *testing.T) {
	tier := GroupRecord{Models: set("a", "b")}
	cases := []struct {
		name  string
		rec   KeyRecord
		model string
		want  bool
	}{
		{"allow adds a model the group lacks", KeyRecord{Groups: ModelSet("t"), Allow: set("z")}, "z", true},
		{"allow works without any group", KeyRecord{Allow: set("z")}, "z", true},
		{"deny beats the group's grant", KeyRecord{Groups: ModelSet("t"), Deny: set("b")}, "b", false},
		{"deny beats the key's own allow", KeyRecord{Allow: set("z"), Deny: set("z")}, "z", false},
		{"denying an ungranted model is harmless", KeyRecord{Groups: ModelSet("t"), Deny: set("zzz")}, "a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grp := GroupRecord{}
			if len(tc.rec.Groups) > 0 {
				grp = tier
			}
			if got := CanUse(tc.rec, grp, tc.model); got != tc.want {
				t.Fatalf("canUse = %v, want %v", got, tc.want)
			}
		})
	}
}

// Out of credit must win over the model gate: the key is rejected before we check whether the
// model was allowed, so the 402 is not masked by a 403.
func TestEvaluateCreditPrecedence(t *testing.T) {
	rec := KeyRecord{Status: "active", Groups: ModelSet("tier"), Limited: true}
	if got, _ := Evaluate(rec, GroupRecord{Models: set("a")}, "b"); got != 402 {
		t.Fatalf("expected 402 to win over 403, got %d", got)
	}
}

// The prepaid gate is this box's own view of the key's spend against its cap. The control plane's
// flag — the team's balance is gone — refuses alike, whatever this box's counter says.
func TestEvaluateThePrepaidCap(t *testing.T) {
	tier := GroupRecord{Models: set("a")}
	prepaid := func(spent, budget int64) KeyRecord {
		return KeyRecord{Status: "active", Groups: ModelSet("tier"), Prepaid: true, Spent: spent, Budget: budget}
	}
	cases := []struct {
		name   string
		rec    KeyRecord
		status int
		reason string
	}{
		{"funded admits", prepaid(999, 1000), 200, ""},
		{"spent to the ceiling", prepaid(1000, 1000), 402, "credit balance exhausted"},
		{"overspent", prepaid(1500, 1000), 402, "credit balance exhausted"},
		{"no cap pushed", prepaid(0, 0), 402, "credit balance exhausted"},
		{"not prepaid ignores the cap", KeyRecord{Status: "active", Groups: ModelSet("tier"), Spent: 1500, Budget: 1000}, 200, ""},
		{"limited refuses a funded key", KeyRecord{Status: "active", Groups: ModelSet("tier"), Limited: true, Prepaid: true, Budget: 1000}, 402, "credit balance exhausted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := Evaluate(tc.rec, tier, "a")
			if got != tc.status || reason != tc.reason {
				t.Fatalf("status = %d (%q), want %d (%q)", got, reason, tc.status, tc.reason)
			}
			if tc.rec.Exhausted() != (tc.status != 200) {
				t.Fatalf("Exhausted = %v, want %v", tc.rec.Exhausted(), tc.status != 200)
			}
		})
	}
}

// A pin is honoured against this gateway's geography; an unpinned key serves anywhere, and a
// gateway with no geography of its own refuses every pinned key.
func TestGeographyDenial(t *testing.T) {
	cases := []struct {
		name, pin, gateway string
		want               bool
	}{
		{"unpinned", "", "in", false},
		{"pinned here", "eu", "eu", false},
		{"pinned elsewhere", "eu", "in", true},
		{"gateway without a geography", "eu", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := GeographyDenial(KeyRecord{Geography: tc.pin}, tc.gateway)
			if (err != nil) != tc.want {
				t.Fatalf("denial = %v, want refused %v", err, tc.want)
			}
		})
	}
}
