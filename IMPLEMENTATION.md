# Implementation Guide

## Architecture Overview

```
┌──────────────────────────────────────────────────────────────────────┐
│ Application                                                           │
│   Middleware → Context → Emit()/slog/CaptureErrorAs/Go (panics)       │
├──────────────────────────────────────────────────────────────────────┤
│ dispatchEvent                                                         │
│   sanitize (ids, level, sizes) → redact → Recorder | stdout + shipper │
├──────────────────────────────────────────────────────────────────────┤
│ shipper                                                               │
│   run(): eventsCh → batches (never does I/O)                          │
│   memory mode:  shipLoop → POST (retry 3×) ─────────────┐             │
│   spool mode:   writeLoop → disk segments → drainLoop → POST (forever)│
│   rejected request → bisect → quarantine the bad line   │             │
└─────────────────────────────────────────────────────────┴─ Monitor ──┘
```

## Core Components

| File              | Purpose |
| ----------------- | ------- |
| `monitor.go`      | `Init`, `Config`, `Emit`, `dispatchEvent`, global state |
| `event.go`        | `Event` struct and JSON serialization |
| `validate.go`     | Event sanitization, oversize shrinking, throttled diagnostics |
| `redact.go`       | Credential scrubbing by key and by value shape |
| `shipper.go`      | Batching, status classification, retry policies, bisection |
| `spool.go`        | Durable on-disk queue |
| `platform_*.go`   | Advisory lock and free-disk measurement (build-tagged) |
| `zone.go`         | Startup zone assertion against `/health` |
| `stats.go`        | `Stats()` / `ShipperStats` |
| `recorder.go`     | In-memory event capture for tests |
| `safego.go`       | Goroutine panic reporting |
| `slog.go`         | `log/slog` handler that tees into Monitor |
| `middleware.go`   | HTTP id propagation and `http.request` events |
| `client.go`       | Outbound id propagation and `http.client_request` events |
| `context.go`      | Context key storage for ids |
| `ids.go`          | Id generation and the monitor-core id rule |

## Data Flow

1. **Init** – stores config and redactor atomically; replaces an invalid `JobID`; opens the
   spool (falling back to memory if it cannot); starts the shipper; starts the zone check.
   Never touches the network synchronously, never fails on it.
2. **Middleware** – takes valid inbound `X-Request-Id`/`X-Trace-Id` or mints new ones.
3. **Emit** – builds the event from context + config, attaches the source location.
4. **dispatchEvent** – sanitizes and redacts, then records it (test) or prints and ships it.
5. **Shipper** – batches; memory mode POSTs directly, spool mode writes to disk and a single
   drain POSTs oldest-first.

## Tenancy

The event carries **no `project` and no `zone` field**, by design. `monitor-core` stamps
`project` from the `api_keys` row behind the credential and overwrites whatever a client
sends — that overwrite is the tenancy boundary, and a wire field would undermine it.

| Config      | Role |
| ----------- | ---- |
| `IngestURL` | That zone's registered ingest endpoint |
| `APIKey`    | A key **minted on that zone** — this is what decides the project |
| `Zone`      | The expected zone slug; asserted at `Init`, never transmitted |

## Zone Verification

- **Trigger**: `Init`, when `Zone` and `IngestURL` are set and `DisableZoneVerify` is false
- **Probe**: one `GET {scheme}://{host}/health` built from the ingest URL's **origin**
- **Bounds**: own goroutine, 3s client timeout, 64 KiB body cap
- **Outcomes**: `matched` (silent) · `mismatched` (loud banner) · `unverified` (quiet) ·
  `unreachable` (quiet)

## Event Schema

```json
{
  "timestamp": "2026-09-12T12:00:00.000Z",
  "service": "my-service",
  "env": "prod",
  "job_id": "3f9a1c07b2e84d51",
  "request_id": "a07c5e19d3b2f648",
  "trace_id": "01234567-89ab-4cde-8f01-23456789abcd",
  "user_id": "12345",
  "name": "event.name",
  "level": "info",
  "data": {}
}
```

