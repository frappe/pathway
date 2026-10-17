package domain

import (
	"crypto/sha256"
	"encoding/hex"
)

// BucketOf names the state bucket a key belongs to: sha256(id)[:2], 256 buckets.
// The control plane buckets by the same rule, so the two sides must never disagree on it.
func BucketOf(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:1])
}
