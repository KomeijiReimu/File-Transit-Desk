package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
)

const (
	sessionBindingHeader = "X-Session-Binding"
	sessionBindingDomain = "file-trans/session-binding/v1\x00"
)

// sessionBindingForID derives a non-authorizing, domain-separated subject
// identifier from the database session hash. Only 128 bits are exposed; the
// database hash and HttpOnly cookie are never returned to the client.
func sessionBindingForID(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionBindingDomain + sessionID))
	return hex.EncodeToString(sum[:16])
}

func sessionBindingEqual(expected, actual string) bool {
	if len(expected) != len(actual) || len(actual) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}
