package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/google/uuid"
)

// NewRequestID mints a uuid v7: unix ms then random, so ids sort by admission. It carries nothing
// else — where the request went is on the access line under the same rid. Must: the only failure
// is crypto/rand, and a request without an id would be counted in-flight under "".
func NewRequestID() string {
	return uuid.Must(uuid.NewV7()).String()
}

// SHA256Hex is the fleet's one hashing rule: it turns an API secret into the meter id that names
// its Redis record, and a caller-chosen session into the opaque key an ingress is allowed to see.
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Bearer strips the scheme off an Authorization header. A bare token is accepted as-is: some
// clients send one, and refusing it would fail a request over a formatting detail.
func Bearer(header string) string {
	header = strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return header
}
