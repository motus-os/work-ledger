// Package ids creates opaque identifiers for ledger records.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

const randomBytes = 16

// New returns a prefix followed by 128 bits from the operating system's
// cryptographic random source. Prefix must be a short, non-empty ASCII label
// ending in an underscore, such as "run_" or "event_".
func New(prefix string) (string, error) {
	if !validPrefix(prefix) {
		return "", fmt.Errorf("invalid ID prefix %q", prefix)
	}
	random := make([]byte, randomBytes)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("read random ID bytes: %w", err)
	}
	return prefix + hex.EncodeToString(random), nil
}

func validPrefix(prefix string) bool {
	if len(prefix) < 2 || len(prefix) > 16 || prefix[len(prefix)-1] != '_' {
		return false
	}
	for _, character := range prefix[:len(prefix)-1] {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}
