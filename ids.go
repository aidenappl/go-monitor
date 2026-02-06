package monitor

import (
	"crypto/rand"
	"fmt"
)

// generateID creates a UUID v4 (random) format ID.
// Format: xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx
// Uses crypto/rand for secure randomness.
func generateID() string {
	return generateUUID()
}

// generateShortID creates a UUID v4 (random) format ID.
// For consistency, all IDs use the same UUID format.
func generateShortID() string {
	return generateUUID()
}

// generateUUID creates a UUID v4 (random) format string.
func generateUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("monitor: failed to generate random ID: " + err.Error())
	}

	// Set version (4) and variant (RFC 4122)
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant is 10

	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
