package transform

import (
	"encoding/json"
	"testing"
)

func saltBody(t *testing.T, raw string) Body {
	t.Helper()
	var b Body
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// The prefix on a provided salt is the security: raw pass-through would let one tenant echo
// another's salt string and land inside their cache namespace.
func TestAProvidedSaltIsNamespacedUnderTheTenant(t *testing.T) {
	body := saltBody(t, `{"model":"m","cache_salt":"team-a"}`)
	changed, err := cacheSalt{}.Apply(Context{User: "test-user"}, body)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if string(body["cache_salt"]) != `"test-user:team-a"` {
		t.Errorf("cache_salt = %s", body["cache_salt"])
	}
}

// Opt-in by user call: no salt means no salt — the shared namespace is the recorded trade.
func TestNoSaltIsNeverInvented(t *testing.T) {
	body := saltBody(t, `{"model":"m"}`)
	changed, _ := cacheSalt{}.Apply(Context{User: "test-user"}, body)
	if changed {
		t.Error("a salt was added to a request that sent none")
	}
	if _, present := body["cache_salt"]; present {
		t.Error("cache_salt appeared from nowhere")
	}
}

// A vendor has no such field, and a strict one 400s on unknowns.
func TestASaltIsStrippedFromAVendorHop(t *testing.T) {
	body := saltBody(t, `{"model":"m","cache_salt":"team-a"}`)
	changed, _ := cacheSalt{}.Apply(Context{User: "test-user", Provider: true}, body)
	if !changed {
		t.Error("nothing changed")
	}
	if _, present := body["cache_salt"]; present {
		t.Error("cache_salt reached a vendor hop")
	}
}

// A non-string salt is the engine's schema error to give, not ours to guess around.
func TestANonStringSaltPassesUntouched(t *testing.T) {
	body := saltBody(t, `{"cache_salt":42}`)
	changed, _ := cacheSalt{}.Apply(Context{User: "test-user"}, body)
	if changed || string(body["cache_salt"]) != "42" {
		t.Errorf("cache_salt = %s, changed = %v", body["cache_salt"], changed)
	}
}
