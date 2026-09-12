package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// stdoutWriter is the destination for the stdout branch of dispatchEvent.
// It defaults to os.Stdout and is only overridden in tests.
var stdoutWriter io.Writer = os.Stdout

// stderrWriter is the destination for the SDK's own diagnostics — dropped
// events, ship failures, the zone assertion. It defaults to os.Stderr and is
// only overridden in tests. Routing them through one variable is what lets a
// test assert that the SDK complained, instead of trusting that it did.
var stderrWriter io.Writer = os.Stderr

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
	// It must be minted ON the zone IngestURL points at: the server derives the
	// project (and therefore the tenant) from the api_keys row behind this key,
	// so a key from another zone or from the control plane files this service's
	// events under someone else's project without erroring anywhere.
	APIKey string

	// Zone is the Monitor zone this service expects to be reporting into
	// (e.g. "trailblaze", "appleby"). Optional; empty disables the check.
	//
	// It is NEVER sent on the wire. Tenancy is stamped server-side and a client
	// cannot influence it — this is purely a startup assertion. At Init the SDK
	// asks the ingest origin's /health which zone it actually is and complains
	// loudly if the answer disagrees, because a service pointed at the wrong
	// zone otherwise reports there silently and permanently, and misattributed
	// telemetry cannot be unmixed after the fact.
	Zone string

	// DisableZoneVerify skips the Init-time zone assertion even when Zone is set.
	// Default: false (the check is on whenever Zone is set).
	DisableZoneVerify bool

	// BatchSize is the maximum number of events per batch. Default: 200.
	BatchSize int

	// FlushEvery is how often to flush batches. Default: 1s.
	FlushEvery time.Duration

	// GzipEnabled enables gzip compression for shipped batches. Default: false.
	GzipEnabled bool

	// DisableStdout disables printing events to stdout. Default: false.
	DisableStdout bool

	// Debug enables debug-level events. Default: false.
	Debug bool

	// CaptureSource enables automatic source location capture. Default: true.
	// Set to false to disable adding source_file, source_line, source_func to events.
	CaptureSource *bool

	// OnDrop is invoked with the shipper's running drop total every time events
	// are lost (full buffer, unserializable event, or a batch abandoned after
	// retries / rejected with a 4xx). Optional.
	//
	// It exists so loss is visible to something other than stderr — the one
	// place nobody watches on the service whose telemetry just stopped arriving.
	//
	// Called synchronously on the goroutine that hit the drop, which for a full
	// buffer is the caller of Emit: bump a counter, do not do I/O or take a lock
	// that anything slow holds, or you have made Emit block — the thing this SDK
	// promises never to do. A panic here is recovered and logged.
	OnDrop func(total int64)

	// SpoolDir enables the durable on-disk spool. Empty (the default) keeps
	// events in memory only, where a Monitor outage longer than the retry
	// window (~7s) loses them.
	//
	// With a spool every batch is written to <SpoolDir>/<Service>/ before it is
	// shipped, and deleted only once ingest accepts it. Monitor can be down for
	// hours — or this process restarted in the meantime — and the events ship
	// when it answers. Init still never touches the network or blocks: this is
	// what lets a service report to a Monitor it may be hosting.
	//
	// One process per directory: a second process on the same path falls back
	// to memory with a warning. The directory must survive restarts to be
	// useful — a host path or a named volume, never a container's writable
	// layer, which is destroyed by the image-update redeploy it would need to
	// survive.
	SpoolDir string

	// SpoolMaxBytes caps the spool on disk. Default: 64 MiB. When full, the
	// oldest events are evicted (and counted as dropped) to make room.
	SpoolMaxBytes int64

	// SpoolMinFreeBytes is the free-disk floor: below it nothing more is
	// spooled and new events are dropped (and counted). Default: 512 MiB;
	// negative disables the check. A telemetry buffer that fills the disk of
	// the host it reports on causes the outage it was meant to record.
	SpoolMinFreeBytes int64

	// SpoolSyncEvery is how often spooled writes are fsynced. Default: 500ms.
	// A process crash loses nothing either way; this bounds what a host crash
	// can lose.
	SpoolSyncEvery time.Duration

	// MaxBackoff caps the delay between delivery attempts of spooled events.
	// Default: 5m.
	MaxBackoff time.Duration

	// DrainRate is the maximum number of requests per second used to deliver
	// spooled events. Default: 5 — with the default BatchSize, up to 1,000
	// events/s. It keeps a recovering fleet from hitting Monitor with every
	// backlog at once.
	DrainRate float64

	// RedactKeys adds data keys whose values are always replaced with
	// [REDACTED], on top of the built-in credential keys (password, secret,
	// token, api_key, authorization, cookie, dsn, …). Matching ignores case
	// and '_', '-', '.' separators.
	RedactKeys []string

	// RedactAllowKeys exempts data keys from key-based redaction — for a key
	// that names a credential without holding one, like a secrets manager's
	// "secret_key" (the NAME of a secret). Values are still scanned for
	// credential patterns.
	RedactAllowKeys []string

	// DisableRedaction turns off redaction entirely. There is no good reason
	// to set this outside a test. Error values are still converted to their
	// messages, which encoding/json would otherwise render as {}.
	DisableRedaction bool
}

// globalConfig stores the initialized configuration atomically.
var globalConfig atomic.Pointer[Config]

// globalShipper stores the active shipper (if any).
var globalShipper atomic.Pointer[shipper]

// globalRedactor is the redactor built from the active config.
var globalRedactor atomic.Pointer[redactor]

// defaultRedactor applies the built-in rules when no config is active (a test
// recording without Init).
var defaultRedactor = newRedactor(nil)

