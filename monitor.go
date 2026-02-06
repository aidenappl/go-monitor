package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// Config holds the configuration for the monitor.
type Config struct {
	// Service is the name of the service emitting events. Required.
	Service string

	// Env is the environment (e.g., "prod", "staging", "dev"). Optional.
	Env string

	// JobID is an optional override for the process-level job ID.
	// If empty, one will be auto-generated.
	JobID string

	// IngestURL is the URL to POST NDJSON batches to.
	// If empty, the async shipper is disabled and events only go to stdout.
	IngestURL string

	// APIKey is an optional API key for authenticating with the ingest endpoint.
	APIKey string

	// BatchSize is the maximum number of events per batch. Default: 200.
	BatchSize int

	// FlushEvery is how often to flush batches. Default: 1s.
	FlushEvery time.Duration

	// GzipEnabled enables gzip compression for shipped batches. Default: false.
	GzipEnabled bool

	// DisableStdout disables printing events to stdout. Default: false.
	DisableStdout bool
}

// globalConfig stores the initialized configuration atomically.
var globalConfig atomic.Pointer[Config]

// globalShipper stores the active shipper (if any).
var globalShipper atomic.Pointer[shipper]

// ErrNotInitialized is returned when Emit is called before Init.
var ErrNotInitialized = errors.New("monitor: not initialized, call Init first")

// ErrServiceRequired is returned when Config.Service is empty.
var ErrServiceRequired = errors.New("monitor: Config.Service is required")

// Init initializes the monitor with the given configuration.
// Must be called before Emit. Can be called multiple times to reconfigure.
func Init(cfg Config) error {
	if cfg.Service == "" {
		return ErrServiceRequired
	}

	// Apply defaults
	if cfg.JobID == "" {
		cfg.JobID = generateShortID()
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 200
	}
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = time.Second
	}

	// Stop existing shipper if any
	if oldShipper := globalShipper.Load(); oldShipper != nil {
		oldShipper.stop()
	}

	// Store the config
	globalConfig.Store(&cfg)

	// Start shipper if IngestURL is configured
	if cfg.IngestURL != "" {
		s := newShipper(&cfg)
		globalShipper.Store(s)
		s.start()
	} else {
		globalShipper.Store(nil)
	}

	return nil
}

// EmitOption is a functional option for Emit.
type EmitOption func(*emitOptions)

type emitOptions struct {
	level string
}

// WithLevel sets the log level for the event.
func WithLevel(level string) EmitOption {
	return func(o *emitOptions) {
		o.level = level
	}
}

// Emit emits a monitoring event with the given name and data.
// The event will always contain: job_id, request_id, trace_id, service, timestamp.
// If any ID is missing from the context, it will be generated.
func Emit(ctx context.Context, name string, data any, opts ...EmitOption) {
	cfg := globalConfig.Load()
	if cfg == nil {
		// Silently ignore if not initialized to avoid breaking apps
		// In debug mode, you might want to log this
		return
	}

	// Apply options
	o := &emitOptions{level: "info"}
	for _, opt := range opts {
		opt(o)
	}

	// Create the event
	event := newEvent(ctx, name, data, o.level)

	// Output to stdout (NDJSON)
	if !cfg.DisableStdout {
		jsonBytes, err := event.ToJSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "monitor: failed to marshal event: %v\n", err)
			return
		}
		fmt.Println(string(jsonBytes))
	}

	// Send to shipper if configured
	if s := globalShipper.Load(); s != nil {
		s.send(event)
	}
}

// Flush flushes any buffered events to the ingest endpoint.
// This is useful to call before application shutdown.
func Flush() {
	if s := globalShipper.Load(); s != nil {
		s.flush()
	}
}

// Shutdown gracefully shuts down the monitor, flushing any remaining events.
func Shutdown() {
	if s := globalShipper.Load(); s != nil {
		s.stop()
		globalShipper.Store(nil)
	}
}
