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
| `ids.go`        | UUID v4 generation                           |

## Data Flow

1. **Init** – `Init(Config)` stores config atomically, starts shipper if `IngestURL` set
2. **Middleware** – Extracts/generates `request_id` and `trace_id`, stores in context
3. **Emit** – Creates `Event` from context + config, writes to stdout and/or shipper
4. **Shipper** – Buffers events, flushes on interval or batch size, POSTs as NDJSON

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

- **Buffer**: In-memory slice, capacity = `BatchSize`
- **Flush triggers**: Timer (`FlushEvery`) or buffer full
- **Transport**: HTTP POST with optional gzip, `X-Api-Key` header
- **Failure handling**: Logs to stderr, does not retry

## Thread Safety

- `globalConfig` and `globalShipper` use `atomic.Pointer`
- Shipper uses channels for event queue, mutex for batch buffer
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
monitor.Init(monitor.Config{Service: "api", IngestURL: "..."})
r.Use(monitor.Middleware)
// IDs auto-propagate through r.Context()
```

**Manual ID injection:**

```go
ctx = monitor.WithUserID(ctx, "user-123")
ctx = monitor.WithTraceID(ctx, parentTraceID)
```
