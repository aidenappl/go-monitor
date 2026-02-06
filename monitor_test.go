package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInit(t *testing.T) {
	t.Run("missing service", func(t *testing.T) {
		err := Init(Config{})
		if err != ErrServiceRequired {
			t.Errorf("Init() error = %v, want ErrServiceRequired", err)
		}
	})

	t.Run("valid config", func(t *testing.T) {
		err := Init(Config{Service: "test-service"})
		if err != nil {
			t.Errorf("Init() error = %v, want nil", err)
		}
	})

	t.Run("valid config with all fields", func(t *testing.T) {
		err := Init(Config{Service: "test-service", Env: "test", JobID: "custom-job-id"})
		if err != nil {
			t.Errorf("Init() error = %v, want nil", err)
		}
	})
}

func TestContextHelpers(t *testing.T) {
	ctx := context.Background()

	// Test empty context
	if got := JobID(ctx); got != "" {
		t.Errorf("JobID(empty ctx) = %v, want empty", got)
	}
	if got := RequestID(ctx); got != "" {
		t.Errorf("RequestID(empty ctx) = %v, want empty", got)
	}
	if got := TraceID(ctx); got != "" {
		t.Errorf("TraceID(empty ctx) = %v, want empty", got)
	}
	if got := UserID(ctx); got != "" {
		t.Errorf("UserID(empty ctx) = %v, want empty", got)
	}

	// Test with values
	ctx = WithJobID(ctx, "job-123")
	ctx = WithRequestID(ctx, "req-456")
	ctx = WithTraceID(ctx, "trace-789")
	ctx = WithUserID(ctx, "user-abc")

	if got := JobID(ctx); got != "job-123" {
		t.Errorf("JobID() = %v, want job-123", got)
	}
	if got := RequestID(ctx); got != "req-456" {
		t.Errorf("RequestID() = %v, want req-456", got)
	}
	if got := TraceID(ctx); got != "trace-789" {
		t.Errorf("TraceID() = %v, want trace-789", got)
	}
	if got := UserID(ctx); got != "user-abc" {
		t.Errorf("UserID() = %v, want user-abc", got)
	}
}

func TestGenerateID(t *testing.T) {
	id := generateID()
	// UUID format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx (36 chars)
	if len(id) != 36 {
		t.Errorf("generateID() length = %d, want 36 (UUID format)", len(id))
	}
	// Check UUID format with hyphens
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Errorf("generateID() = %v, want UUID format", id)
	}

	// Should be unique
	id2 := generateID()
	if id == id2 {
		t.Error("generateID() should generate unique IDs")
	}
}

func TestGenerateShortID(t *testing.T) {
	id := generateShortID()
	// Now uses same UUID format as generateID
	if len(id) != 36 {
		t.Errorf("generateShortID() length = %d, want 36 (UUID format)", len(id))
	}

	// Should be unique
	id2 := generateShortID()
	if id == id2 {
		t.Error("generateShortID() should generate unique IDs")
	}
}