## ID Hierarchy

| ID           | Scope             | Source |
| ------------ | ----------------- | ------ |
| `job_id`     | Process, or a job | Config (validated) / auto; `WithJobID(ctx, NewJobID())` per job |
| `trace_id`   | Distributed trace | Valid `X-Trace-Id` header, or `NewTraceID()` |
| `request_id` | Single request    | Valid `X-Request-Id` header, or `NewRequestID()` |
| `user_id`    | User context      | `WithUserID(ctx, id)` — free text, not validated by the server |

Every id must match monitor-core's `^(UUID|[0-9a-fA-F]{8,64})$` or it is cleared at emit.

## Shipper Behavior

- **Buffer**: channel of `BatchSize*2`; full → counted drop. `Emit` never blocks.
- **Flush triggers**: batch full, `FlushEvery`, `Flush()`, shutdown. A `Flush` first drains
  what is already buffered, so an event emitted before it always ships with it.
- **Request limits**: ≤ 8 MiB uncompressed per request, ≤ 1,000,000 bytes per line
  (oversized events are shrunk to their grouping fields).
- **Classification**: `<400` delivered · content `4xx` rejected · `401/403/404/405`
  misconfigured · `408/429/5xx`/network retryable. `Retry-After` honoured.
- **Bisection**: a rejected request is split and resent until the bad line stands alone,
  within `4·⌈log₂ n⌉+4` extra requests; the isolated line is quarantined.
- **Memory policy**: 3 retries (1s/2s/4s); misconfigured and exhausted batches are dropped.
- **Spool policy**: retries forever with full-jitter backoff up to `MaxBackoff`;
  misconfigured batches are held.
- **Loss accounting**: every abandoning path calls `recordDrop` → counter + `OnDrop`.

## Spool

- **Layout** (`<SpoolDir>/<Service>/`): `LOCK`, `seg-<seq>.ndjson` segments, `cursor`,
  `poison.ndjson`.
- **Write path** (`writeLoop`, one goroutine): lines placed one at a time; segments sealed at
  4 MiB (≤ a quarter of the cap) or when the drain asks; fsync every `SpoolSyncEvery`.
- **Limits**: bytes, 256 files, free-disk floor. Eviction removes the oldest sealed
  segment, never the one being drained.
- **Drain** (`drainLoop`, one goroutine): startup jitter, rate-limited to `DrainRate`,
  oldest segment first, cursor persisted with write-then-rename after every request.
- **Recovery**: torn final line trimmed; resume at the cursor. At-least-once — a crash
  re-sends at most one batch.

## Thread Safety

- `globalConfig`, `globalShipper`, `globalRedactor`, and the active `Recorder` are
  `atomic.Pointer`s.
- The batch slice belongs to `run()` alone; the active segment file belongs to
  `writeLoop()` alone. Segment bookkeeping shared with the drain is guarded by `spool.mu`;
  pending counts are atomic.
- Counters are `atomic.Int64`. `stop()` is guarded by a `sync.Once`.

## Usage Patterns

**HTTP service that must not lose events:**

```go
monitor.Init(monitor.Config{
    Service:       "api",
    Zone:          "appleby",
    IngestURL:     "https://appleby-monitor-api.appleby.cloud/v1/events",
    APIKey:        os.Getenv("MONITOR_API_KEY"), // minted on the appleby zone
    SpoolDir:      "/var/lib/api/monitor-spool",
    DisableStdout: true,
})
defer monitor.Shutdown()
```

**Daemon job correlation:**

```go
ctx := monitor.WithJobID(ctx, monitor.NewJobID())
monitor.Go(ctx, "deploy-worker", runDeploy)
```

Note `https://monitor.appleby.cloud` is the dashboard's web origin, not an ingest endpoint.
