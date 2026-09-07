package monitor

import (
	"regexp"
	"testing"
)

// monitorCoreCorrelationIDRegex is a VERBATIM COPY of
// structs.correlationIDRegex in monitor-core. It is duplicated rather than
// imported on purpose: go-monitor must not depend on the server it ships to, and
// the whole point is to detect the two drifting apart.
//
// ⚠️ If this test fails, DO NOT relax it to match the code. It is asserting the
// contract that keeps events alive. monitor-core rejects anything outside this
// shape, ingest is all-or-nothing, and shipBatch drops a 4xx without retrying —
// so a mismatch means every service on this build loses 100% of its events, with
// one line on stderr and nothing in Monitor. Widen monitor-core FIRST, deploy it,
// then update this copy.
var monitorCoreCorrelationIDRegex = regexp.MustCompile(
	`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|[0-9a-fA-F]{8,64})$`)

// TestGeneratedIDsAreAcceptedByMonitorCore is the guard that was missing when the
// 32-bit id shipped against a server that required a hyphenated UUID.
func TestGeneratedIDsAreAcceptedByMonitorCore(t *testing.T) {
	for _, tc := range []struct {
		name string
		gen  func() string
	}{
		{"generateID (trace_id)", generateID},
		{"generateUUID", generateUUID},
		{"generateShortID (job_id, request_id)", generateShortID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Many samples: a format bug that only shows on a leading zero byte
			// would otherwise pass most of the time. %x without zero-padding is
			// exactly that bug.
			for i := 0; i < 2000; i++ {
				id := tc.gen()
				if !monitorCoreCorrelationIDRegex.MatchString(id) {
					t.Fatalf("%s produced %q, which monitor-core REJECTS — every event from a service on this build would be dropped", tc.name, id)
				}
			}
		})
	}
}

// TestShortIDIsWideEnough pins the collision argument. At 32 bits the birthday
// bound sits around 77,000 ids, which a per-request id reaches in days.
func TestShortIDIsWideEnough(t *testing.T) {
	if SHORT_ID_BYTES < 8 {
		t.Fatalf("SHORT_ID_BYTES = %d; anything under 8 collides within days at real request volume", SHORT_ID_BYTES)
	}
	if got := len(generateShortID()); got != SHORT_ID_BYTES*2 {
		t.Errorf("generateShortID returned %d chars, want %d — a non-padded format verb drops leading zeros", got, SHORT_ID_BYTES*2)
	}
}
