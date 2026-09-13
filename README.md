# go-monitor

A lightweight, structured event monitoring library for Go services with guaranteed fields on every event.

## Features

- **Guaranteed fields**: Every event carries `service`, `timestamp`, `name`, `level`, and the ids from its context
- **Context-aware**: IDs flow through request contexts automatically
- **HTTP middleware**: Gorilla mux compatible — id propagation, and optional per-request events
- **NDJSON output**: Events are printed as newline-delimited JSON to stdout
- **Async shipping**: Batch events and POST them to a Monitor zone, with gzip support
- **Durable spool**: Optionally survive Monitor being down for hours — or the process restarting — without losing events
- **Never loses a batch to one bad event**: Malformed events are repaired at emit time, and a rejected request is bisected so only the bad line is dropped
- **Redaction**: Credentials are scrubbed in-process before anything is printed, spooled or sent
- **Zone assertion**: Verifies at startup that you're pointed at the zone you think you are
- **Visible loss**: Dropped events are counted (`Stats()`) and reported (`OnDrop`), not just logged
- **Zero dependencies**: Standard library only

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
    "os"

    monitor "github.com/aidenappl/go-monitor"
    "github.com/gorilla/mux"
)

func main() {
    // Init never fails on the network and never blocks: a service can start
    // before the Monitor it reports to is reachable.
    monitor.Init(monitor.Config{
        Service:   "my-service",
        Env:       "prod",
        Zone:      "appleby",
        IngestURL: "https://appleby-monitor-api.appleby.cloud/v1/events",
        APIKey:    os.Getenv("MONITOR_API_KEY"),
        SpoolDir:  "/var/lib/my-service/monitor-spool", // optional: survive outages
    })
    defer monitor.Shutdown()

    monitor.Emit(context.Background(), "service.startup", map[string]any{"version": "1.0.0"})

    r := mux.NewRouter()
    r.Use(monitor.MiddlewareWithConfig(monitor.MiddlewareConfig{
        RouteTemplate: routeTemplate, // see "HTTP Middleware"
    }))

    r.HandleFunc("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
        monitor.Emit(r.Context(), "user.get.success", map[string]any{"user_id": mux.Vars(r)["id"]})
        w.Write([]byte("OK"))
    })

    http.ListenAndServe(":8080", r)
}
```

## Configuration

```go
type Config struct {
    Service string // Required. Also names the spool subdirectory.
    Env     string // e.g. "prod", "staging".

    // JobID overrides the process-level job id. Must be a UUID or 8-64 hex
    // characters; anything else is replaced at Init with a warning.
    JobID string

    // IngestURL is one zone's full ingest endpoint. Empty: stdout only.
    IngestURL string

    // APIKey must be minted on the zone IngestURL points at — the server
    // derives the project from this key.
    APIKey string

    Zone              string // Expected zone; verified at Init, never sent.
    DisableZoneVerify bool

    BatchSize     int           // Events per batch. Default: 200.
    FlushEvery    time.Duration // Default: 1s.
    GzipEnabled   bool
    DisableStdout bool
    Debug         bool  // Enables Debug() events.
    CaptureSource *bool // source_file/line/func. Default: true.

    // OnDrop is called with the running drop total whenever events are lost.
    // Must not block — see "Dropped events".
    OnDrop func(total int64)

    // Durable spool — see "Surviving Monitor outages".
    SpoolDir          string        // Empty: memory only.
    SpoolMaxBytes     int64         // Default: 64 MiB.
    SpoolMinFreeBytes int64         // Default: 512 MiB; negative disables.
    SpoolSyncEvery    time.Duration // Default: 500ms.
    MaxBackoff        time.Duration // Default: 5m.
    DrainRate         float64       // Requests/second. Default: 5.

    // Redaction — see "Redaction".
    RedactKeys       []string
    RedactAllowKeys  []string
    DisableRedaction bool
}
```

Every field except `Service` is optional and safe at its zero value.

## Event Schema

```json
{
  "timestamp": "2026-09-12T10:30:00.123456789Z",
  "service": "my-service",
  "env": "prod",
  "job_id": "3f9a1c07b2e84d51",
  "request_id": "a07c5e19d3b2f648",
  "trace_id": "01234567-89ab-4cde-8f01-23456789abcd",
  "user_id": "12345",
  "name": "user.create.success",
  "level": "info",
  "data": { "plan": "pro" }
}
```

| Field        | Description |
| ------------ | ----------- |
| `timestamp`  | RFC3339Nano UTC |
| `service`    | From config |
| `env`        | From config (optional) |
| `job_id`     | Process-level id, or a per-job id from `WithJobID` |
| `request_id` | Request-scoped id (16 hex chars when minted here) |
| `trace_id`   | Distributed trace id (a UUID when minted here) |
| `user_id`    | Set with `WithUserID` |
| `name`       | Event name — `{resource}.{action}.{result}` by convention |
| `level`      | `debug` · `info` · `warn` · `error` · `fatal` |
| `data`       | Arbitrary event data |

**Ids must be a UUID or 8-64 hex characters** — monitor-core rejects the whole request
otherwise. Mint them with `NewRequestID()`, `NewJobID()` and `NewTraceID()`, and check an
inbound one with `ValidCorrelationID`. An invalid id is never sent: it is cleared from
the event and kept in `data.invalid_<field>`.

## API Reference

### Initialization

```go
monitor.Init(monitor.Config{Service: "my-service"}) // required before Emit
monitor.Flush()    // ship everything emitted so far
monitor.Shutdown() // flush (bounded to 5s) and stop
monitor.Stats()    // see "Dropped events"
```

### Emitting Events

```go
monitor.Emit(ctx, "stack.deploy.success", map[string]any{"stack_id": 7})
monitor.Emit(ctx, "stack.deploy.failed", data, monitor.WithLevel(monitor.LevelError))

