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

// SHORT_ID_BYTES is 8 — a 16 hex-character, 64-bit token.
//
// It was 4 (32 bits) and that was too few in two separate ways.
//
// COLLISIONS. 32 bits is ~4.3 billion values, so the birthday bound puts a
// 50% chance of collision at roughly 77,000 ids. request_id is minted per
// request, and a Monitor install seeing ~53k events/day reaches that within
// days — after which two unrelated requests share an id and a trace lookup
// returns both, silently. 64 bits moves the same bound past 5 billion ids,
// which at that rate is longer than the platform will exist.
//
// THE WIRE CONTRACT. monitor-core validates these fields against
// structs.correlationIDRegex and rejects anything outside it. Because ingest is
// all-or-nothing and shipBatch drops 4xx WITHOUT RETRYING (see shipper.go), one
// rejected id destroys the whole batch — so a service on a build whose ids
// monitor-core does not accept loses 100% of its events, with one line on
// stderr and nothing in Monitor to say so. That is not hypothetical: the 32-bit
// form shipped while monitor-core still required a hyphenated UUID.
//
// ids_test.go asserts every id this file mints against monitor-core's exact
// regex. If you change the format, that test is the thing that has to pass —
// and monitor-core must be deployed with the wider rule BEFORE this ships.
// There is no ordering in which the reverse is safe.
const SHORT_ID_BYTES = 8

// generateShortID creates a compact, log-friendly random ID — the process-level
// job_id and the per-request request_id — where a 16-character token is
// preferable to a full 36-character UUID.
func generateShortID() string {
	b := make([]byte, SHORT_ID_BYTES)
	if _, err := rand.Read(b); err != nil {
		panic("monitor: failed to generate random ID: " + err.Error())
	}
	return fmt.Sprintf("%x", b)
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
