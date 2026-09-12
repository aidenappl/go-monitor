# go-monitor

A lightweight, structured event monitoring library for Go services with guaranteed fields on every event.

## Features

- **Guaranteed fields**: Every event contains `job_id`, `request_id`, `trace_id`, `service`, and `timestamp`
- **Context-aware**: IDs flow through request contexts automatically
- **HTTP middleware**: Gorilla mux compatible middleware that ensures request tracing
- **NDJSON output**: Events are printed as newline-delimited JSON to stdout
- **Optional async shipping**: Batch events and POST to an ingest URL with gzip support
- **Zone assertion**: Verifies at startup that you're pointed at the zone you think you are
- **Visible loss**: Dropped events are counted (`Stats()`) and reported (`OnDrop`), not just logged
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

    // IngestURL is the URL to POST NDJSON batches to — one zone's full ingest
    // endpoint. If empty, the async shipper is disabled and events only go to stdout.
    IngestURL string

    // APIKey authenticates with the ingest endpoint. Must be minted on the zone
    // IngestURL points at — the server derives the project from this key.
    APIKey string

    // Zone is the zone this service expects to report into. Never sent on the
    // wire; verified once at Init against {ingest origin}/health.
    Zone string

    // DisableZoneVerify opts out of that check. Default: false.
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

    // CaptureSource enables source_file/source_line/source_func capture. Default: true.
    CaptureSource *bool

    // OnDrop is called with the running drop total whenever the shipper loses
    // events. Must not block — see "Dropped events" below.
    OnDrop func(total int64)
}
```

Every field except `Service` is optional and safe at its zero value.

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

// Shipper counters (see "Dropped events")
monitor.Stats() // ShipperStats{Enqueued, Dropped, Flushed}
```

### Emitting Events

```go
// Basic emit
monitor.Emit(ctx, "event.name", map[string]any{"key": "value"})

// With custom level
monitor.Emit(ctx, "error.occurred", data, monitor.WithLevel("error"))
```

#### Inspecting emit options

`EmitOption` is a function over an unexported struct, so wrappers around this
package cannot see what an option did. `ResolveEmitOptions` applies options and
reports the settings `Emit` would use — mainly so a wrapper or test recorder can
assert the **level** an event was emitted at, not just its name.

```go
opts := monitor.ResolveEmitOptions(monitor.WithLevel(monitor.LevelError))
opts.Level // "error"  (LevelInfo when no option sets one)
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
    Zone:        "trailblaze",                                  // asserted at Init, never sent
    IngestURL:   "https://api.monitor.appleby.cloud/v1/events", // that zone's ingest URL
    APIKey:      os.Getenv("MONITOR_API_KEY"),                  // minted on that zone
    BatchSize:   200,         // Events per batch
    FlushEvery:  time.Second, // Flush interval
    GzipEnabled: true,        // Compress batches
})
```

The shipper:

- Buffers events in memory
- Flushes when batch size is reached or flush interval elapses
- Sends NDJSON payloads via HTTP POST
- Sends the `X-Api-Key: <api-key>` header if APIKey is set
- Supports gzip compression

**One process ships to one destination.** The shipper is a single pointer that `Init`
replaces, and that is deliberate: a process belongs to one zone. Calling `Init` again
retargets the whole process rather than adding a second destination — don't build
fan-out on top of it.

## Targeting a zone

Monitor is multi-tenant. **Zones** are isolated deployments (own API process, own
ClickHouse, own MariaDB), each containing **projects**. A service picks its zone by
where it points and what it presents:

| Zone         | `IngestURL`                                          |
| ------------ | ---------------------------------------------------- |
| `trailblaze` | `https://api.monitor.appleby.cloud/v1/events`        |
| `appleby`    | `https://appleby-monitor-api.appleby.cloud/v1/events` |

> `https://monitor.appleby.cloud` is the **web** origin (the dashboard). It ingests
> nothing — don't point `IngestURL` at it.

**The API key is what decides tenancy.** The event has no `project` field and no `zone`
field; the server stamps the project from the `api_keys` row behind your key and
overwrites anything a client sends. So a key minted on the **control plane**, or on
**another zone**, files this service's events under that zone's project — the batch is
accepted, the response is a `200`, and the events show up under the wrong tenant with
nothing wrong anywhere. Use a key minted **on the zone you are pointing at**.

### Zone verification

Set `Zone` and the SDK checks that assumption once at startup, hitting
`{scheme}://{host}/health` on the ingest URL's origin and comparing the `zone` it
reports:

```go
monitor.Init(monitor.Config{
    Service:   "my-service",
    Zone:      "trailblaze",
    IngestURL: "https://api.monitor.appleby.cloud/v1/events",
    APIKey:    os.Getenv("MONITOR_API_KEY"),
})
```

- **Mismatch** → a loud multi-line banner on stderr at startup. Misattributed telemetry
  cannot be moved afterwards, so this is the only moment anyone can catch it.
- **Unreachable, or a `/health` that doesn't name a zone** → one quiet line. The service
  still boots and still ships; a health endpoint being briefly down proves nothing.
- `Init` never fails or blocks on this — the probe runs on its own goroutine with a 3s
  timeout.
- `Zone` is **never** put on the wire. Set `DisableZoneVerify: true` to skip the check.

## Dropped events

The shipper drops events when its buffer is full or when a batch can't be delivered.
Silent loss is worst here — the system that would have told you is the one that broke —
so loss is counted, not just logged:

```go
stats := monitor.Stats()
// ShipperStats{Enqueued: 1042, Dropped: 3, Flushed: 1039}
```

| Field      | Meaning                                                                     |
| ---------- | --------------------------------------------------------------------------- |
| `Enqueued` | Events accepted into the shipper's buffer                                   |
| `Dropped`  | Events lost: full buffer, unserializable, or a batch abandoned / `4xx`-rejected |
| `Flushed`  | Events the ingest endpoint accepted                                          |

`Stats()` returns the zero value when no shipper is running, and counters restart on
re-`Init`. Surface it on your own health endpoint so loss is visible from outside the
process that lost it.

To be told instead of polled, set `OnDrop`:

```go
monitor.Init(monitor.Config{
    Service:   "my-service",
    IngestURL: "https://api.monitor.appleby.cloud/v1/events",
    OnDrop: func(total int64) {
        droppedEvents.Set(float64(total)) // bump a metric; that's all
    },
})
```

It runs on the goroutine that hit the drop — for a full buffer, the one calling
`Emit` — so it must not block or do I/O. A panic inside it is recovered and logged; it
cannot take the shipper down. The existing stderr lines are still printed either way.

## License

MIT
