package monitor

import "testing"

// TestSDKPatternIsMonitorCores pins the SDK's own validator to the independent
// copy of monitor-core's regex in ids_test.go. If this fails, the SDK is
// clearing ids the server would accept, or passing ids it would reject.
func TestSDKPatternIsMonitorCores(t *testing.T) {
	if correlationIDRegex.String() != monitorCoreCorrelationIDRegex.String() {
		t.Fatalf("SDK pattern %q drifted from monitor-core's %q", correlationIDRegex, monitorCoreCorrelationIDRegex)
	}
}

func TestValidCorrelationID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"", true},
		{"0123456789abcdef", true},
		{"deadbeef", true},
		{"11111111-2222-4333-8444-555555555555", true},
		{"ABCDEF0123456789", true},
		{"req-123", false},
		{"lattice-runner-1", false},
		{"a1b2c3", false},
		{"0123456789abcdef ", false},
		{"1' OR '1'='1", false},
	} {
		if got := ValidCorrelationID(tc.id); got != tc.want {
			t.Errorf("ValidCorrelationID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestExportedGeneratorsMintValidIDs(t *testing.T) {
	for name, gen := range map[string]func() string{
		"NewRequestID": NewRequestID,
		"NewJobID":     NewJobID,
		"NewTraceID":   NewTraceID,
	} {
		for i := 0; i < 500; i++ {
			if id := gen(); id == "" || !ValidCorrelationID(id) {
				t.Fatalf("%s() = %q, not accepted by monitor-core", name, id)
			}
		}
	}
}
