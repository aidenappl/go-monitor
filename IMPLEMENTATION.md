# Implementation Guide

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────┐
│                       Application                            │
├─────────────────────────────────────────────────────────────┤
│  Middleware     →    Context      →    Emit()               │
│  (IDs injected)      (IDs stored)      (Event created)      │
├─────────────────────────────────────────────────────────────┤
│                         Event                                │
│  ┌─────────────┐    ┌─────────────┐                         │
│  │   stdout    │    │   shipper   │ → HTTP POST (gzip)      │
│  │  (NDJSON)   │    │  (batched)  │                         │
│  └─────────────┘    └─────────────┘                         │
└─────────────────────────────────────────────────────────────┘
```

## Core Components

| File            | Purpose                                      |
| --------------- | -------------------------------------------- |
| `monitor.go`    | Initialization, config, `Emit()` entry point |
| `event.go`      | Event struct and JSON serialization          |
| `context.go`    | Context key storage for IDs                  |
| `middleware.go` | HTTP middleware for ID injection             |
| `shipper.go`    | Async batching and HTTP shipping             |
| `zone.go`       | Startup zone assertion against `/health`     |
| `stats.go`      | `Stats()` / `ShipperStats` loss counters     |
| `ids.go`        | UUID v4 generation                           |

## Data Flow

1. **Init** – `Init(Config)` stores config atomically, starts shipper if `IngestURL` set,
   then starts the zone check (own goroutine) if `Zone` is set
2. **Middleware** – Extracts/generates `request_id` and `trace_id`, stores in context
3. **Emit** – Creates `Event` from context + config, writes to stdout and/or shipper
4. **Shipper** – Buffers events, flushes on interval or batch size, POSTs as NDJSON

## Tenancy

The event carries **no `project` and no `zone` field**, by design. `monitor-core` stamps
`project` from the `api_keys` row behind the credential and overwrites whatever a client
sends — that overwrite is the tenancy boundary, and a wire field would undermine it.

A process therefore selects a zone with two config values, and `globalShipper` is a
single `atomic.Pointer`, so a process ships to exactly one of them:

| Config      | Role                                                              |
| ----------- | ----------------------------------------------------------------- |
| `IngestURL` | That zone's registered ingest endpoint                            |
| `APIKey`    | A key **minted on that zone** — this is what decides the project  |
| `Zone`      | The expected zone slug; asserted at `Init`, never transmitted     |

## Zone Verification

- **Trigger**: `Init`, when `Zone` and `IngestURL` are set and `DisableZoneVerify` is false
- **Probe**: one `GET {scheme}://{host}/health` built from the ingest URL's **origin**
  (`IngestURL` is the full `.../v1/events` endpoint, so the path is stripped, not appended)
- **Bounds**: own goroutine, 3s client timeout, 64 KiB body cap — `Init` never waits and
  never fails on it
- **Outcomes**: `matched` (silent) · `mismatched` (loud banner) · `unverified`, i.e. 200
  with no `zone` key (quiet) · `unreachable` (quiet)

## Event Schema

```json
{
  "timestamp": "2026-02-06T12:00:00.000Z",
  "service": "my-service",
  "env": "prod",
  "job_id": "abc123",
  "request_id": "def456",
  "trace_id": "ghi789",
  "user_id": "user-1",
  "name": "event.name",
  "level": "info",
  "data": {}
}
```

## ID Hierarchy

| ID           | Scope             | Source                             |
| ------------ | ----------------- | ---------------------------------- |
| `job_id`     | Process lifetime  | Config or auto-generated           |
| `trace_id`   | Distributed trace | `X-Trace-Id` header or generated   |
| `request_id` | Single request    | `X-Request-Id` header or generated |
| `user_id`    | User context      | Set via `WithUserID(ctx, id)`      |

## Shipper Behavior

- **Buffer**: Buffered channel, capacity = `BatchSize * 2`, drained by the
  accumulator goroutine into an in-memory batch slice
- **Flush triggers**: Timer (`FlushEvery`), batch full, or explicit `Flush()`
- **Concurrency**: An accumulator goroutine drains the channel and never blocks
  on network I/O; a single dedicated flush-worker goroutine performs the ordered
  HTTP POSTs, so retries don't stall event intake
- **Transport**: HTTP POST with optional gzip, `X-Api-Key` header
- **Failure handling**: Logs to stderr; retries up to 3× on 5xx/network errors
  with exponential backoff (1s/2s/4s); drops on 4xx. Retries and in-flight
  requests are bound to a context that is cancelled on `Shutdown` (bounded by a
  short shutdown timeout)
- **Loss accounting**: every path that abandons events (full buffer, marshal
  failure, gzip failure, retries exhausted, 4xx, cancelled shutdown) calls
  `recordDrop`, which bumps the atomic counter and invokes `Config.OnDrop` with the
  running total — nil-guarded and `recover()`ed, so a broken callback can't take
  down the shipper. `Stats()` exposes `Enqueued`/`Dropped`/`Flushed`

## Thread Safety

- `globalConfig` and `globalShipper` use `atomic.Pointer`
- Shipper uses channels for the event queue and the batch handoff; the batch slice is
  owned by the accumulator goroutine alone, and `stop()` is guarded by a `sync.Once`
- Shipper counters are `atomic.Int64` — `send()` runs on every caller's goroutine while
  `shipLoop()` runs on its own
- Context operations are inherently safe

## Usage Patterns

**Standalone script:**

```go
monitor.Init(monitor.Config{Service: "script"})
monitor.Emit(ctx, "job.done", nil)
monitor.Shutdown()
```

**HTTP service:**

```go
monitor.Init(monitor.Config{
    Service:   "api",
    Zone:      "trailblaze",
    IngestURL: "https://api.monitor.appleby.cloud/v1/events",
    APIKey:    os.Getenv("MONITOR_API_KEY"), // minted on the trailblaze zone
})
r.Use(monitor.Middleware)
// IDs auto-propagate through r.Context()
```

Note `https://monitor.appleby.cloud` is the dashboard's web origin, not an ingest
endpoint. The `appleby` zone ingests at `https://appleby-monitor-api.appleby.cloud/v1/events`.

**Manual ID injection:**

```go
ctx = monitor.WithUserID(ctx, "user-123")
ctx = monitor.WithTraceID(ctx, parentTraceID)
```