func TestEvent(t *testing.T) {
	// Initialize monitor first
	if err := Init(Config{Service: "test-service", Env: "test", JobID: "test-job"}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := context.Background()
	ctx = WithRequestID(ctx, "req-123")
	ctx = WithTraceID(ctx, "trace-456")

	event := newEvent(ctx, "test.event", map[string]any{"key": "value"}, "info")

	// Check required fields
	if event.Service != "test-service" {
		t.Errorf("event.Service = %v, want test-service", event.Service)
	}
	if event.Env != "test" {
		t.Errorf("event.Env = %v, want test", event.Env)
	}
	if event.JobID != "test-job" {
		t.Errorf("event.JobID = %v, want test-job", event.JobID)
	}
	if event.RequestID != "req-123" {
		t.Errorf("event.RequestID = %v, want req-123", event.RequestID)
	}
	if event.TraceID != "trace-456" {
		t.Errorf("event.TraceID = %v, want trace-456", event.TraceID)
	}
	if event.Name != "test.event" {
		t.Errorf("event.Name = %v, want test.event", event.Name)
	}
	if event.Level != "info" {
		t.Errorf("event.Level = %v, want info", event.Level)
	}
	if event.Timestamp == "" {
		t.Error("event.Timestamp should not be empty")
	}

	// Check JSON marshaling
	jsonBytes, err := event.ToJSON()
	if err != nil {
		t.Errorf("event.ToJSON() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Errorf("json.Unmarshal() error = %v", err)
	}

	// Verify all required fields are in JSON
	requiredFields := []string{"timestamp", "service", "name", "level"}
	for _, field := range requiredFields {
		if _, ok := decoded[field]; !ok {
			t.Errorf("JSON missing required field: %s", field)
		}
	}

	// IDs should be present since they were set in context
	idFields := []string{"job_id", "request_id", "trace_id"}
	for _, field := range idFields {
		if _, ok := decoded[field]; !ok {
			t.Errorf("JSON missing ID field: %s", field)
		}
	}
}

func TestEventMissingIDs(t *testing.T) {
	if err := Init(Config{Service: "test-service", JobID: "global-job"}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// Event with no IDs in context should have job_id from config only
	ctx := context.Background()
	event := newEvent(ctx, "test.event", nil, "")

	if event.JobID != "global-job" {
		t.Errorf("event.JobID = %v, want global-job (from config)", event.JobID)
	}
	// request_id and trace_id should be empty when not in context
	if event.RequestID != "" {
		t.Errorf("event.RequestID = %v, want empty", event.RequestID)
	}
	if event.TraceID != "" {
		t.Errorf("event.TraceID = %v, want empty", event.TraceID)
	}
	if event.Level != "info" {
		t.Errorf("event.Level = %v, want info (default)", event.Level)
	}
}

func TestEventOmitsEmptyIDs(t *testing.T) {
	if err := Init(Config{Service: "test-service", JobID: "only-job"}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := context.Background()
	event := newEvent(ctx, "test.event", nil, "info")

	jsonBytes, err := event.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	// job_id should be present (from config)
	if _, ok := decoded["job_id"]; !ok {
		t.Error("JSON should have job_id from config")
	}

	// request_id and trace_id should be omitted (empty)
	if _, ok := decoded["request_id"]; ok {
		t.Error("JSON should omit empty request_id")
	}
	if _, ok := decoded["trace_id"]; ok {
		t.Error("JSON should omit empty trace_id")
	}
}

func TestMiddleware(t *testing.T) {
	if err := Init(Config{Service: "test-service", JobID: "middleware-test-job"}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// Create a test handler that checks context values
	var gotRequestID, gotTraceID, gotJobID string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestID = RequestID(r.Context())
		gotTraceID = TraceID(r.Context())
		gotJobID = JobID(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	// Wrap with middleware
	wrapped := Middleware(handler)

	t.Run("generates IDs when not present", func(t *testing.T) {
		gotRequestID, gotTraceID, gotJobID = "", "", ""

		req := httptest.NewRequest("GET", "/test", nil)
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)

		if gotRequestID == "" {
			t.Error("RequestID should be generated")
		}
		if gotTraceID == "" {
			t.Error("TraceID should be generated")
		}
		if gotJobID != "middleware-test-job" {
			t.Errorf("JobID = %v, want middleware-test-job", gotJobID)
		}

		// Check response headers
		if rec.Header().Get(HeaderRequestID) == "" {
			t.Error("Response should have X-Request-Id header")
		}
		if rec.Header().Get(HeaderTraceID) == "" {
			t.Error("Response should have X-Trace-Id header")
		}
	})

	t.Run("uses existing IDs from headers", func(t *testing.T) {
		gotRequestID, gotTraceID, gotJobID = "", "", ""

		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set(HeaderRequestID, "incoming-request-id")
		req.Header.Set(HeaderTraceID, "incoming-trace-id")
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)

		if gotRequestID != "incoming-request-id" {
			t.Errorf("RequestID = %v, want incoming-request-id", gotRequestID)
		}
		if gotTraceID != "incoming-trace-id" {
			t.Errorf("TraceID = %v, want incoming-trace-id", gotTraceID)
		}

		// Response headers should match
		if rec.Header().Get(HeaderRequestID) != "incoming-request-id" {
			t.Error("Response X-Request-Id should match incoming")
		}
		if rec.Header().Get(HeaderTraceID) != "incoming-trace-id" {
			t.Error("Response X-Trace-Id should match incoming")
		}
	})
}

func TestEmitWithLevel(t *testing.T) {
	if err := Init(Config{Service: "test-service", DisableStdout: true}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := context.Background()

	// Just verify it doesn't panic - actual output would go to stdout
	Emit(ctx, "test.info", nil)
	Emit(ctx, "test.error", nil, WithLevel("error"))
	Emit(ctx, "test.warn", map[string]any{"warning": true}, WithLevel("warn"))
}

func TestEmitBeforeInit(t *testing.T) {
	// Reset global config
	globalConfig.Store(nil)

	// Should not panic when called before Init
	ctx := context.Background()
	Emit(ctx, "test.event", nil) // Should be a no-op
}

func TestEventJSONFormat(t *testing.T) {
	if err := Init(Config{Service: "json-test", Env: "test", JobID: "json-job"}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := WithRequestID(context.Background(), "json-req")
	ctx = WithTraceID(ctx, "json-trace")

	event := newEvent(ctx, "json.test", map[string]any{
		"string": "value",
		"number": 123,
		"bool":   true,
	}, "info")

	jsonBytes, err := event.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON() error = %v", err)
	}

	jsonStr := string(jsonBytes)

	// Verify it's valid NDJSON (single line, no newlines within)
	if strings.Contains(jsonStr, "\n") {
		t.Error("JSON should not contain newlines (NDJSON format)")
	}

	// Parse and verify structure
	var decoded map[string]any
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	// Check specific values
	if decoded["service"] != "json-test" {
		t.Errorf("service = %v, want json-test", decoded["service"])
	}
	if decoded["job_id"] != "json-job" {
		t.Errorf("job_id = %v, want json-job", decoded["job_id"])
	}
	if decoded["request_id"] != "json-req" {
		t.Errorf("request_id = %v, want json-req", decoded["request_id"])
	}
	if decoded["trace_id"] != "json-trace" {
		t.Errorf("trace_id = %v, want json-trace", decoded["trace_id"])
	}

	// Check data field
	data, ok := decoded["data"].(map[string]any)
	if !ok {
		t.Fatal("data should be an object")
	}
	if data["string"] != "value" {
		t.Errorf("data.string = %v, want value", data["string"])
	}
}
