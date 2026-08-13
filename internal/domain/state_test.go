package domain

import "testing"

// The control plane computes the same rule in Python (sha256(id).hexdigest()[:2]); the fixture
// pins the two implementations to each other, not just this one to itself.
func TestBucketOfMatchesTheControlPlane(t *testing.T) {
	if got := BucketOf("GU-1"); got != "ef" {
		t.Errorf(`BucketOf("GU-1") = %q, want "ef" (sha256 hex prefix)`, got)
	}
}

func TestBucketOfIsTwoHexChars(t *testing.T) {
	for _, id := range []string{"", "a", "some-longer-record-id"} {
		got := BucketOf(id)
		if len(got) != 2 {
			t.Errorf("BucketOf(%q) = %q, want 2 chars", id, got)
		}
	}
}