monitor.Info(ctx, "cache.warm.complete", nil)
monitor.Error(ctx, "db.query.failed", map[string]any{"error": err.Error()})

// Name errors by what failed — Monitor groups issues by event name.
monitor.CaptureErrorAs(ctx, "secret.decrypt.failed", err, map[string]any{"secret_id": id})
```

Monitor groups `error` and `fatal` events into issues by service, event name,
`data.path`, and message (`data.error`, then `data.error_message`, then `data.message`).
Put the operation in the name and the route pattern in `data.path`.

#### Inspecting emit options

`ResolveEmitOptions` applies options and reports the settings `Emit` would use — mainly so
a wrapper or test can assert the **level** an event was emitted at:

```go
monitor.ResolveEmitOptions(monitor.WithLevel(monitor.LevelError)).Level // "error"
```

### Context Helpers

```go
ctx = monitor.WithJobID(ctx, monitor.NewJobID())   // e.g. one per deploy in a daemon
ctx = monitor.WithRequestID(ctx, monitor.NewRequestID())
ctx = monitor.WithTraceID(ctx, monitor.NewTraceID())
ctx = monitor.WithUserID(ctx, "12345")

monitor.JobID(ctx); monitor.RequestID(ctx); monitor.TraceID(ctx); monitor.UserID(ctx)
```

### HTTP Middleware

`monitor.Middleware` propagates ids — reading `X-Request-Id`/`X-Trace-Id` (replacing any
that monitor-core would reject), generating missing ones, storing them in the context and
echoing them as response headers. **It emits no events.**

`monitor.MiddlewareWithConfig` does the same and emits one `http.request` event per
request: `method`, `path`, `status_code`, `duration_ms` (plus `request_path`,
`response_status`, `response_content_type`). `4xx` is `warn`, `5xx` is `error`; a panic
is recorded with `error` and `stack`, then re-panicked.

```go
r.Use(monitor.MiddlewareWithConfig(monitor.MiddlewareConfig{
    SkipPaths: []string{"/healthcheck"},
    // path is what issues group by: the template keeps /stacks/{id} one issue.
    RouteTemplate: func(r *http.Request) string {
        if rt := mux.CurrentRoute(r); rt != nil {
            if t, err := rt.GetPathTemplate(); err == nil {
                return t
            }
        }
        return ""
    },
}))
```

**The query string is not captured** unless `CaptureQuery: true` — it is where OAuth codes,
tokens and emails travel. When enabled it is still redacted. Bodies are off by default
too (`CaptureRequestBody` / `CaptureResponseBody`); leave them off on endpoints that accept
secrets.

### Panics in goroutines

A panic in a goroutine cannot be recovered by middleware or `main` — it kills the process
and every buffered event with it.

```go
monitor.Go(ctx, "snapshot-scheduler", func(ctx context.Context) { runScheduler(ctx) })

go func() {
    defer monitor.RecoverAndReport(ctx, "ws-read-pump") // must be deferred directly
    readPump()
}()
```

Both emit `panic.recovered` with `goroutine`, `error`, `panic_type` and `stacktrace`.
`ReportPanic(ctx, name, recovered)` reports a value your own `recover` already caught.

### log/slog

```go
base := slog.NewJSONHandler(os.Stdout, nil)
slog.SetDefault(slog.New(monitor.NewSlogHandler(base, nil)))

