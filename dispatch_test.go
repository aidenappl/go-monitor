package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withStdoutBuffer redirects the dispatch stdout branch to a buffer for the
// duration of the test and restores the original writer afterwards.
func withStdoutBuffer(t *testing.T) *bytes.Buffer {
	t.Helper()
	orig := stdoutWriter
	buf := &bytes.Buffer{}
	stdoutWriter = buf
	t.Cleanup(func() { stdoutWriter = orig })
	return buf
}

// TestDispatchWritesToStdout verifies B1: with DisableStdout=false the event is
// actually written as an NDJSON line to stdout.
func TestDispatchWritesToStdout(t *testing.T) {
	buf := withStdoutBuffer(t)

	if err := Init(Config{Service: "stdout-test", DisableStdout: false, JobID: "job-1"}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	Emit(context.Background(), "test.stdout", map[string]any{"k": "v"})

	out := buf.String()
	if out == "" {
		t.Fatal("expected a line written to stdout, got nothing")
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("expected trailing newline, got %q", out)
	}

	var ev Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &ev); err != nil {
		t.Fatalf("stdout line is not valid JSON: %v (%q)", err, out)
	}
	if ev.Name != "test.stdout" {
		t.Errorf("event name = %q, want test.stdout", ev.Name)
	}
	if ev.Service != "stdout-test" {
		t.Errorf("event service = %q, want stdout-test", ev.Service)
	}
}

// TestDispatchRespectsDisableStdout confirms nothing is written when disabled.
func TestDispatchRespectsDisableStdout(t *testing.T) {
	buf := withStdoutBuffer(t)

	if err := Init(Config{Service: "stdout-off", DisableStdout: true}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	Emit(context.Background(), "test.silent", nil)

	if buf.Len() != 0 {
		t.Errorf("expected no stdout output when DisableStdout=true, got %q", buf.String())
	}
}

// TestMiddlewarePanicRecovery verifies B3: a panicking handler still produces an
// http.request event (with panic + stack) and the panic propagates upstream.
func TestMiddlewarePanicRecovery(t *testing.T) {
	buf := withStdoutBuffer(t)

	if err := Init(Config{Service: "panic-test", DisableStdout: false, JobID: "job-p"}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	mw := MiddlewareWithConfig(MiddlewareConfig{})
	wrapped := mw(handler)

	req := httptest.NewRequest("GET", "/panic", nil)
	rec := httptest.NewRecorder()

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		wrapped.ServeHTTP(rec, req)
	}()

	if recovered == nil {
		t.Fatal("expected panic to propagate, but it was swallowed")
	}
	if recovered != "boom" {
		t.Errorf("propagated panic = %v, want boom", recovered)
	}

	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Fatal("expected an http.request event to be emitted on panic")
	}

	var ev Event
	if err := json.Unmarshal([]byte(out), &ev); err != nil {
		t.Fatalf("emitted event is not valid JSON: %v (%q)", err, out)
	}
	if ev.Name != "http.request" {
		t.Errorf("event name = %q, want http.request", ev.Name)
	}
	if ev.Level != LevelError {
		t.Errorf("event level = %q, want error", ev.Level)
	}

	data, ok := ev.Data.(map[string]any)
	if !ok {
		t.Fatalf("event data is not a map: %T", ev.Data)
	}
	if data["panic"] != "boom" {
		t.Errorf("data[panic] = %v, want boom", data["panic"])
	}
	if stack, _ := data["stack"].(string); stack == "" {
		t.Error("expected a non-empty stack in the panic event")
	}
	if status, _ := data["response_status"].(float64); int(status) != http.StatusInternalServerError {
		t.Errorf("data[response_status] = %v, want 500", data["response_status"])
	}
}

// TestShipperStopIsIdempotent verifies B5: stop() can be called multiple times
// without a double-close panic.
func TestShipperStopIsIdempotent(t *testing.T) {
	cfg := &Config{
		Service:       "stop-test",
		IngestURL:     "http://127.0.0.1:0/never",
		BatchSize:     10,
		FlushEvery:    time.Second,
		DisableStdout: true,
	}

	s := newShipper(cfg)
	s.start()

	// Calling stop twice must not panic (previously double-closed stopCh).
	s.stop()
	s.stop()
}
