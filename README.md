# go-monitor

A lightweight, structured event monitoring library for Go services with guaranteed fields on every event.

## Features

- **Guaranteed fields**: Every event contains `job_id`, `request_id`, `trace_id`, `service`, and `timestamp`
- **Context-aware**: IDs flow through request contexts automatically
- **HTTP middleware**: Gorilla mux compatible middleware that ensures request tracing
- **NDJSON output**: Events are printed as newline-delimited JSON to stdout
- **Optional async shipping**: Batch events and POST to an ingest URL with gzip support
- **Zero dependencies**: Uses only the Go standard library (except for the example)

## Installation

```bash
go get github.com/aidenappl/go-monitor
```

## Quick Start

```go
package main

import (
    "context"
    "net/http"

    monitor "github.com/aidenappl/go-monitor"
    "github.com/gorilla/mux"
)

func main() {
    // Initialize the monitor
    monitor.Init(monitor.Config{
        Service: "my-service",
        Env:     "prod",
    })
    defer monitor.Shutdown()

    // Emit a startup event
    monitor.Emit(context.Background(), "service.startup", map[string]any{
        "version": "1.0.0",
    })

    // Setup router with middleware
    r := mux.NewRouter()
    r.Use(monitor.Middleware)

    r.HandleFunc("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
        // IDs are automatically available from context
        monitor.Emit(r.Context(), "user.get", map[string]any{
            "user_id": mux.Vars(r)["id"],
        })
        w.Write([]byte("OK"))
    })

    http.ListenAndServe(":8080", r)
}
```

## Configuration

```go
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
```

## Event Schema

Every event has these fields. At least one of `job_id`, `request_id`, or `trace_id` should be present:

```json
{
  "timestamp": "2024-01-15T10:30:00.123456789Z",
  "service": "my-service",
  "env": "prod",
  "job_id": "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d",
  "request_id": "f0e1d2c3-b4a5-4968-8c7d-6e5f4a3b2c1d",
  "trace_id": "01234567-89ab-4cde-8f01-23456789abcd",
  "user_id": "user-12345",
  "name": "user.created",
  "level": "info",
  "data": {
    "email": "user@example.com"
  }
}
```

| Field        | Type   | Description                             |
| ------------ | ------ | --------------------------------------- |
| `timestamp`  | string | RFC3339Nano formatted UTC timestamp     |
| `service`    | string | Service name from config                |
| `env`        | string | Environment from config (optional)      |
| `job_id`     | string | Process-level identifier (optional)     |
| `request_id` | string | Request-scoped identifier (optional)    |
| `trace_id`   | string | Distributed trace identifier (optional) |
| `user_id`    | string | User identifier (optional)              |
| `name`       | string | Event name (e.g., "user.created")       |
| `level`      | string | Log level (default: "info")             |
| `data`       | object | Arbitrary event data                    |

**Note:** The middleware auto-generates `request_id` and `trace_id` for HTTP requests. For non-HTTP events, set them via context or they will be omitted.

## API Reference

### Initialization

```go
// Initialize the monitor (required before Emit)
err := monitor.Init(monitor.Config{
    Service: "my-service",
})

// Gracefully shutdown (flushes remaining events)
monitor.Shutdown()

// Manual flush
monitor.Flush()
```

### Emitting Events

```go
// Basic emit
monitor.Emit(ctx, "event.name", map[string]any{"key": "value"})

// With custom level
monitor.Emit(ctx, "error.occurred", data, monitor.WithLevel("error"))
```

### Context Helpers

```go
// Set IDs in context
ctx = monitor.WithJobID(ctx, "job-123")
ctx = monitor.WithRequestID(ctx, "req-456")
ctx = monitor.WithTraceID(ctx, "trace-789")
ctx = monitor.WithUserID(ctx, "user-abc")

// Get IDs from context
jobID := monitor.JobID(ctx)
requestID := monitor.RequestID(ctx)
traceID := monitor.TraceID(ctx)
userID := monitor.UserID(ctx)
```

### HTTP Middleware

The middleware is compatible with `net/http` and gorilla/mux:

```go
r := mux.NewRouter()
r.Use(monitor.Middleware)
```

The middleware:

- Reads `X-Request-Id` and `X-Trace-Id` headers if present
- Generates new IDs if headers are missing
- Stores IDs in the request context
- Sets response headers `X-Request-Id` and `X-Trace-Id`

## Async Shipping

When `IngestURL` is configured, events are batched and shipped asynchronously:

```go
monitor.Init(monitor.Config{
    Service:     "my-service",
    IngestURL:   "https://ingest.example.com/events",
    APIKey:      "your-api-key",
    BatchSize:   200,        // Events per batch
    FlushEvery:  time.Second, // Flush interval
    GzipEnabled: true,       // Compress batches
})
```

The shipper:

- Buffers events in memory
- Flushes when batch size is reached or flush interval elapses
- Sends NDJSON payloads via HTTP POST
- Uses `Authorization: Bearer <api-key>` if APIKey is set
- Supports gzip compression

## License

MIT