slog.ErrorContext(ctx, "snapshot upload failed", "event", "snapshot.upload.failed", "error", err)
```

Records go to `base` unchanged; those at or above `SlogOptions.Level` (default info) are
also emitted to Monitor. The `event` attribute names the event (otherwise `log.<level>`).

## Async Shipping

When `IngestURL` is set, events are batched and POSTed as NDJSON with `X-Api-Key`. What
happens to a failed request depends on why it failed:

| Response | Without a spool | With a spool |
| --- | --- | --- |
| `400`/`413`/`422` — malformed content | Bisected; only the bad event is dropped (`Quarantined`) | Same; the bad event is kept in `poison.ndjson` |
| `401`/`403`/`404` — wrong key or URL | Dropped and counted | **Held** and retried until the config is fixed |
| `408`/`429`/`5xx`/network | 3 retries (1s, 2s, 4s), then dropped | Retried until delivered, backing off up to `MaxBackoff` |

**One process ships to one destination.** `Init` replaces the shipper; calling it again
retargets the process rather than adding a second destination.

## Surviving Monitor outages

Without `SpoolDir`, events live in memory and a Monitor outage longer than the retry
window loses them. With it, every batch is written to `<SpoolDir>/<Service>/` first and
deleted only once ingest accepts it — Monitor can be down for hours, and the process can
restart in the meantime; the backlog ships when it answers.

- Put it on a **host path or named volume**. A container's writable layer is destroyed by
  the redeploy it would need to survive.
- **One process per directory** — a second one falls back to memory with a warning.
- It is **bounded**: `SpoolMaxBytes` (oldest evicted first), 256 files, and a free-disk
  floor (`SpoolMinFreeBytes`) below which it stops writing. Everything discarded is
  counted.
- Writes are fsynced every `SpoolSyncEvery`, never per event. A crashed process loses
  nothing it wrote; a restart may re-send the one batch that was in flight.
- A restarted process waits a random 0–3s before draining, then delivers at `DrainRate`,
  so a fleet coming back together doesn't hit Monitor as one wave.

## Redaction

Every event is scrubbed in-process before it reaches stdout, the spool, or the network:

- **By key**: values under keys like `password`, `secret`, `token`, `api_key`,
  `authorization`, `cookie`, `client_secret`, `dsn`, `code` become `[REDACTED]`.
  Numbers and bools are left alone — `"tokens": 1523` is a count, not a credential.
- **By shape**, anywhere in a string: JWTs, PEM private keys, `Bearer` credentials,
  Forta `frt_` tokens, bcrypt hashes, `?code=`/`?token=`-style parameters, credential
  fields in JSON text, and passwords in URLs and DSNs.

```go
monitor.Init(monitor.Config{
    Service:         "keyring-api",
    RedactKeys:      []string{"email"},      // also redact these
    RedactAllowKeys: []string{"secret_key"}, // a secret's NAME, not its value
})
```

Redaction recognizes credentials by key and by shape; it cannot know a free-text field is
sensitive. Don't put secrets in messages.

## Targeting a zone

Monitor is multi-tenant. **Zones** are isolated deployments (own API process, own
ClickHouse, own MariaDB), each containing **projects**. A service picks its zone by
where it points and what it presents:

| Zone         | `IngestURL`                                           |
| ------------ | ----------------------------------------------------- |
| `trailblaze` | `https://api.monitor.appleby.cloud/v1/events`         |
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

- **Mismatch** → a loud multi-line banner on stderr at startup. Misattributed telemetry
  cannot be moved afterwards, so this is the only moment anyone can catch it.
- **Unreachable, or a `/health` that doesn't name a zone** → one quiet line. The service
  still boots and still ships; a health endpoint being briefly down proves nothing.
- `Init` never fails or blocks on this — the probe runs on its own goroutine with a 3s
  timeout.
- `Zone` is **never** put on the wire. Set `DisableZoneVerify: true` to skip the check.

## Dropped events

Loss is counted, not just logged — the system that would have told you about it is the
one that broke:

```go
monitor.Stats()
// ShipperStats{Enqueued: 1042, Dropped: 3, Flushed: 1039, Quarantined: 1, Spooled: 1042, Pending: 0, PendingBytes: 0}
```

| Field          | Meaning |
| -------------- | ------- |
| `Enqueued`     | Events accepted into the shipper's buffer |
| `Flushed`      | Events ingest accepted |
| `Dropped`      | Events lost for good: full buffer, unserializable, quarantined, retries exhausted or key refused (no spool), evicted from a full spool, or refused below the free-disk floor |
| `Quarantined`  | The part of `Dropped` ingest refused as malformed — a bug in the emitting code |
| `Spooled`      | Events written to the spool |
| `Pending`, `PendingBytes` | Events on disk awaiting delivery — rising while `Flushed` stands still means Monitor is down |

`Stats()` returns the zero value when no shipper is running. Surface it on your own health
endpoint. To be told instead of polled, set `OnDrop` — it runs on the goroutine that hit
the drop (for a full buffer, the one calling `Emit`), so it must only bump a counter. A
panic inside it is recovered.

## Testing

```go
rec := monitor.StartRecording()
defer rec.Stop()

handler.ServeHTTP(w, r)

if got := rec.Named("secret.read.failed"); len(got) != 1 {
    t.Fatalf("want one failure event, got %d", len(got))
}
```

The recorder captures events after sanitization and redaction — exactly what would ship —
and suppresses printing and shipping while active. It works without `Init`.

## License

MIT
