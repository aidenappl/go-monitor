package monitor

import (
	"context"
	"encoding/json"
	"time"
)

// Event represents a single monitoring event.
// At least one of job_id, request_id, or trace_id should be present.
type Event struct {
	Timestamp string `json:"timestamp"`
	Service   string `json:"service"`
	Env       string `json:"env,omitempty"`
	JobID     string `json:"job_id,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	TraceID   string `json:"trace_id,omitempty"`
	Name      string `json:"name"`
	Level     string `json:"level"`
	Data      any    `json:"data,omitempty"`
}

// newEvent creates a new Event with required fields populated.
// IDs are taken from context or global config but not auto-generated.
func newEvent(ctx context.Context, name string, data any, level string) Event {
	cfg := globalConfig.Load()

	// Get IDs from context, fall back to global job ID only
	jobID := JobID(ctx)
	if jobID == "" && cfg != nil {
		jobID = cfg.JobID
	}

	requestID := RequestID(ctx)
	traceID := TraceID(ctx)

	service := ""
	env := ""
	if cfg != nil {
		service = cfg.Service
		env = cfg.Env
	}

	if level == "" {
		level = "info"
	}

	return Event{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Service:   service,
		Env:       env,
		JobID:     jobID,
		RequestID: requestID,
		TraceID:   traceID,
		Name:      name,
		Level:     level,
		Data:      data,
	}
}

// MarshalJSON implements json.Marshaler for Event.
func (e Event) MarshalJSON() ([]byte, error) {
	type EventAlias Event
	return json.Marshal(EventAlias(e))
}

// ToJSON returns the event as a JSON byte slice.
func (e Event) ToJSON() ([]byte, error) {
	return json.Marshal(e)
}
