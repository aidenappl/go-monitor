package monitor

import (
	"crypto/rand"
	"encoding/hex"
)

// generateID creates a random 16-byte hex-encoded ID (32 characters).
// Uses crypto/rand for secure randomness.
func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback should never happen with crypto/rand, but be safe
		panic("monitor: failed to generate random ID: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// generateShortID creates a random 8-byte hex-encoded ID (16 characters).
// Useful for request IDs where shorter IDs are preferred.
func generateShortID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("monitor: failed to generate random ID: " + err.Error())
	}
	return hex.EncodeToString(b)
}