// currentRedactor returns the redactor for cfg, or nil if redaction is off.
func currentRedactor(cfg *Config) *redactor {
	if cfg == nil {
		return defaultRedactor
	}
	if cfg.DisableRedaction {
		return passthroughRedactor
	}
	if r := globalRedactor.Load(); r != nil {
		return r
	}
	return defaultRedactor
}

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
	} else if !ValidCorrelationID(cfg.JobID) {
		// Every event carries this id, so an invalid one would be cleared from
		// every event this process ever sends. Replace it once, loudly.
		fmt.Fprintf(stderrWriter, "monitor: Config.JobID %q is not an id monitor-core accepts (a UUID or 8-64 hex characters); using a generated one instead\n", cfg.JobID)
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
	globalRedactor.Store(newRedactor(&cfg))
	globalConfig.Store(&cfg)

	// Start shipper if IngestURL is configured
	if cfg.IngestURL != "" {
		s := newShipper(&cfg)
		globalShipper.Store(s)
		s.start()
	} else {
		globalShipper.Store(nil)
	}

	// Assert the zone last and off this goroutine: the shipper is already up, so
	// a slow or unreachable /health can neither delay the service's startup nor
	// hold back a single event.
	if cfg.Zone != "" && cfg.IngestURL != "" && !cfg.DisableZoneVerify {
		startZoneVerification(&cfg)
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

// ResolvedEmitOptions is the effective configuration of a single Emit call,
// after every EmitOption has been applied.
type ResolvedEmitOptions struct {
	// Level is the event level, LevelInfo when no option set one.
	Level string
}

// ResolveEmitOptions applies opts and reports the settings Emit would use.
//
// EmitOption is a function over an unexported struct, so wrappers around this
// package cannot otherwise inspect what an option did. This exists so they can
// — most usefully, so a test recorder can assert the level an event was emitted
// at, not just its name.
func ResolveEmitOptions(opts ...EmitOption) ResolvedEmitOptions {
	o := &emitOptions{level: LevelInfo}
	for _, opt := range opts {
		opt(o)
	}
	return ResolvedEmitOptions{Level: o.level}
}

// captureSourceEnabled returns true if source capture is enabled in the config.
// Defaults to true when CaptureSource is nil (not explicitly set).
func captureSourceEnabled(cfg *Config) bool {
	if cfg.CaptureSource == nil {
		return true
	}
	return *cfg.CaptureSource
}

// attachSourceLocation adds source_file, source_line, source_func to the event's
// data map using runtime.Caller at the given depth.
func attachSourceLocation(event *Event, callerDepth int) {
	pc, file, line, ok := runtime.Caller(callerDepth)
	if !ok {
		return
	}

	funcName := runtime.FuncForPC(pc).Name()
	// Extract last segment after the final dot (e.g., "monitor.Info" -> "Info")
	if idx := strings.LastIndex(funcName, "."); idx >= 0 {
		funcName = funcName[idx+1:]
	}

	// Merge source fields into data
	dataMap, ok := event.Data.(map[string]any)
	if !ok || dataMap == nil {
		dataMap = make(map[string]any)
		if event.Data != nil && !ok {
			dataMap["_data"] = event.Data
		}
	}
	dataMap["source_file"] = filepath.Base(file)
	dataMap["source_line"] = line
	dataMap["source_func"] = funcName

	event.Data = dataMap
}

// Emit emits a monitoring event with the given name and data.
// The event will always contain: job_id, request_id, trace_id, service, timestamp.
// If any ID is missing from the context, it will be generated.
func Emit(ctx context.Context, name string, data any, opts ...EmitOption) {
	cfg := globalConfig.Load()
	if cfg == nil && activeRecorder.Load() == nil {
		return
	}

	// Apply options — shared with ResolveEmitOptions so the two can never drift.
	o := ResolveEmitOptions(opts...)

	// Create the event
	event := newEvent(ctx, name, data, o.Level)

	// Attach source location if enabled
	if cfg == nil || captureSourceEnabled(cfg) {
		attachSourceLocation(&event, 2)
	}

	dispatchEvent(event)
}

// emitWithCallerDepth is used by convenience functions (Info, Warn, etc.) to emit
// events with the correct caller depth for source location capture.
func emitWithCallerDepth(ctx context.Context, name string, data any, level string, callerDepth int) {
	cfg := globalConfig.Load()
	if cfg == nil && activeRecorder.Load() == nil {
		return
	}

	event := newEvent(ctx, name, data, level)

	if cfg == nil || captureSourceEnabled(cfg) {
		attachSourceLocation(&event, callerDepth+1)
	}

	dispatchEvent(event)
}

// dispatchEvent sanitizes and redacts an event, then prints and ships it.
//
// Sanitizing here, before stdout, the recorder and the shipper, means every
// destination sees the same event — and no destination ever sees a secret the
// others did not.
func dispatchEvent(event Event) {
	cfg := globalConfig.Load()
	rec := activeRecorder.Load()
	if cfg == nil && rec == nil {
		return
	}
	sanitizeEvent(&event, currentRedactor(cfg))
	if rec != nil {
		rec.record(event)
		return
	}
	if !cfg.DisableStdout {
		jsonBytes, err := event.ToJSON()
		if err != nil {
			fmt.Fprintf(stderrWriter, "monitor: failed to marshal event: %v\n", err)
			return
		}
		// Write the NDJSON line (payload + trailing newline) in a single Write.
		if _, err := stdoutWriter.Write(append(jsonBytes, '\n')); err != nil {
			fmt.Fprintf(stderrWriter, "monitor: failed to write event to stdout: %v\n", err)
		}
	}
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
