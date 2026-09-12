package monitor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// resetThrottle clears warnThrottled's memory so a test can assert a warning
// another test already triggered within the last minute.
func resetThrottle(t *testing.T) {
	t.Helper()
	throttleMu.Lock()
	throttleLast = map[string]time.Time{}
	throttleMu.Unlock()
}

func TestSanitizeClearsInvalidCorrelationIDs(t *testing.T) {
	withStderrBuffer(t)
	callerData := map[string]any{"k": "v"}
	e := Event{
		Name:      "x.y.z",
		JobID:     "0123456789abcdef",
		RequestID: "req-123",
		TraceID:   "not-a-trace",
		Data:      callerData,
	}
	sanitizeEvent(&e, nil)

	if e.JobID != "0123456789abcdef" {
		t.Errorf("valid job_id changed to %q", e.JobID)
	}
	if e.RequestID != "" || e.TraceID != "" {
		t.Errorf("invalid ids kept: request_id=%q trace_id=%q", e.RequestID, e.TraceID)
	}
	m := e.Data.(map[string]any)
	if m["invalid_request_id"] != "req-123" || m["invalid_trace_id"] != "not-a-trace" {
		t.Errorf("rejected ids not preserved in data: %v", m)
	}
	if _, touched := callerData["invalid_request_id"]; touched {
		t.Error("sanitizeEvent mutated the caller's data map")
	}
}

func TestSanitizeWarnsAboutInvalidIDs(t *testing.T) {
	buf := withStderrBuffer(t)
	resetThrottle(t)
	e := Event{Name: "a", RequestID: "nope"}
	sanitizeEvent(&e, nil)
	if !strings.Contains(buf.String(), "invalid request_id") {
		t.Errorf("stderr %q should explain the cleared id", buf.String())
	}
}

func TestSanitizeWrapsNonMapDataWhenPreservingAnID(t *testing.T) {
	withStderrBuffer(t)
	e := Event{Name: "a", RequestID: "bad", Data: "plain string"}
	sanitizeEvent(&e, nil)
	m, ok := e.Data.(map[string]any)
	if !ok || m["_data"] != "plain string" || m["invalid_request_id"] != "bad" {
		t.Errorf("Data = %#v, want the original under _data plus invalid_request_id", e.Data)
	}
}

func TestNormalizeLevel(t *testing.T) {
	for in, want := range map[string]string{
		"":        "info",
		"ERROR":   "error",
		"Error":   "error",
		"warning": "warn",
		"WARNING": "warn",
		"fatal":   "fatal",
		"debug":   "debug",
		"notice":  "notice",
	} {
		if got := normalizeLevel(in); got != want {
			t.Errorf("normalizeLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeNamesAndBoundsFields(t *testing.T) {
	e := Event{Data: map[string]any{"path": strings.Repeat("/p", 1000)}}
	sanitizeEvent(&e, nil)
	if e.Name != "event.unnamed" {
		t.Errorf("empty name became %q, want event.unnamed", e.Name)
	}
	if p := e.Data.(map[string]any)["path"].(string); len(p) > maxPathChars {
		t.Errorf("path is %d chars, want at most %d", len(p), maxPathChars)
	}

	long := Event{Name: strings.Repeat("n", 400)}
	sanitizeEvent(&long, nil)
	if len(long.Name) != maxNameChars {
		t.Errorf("name is %d chars, want %d", len(long.Name), maxNameChars)
	}
}

func TestTruncateStringKeepsUTF8Valid(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	got := truncateString(s, 5)
	if !json.Valid([]byte(`"`+got+`"`)) || len(got) > 5 {
		t.Errorf("truncateString = %q (%d bytes), want valid UTF-8 within 5 bytes", got, len(got))
	}
}

func TestMarshalLineShrinksOversizedEvents(t *testing.T) {
	e := Event{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Service:   "big",
		Name:      "report.render.failed",
		Level:     "error",
		Data: map[string]any{
			"error":   strings.Repeat("x", 10_000),
			"path":    "/reports/{id}",
			"payload": strings.Repeat("y", 2_000_000),
		},
	}
	line, err := marshalLine(e)
	if err != nil {
		t.Fatalf("marshalLine: %v", err)
	}
	if len(line) > maxLineBytes {
		t.Fatalf("line is %d bytes, over the %d limit", len(line), maxLineBytes)
	}
	var got Event
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatal(err)
	}
	d := got.Data.(map[string]any)
	if d["truncated"] != true || d["path"] != "/reports/{id}" {
		t.Errorf("shrunk data lost its markers or grouping fields: %v", d)
	}
	if errStr, _ := d["error"].(string); len(errStr) != maxFieldChars {
		t.Errorf("error kept %d chars, want %d", len(errStr), maxFieldChars)
	}
	if _, ok := d["payload"]; ok {
		t.Error("the oversized payload should be gone")
	}
}

func TestInitReplacesAnInvalidJobID(t *testing.T) {
	buf := withStderrBuffer(t)
	if err := Init(Config{Service: "jobid-check", DisableStdout: true, JobID: "lattice-runner-1"}); err != nil {
		t.Fatal(err)
	}
	got := globalConfig.Load().JobID
	if got == "lattice-runner-1" || got == "" || !ValidCorrelationID(got) {
		t.Errorf("JobID = %q, want a generated id monitor-core accepts", got)
	}
	if !strings.Contains(buf.String(), "lattice-runner-1") {
		t.Errorf("stderr %q should name the rejected JobID", buf.String())
	}
}

func TestEmittedEventsCarryOnlyValidIDs(t *testing.T) {
	withStderrBuffer(t)
	rec := recording(t)
	ctx := WithRequestID(context.Background(), "req-from-a-proxy")
	Emit(ctx, "sanitize.e2e", map[string]any{"a": 1})
	evs := rec.Named("sanitize.e2e")
	if len(evs) != 1 {
		t.Fatalf("recorded %d events, want 1", len(evs))
	}
	if evs[0].RequestID != "" {
		t.Errorf("request_id %q reached the wire", evs[0].RequestID)
	}
}
