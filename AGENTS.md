# AGENTS.md — go-monitor

> The comprehensive working document for this repo. An agent that reads only this
> file should be able to work in go-monitor correctly. Keep it current — see
> **Keeping this file updated** at the bottom.

---

## 1. What this repo is

`go-monitor` is the **Go SDK** for the Monitor platform. Go services import it to emit
structured events and ship them as NDJSON to `monitor-core`'s ingestion endpoint. It is
the Go counterpart to `monitor-js`.

Module: `github.com/aidenappl/go-monitor`. It is a **library** (imported, not run) —
the `examples/` dir contains runnable samples; the Devfile's `build`/`run` targets are
generic scaffolding and not the primary use.

It **owns**: the client-side event model, level helpers, context propagation, an
async batching shipper, HTTP middleware, and an outbound-HTTP transport wrapper. It
**does not** own ingestion, storage, or querying (that's `monitor-core`).

---

## 2. Stack & dependencies

- Go 1.25 (`go.mod` — `go 1.25.5`). Standard library only for the core (no heavy deps).
- `github.com/google/uuid` for ID generation.

---

## 3. Project structure

```
go-monitor/
  monitor.go       # Init/Emit/Flush/Shutdown; global config + shipper; dispatchEvent; source capture
  event.go         # Event struct + ToJSON (NDJSON line serialization)
  shipper.go       # Async batching shipper: buffered channel → batch → gzip → POST with retries
  levels.go        # Debug/Info/Warn/Error/Fatal helpers + WithLevel option
  context.go       # WithRequestID/WithTraceID/WithUserID/WithJobID + getters
  middleware.go    # net/http Middleware (auto-emit http.request) + WrapHTTPClient/WrapTransport
  client.go        # RoundTripper that injects X-Trace-Id/X-Request-Id on outbound calls
  timer.go         # StartTimer / Timer.End for duration events
  source.go        # source_file/source_line/source_func capture
  ids.go           # UUID generation
  errors.go        # CaptureError helper
  resolve_options.go / doc.go
  *_test.go        # unit tests (note: most set DisableStdout:true — see §9)
  examples/mux-basic/
  README.md IMPLEMENTATION.md Devfile.yaml
```

---

## 4. Running, building & testing

Uses the `dev` CLI (`Devfile.yaml`). Prerequisite: Go 1.25.

```bash
dev build     # go build -o bin/app .
dev test      # go test ./...
dev check     # gofmt -w -s . && go vet ./... && go test ./...
dev tidy      # go mod tidy
go test -race ./...   # run before shipping concurrency changes
```

The suite is green and now covers the stdout branch (`dispatch_test.go`), the
panic-recovery middleware, and stop-idempotency. Run `go test -race ./...` after
any change to `shipper.go`/`monitor.go`/`middleware.go`.

---

## 5. How code is written here — the pipeline

```
Emit(ctx, name, data, ...opts)
  → newEvent  (fills timestamp/service/env/IDs from ctx + config)
  → attachSourceLocation (if CaptureSource; wraps non-map data under _data)
  → dispatchEvent:
       ├─ stdout branch  (if !DisableStdout) → write NDJSON line to stdoutWriter (os.Stdout)
       └─ shipper.send(event) → buffered channel (cap BatchSize*2)
shipper.run() [accumulator goroutine]:
  drains eventsCh into a batch slice; hands a ready batch to the flush worker
  (non-blocking) on batch-full / FlushEvery ticker / explicit flush / stop-drain.
  Never does network I/O, so retries can't stall event intake.
shipper.shipLoop() [single flush-worker goroutine]:
  consumes batches in order (ordering preserved) → shipBatch:
  build NDJSON → optional gzip → context-bound POST (up to 3 retries; retry
  backoff and in-flight requests cancelled on Shutdown, bounded by a timeout)
```

Public API: `Init(Config)`, `Emit`, `WithLevel`, level helpers (`Debug`/`Info`/`Warn`/
`Error`/`Fatal`), `CaptureError`, `StartTimer`/`Timer.End`, context helpers, `Middleware`/
`MiddlewareWithConfig`, `WrapHTTPClient`/`WrapTransport`, `Flush`, `Shutdown`,
`ResolveEmitOptions`.

### Config (monitor.go)

| Field | Default | Notes |
|---|---|---|
| `Service` | — | **Required.** |
| `Env` | `` | Optional. |
| `JobID` | auto | Process-level job ID. |
| `IngestURL` | `` | Full ingest endpoint URL. **If empty, the shipper is disabled** and events go only to stdout (unless `DisableStdout`). |
| `APIKey` | `` | Sent as `X-Api-Key` (not `Authorization: Bearer`). |
| `BatchSize` | 200 | Channel cap is `BatchSize*2`. |
| `FlushEvery` | 1s | |
| `GzipEnabled` | false | Adds `Content-Encoding: gzip`. |
| `DisableStdout` | false | |
| `Debug` | false | |
| `CaptureSource` | true (`*bool`) | Adds `source_file`/`source_line`/`source_func`. |

---

## 6. Wire-format / ingestion contract

**This is the contract `monitor-core` must accept — diff against it, don't infer from
the SDK struct.**

- **Request:** `POST <IngestURL>` (SDK appends no path — pass the full endpoint).
- **Headers:**
  - `Content-Type: application/x-ndjson` (always)
  - `Content-Encoding: gzip` (only when `GzipEnabled`)
  - `X-Api-Key: <APIKey>` (only when set) — matches `monitor-core`'s
    `IngestAuthMiddleware` which reads `X-Api-Key`.
- **Body:** newline-delimited JSON, one event per line, trailing `\n` per line, batched
  up to `BatchSize`, also flushed every `FlushEvery`.
- **Per-event shape** (`event.go`):
  ```json
  {"timestamp":"RFC3339Nano UTC","service":"…","env":"…","job_id":"…",
   "request_id":"…","trace_id":"…","user_id":"…","name":"…","level":"info","data":{…}}
  ```
  `timestamp`/`service`/`name`/`level` always present; the rest `omitempty`. `data` is
  arbitrary, or a map into which source capture injects `source_*` (non-map data is
  wrapped under `_data`).
- **Success/retry:** status `<400` = success; `4xx` = drop (no retry); `5xx`/network =
  retry ×3.

---

## 7. Ecosystem & related repos

| Repo | Relationship |
|---|---|
| `monitor-core` | Ingestion target. §6 is the exact contract; `monitor-core`'s `POST /v1/events` + `IngestAuthMiddleware` are the other side. |
| `monitor-js` | The TypeScript SDK — keep the wire format in sync. |
| `monitor-web` | Displays the events this SDK ships. |

---

## 8. Operations

Not deployed — it's a dependency. Consumers set `IngestURL` (typically
`https://monitor.appleby.cloud/v1/events`) and `APIKey` (an ingest-scoped Monitor API
key or the env master key). Call `Init` once at startup and `Shutdown`/`Flush` on exit
to drain the buffer. `Shutdown` bounds its final drain+flush with a 5s timeout,
cancelling any in-flight/retrying request past that.

---

## 9. Rules & guardrails + known issues

**Rules**
- Keep the wire format (§6) in lockstep with `monitor-core` and `monitor-js`.
- Never block the caller in `Emit` — the shipper is async by design.
- Run `go test -race ./...` for any change to `shipper.go`/`monitor.go`.

**Known issues & gaps**

The 2026-07-23 review findings (B1–B8 plus the `generateShortID`/`Fatal` and
doc-drift items) were all fixed on 2026-07-23. Summary of the resolutions:

| ID | Where | Resolution |
|---|---|---|
| B1 | `monitor.go` `dispatchEvent` | stdout branch now writes the NDJSON line (`ToJSON()` + `\n`) to `stdoutWriter` (defaults to `os.Stdout`; injectable for tests). Covered by `dispatch_test.go`. |
| B2 | `shipper.go` | Retry/HTTP moved off the accumulator goroutine. `run()` only drains `eventsCh` and hands batches to a dedicated single flush-worker (`shipLoop`) via `batchCh`; intake no longer stalls during retries. Ordering preserved by the single worker. |
| B3 | `middleware.go` | Handler call wrapped in a deferred func that recovers, emits the `http.request` event (level error, with `panic` + `stack` + status/duration) on every path, and re-panics to preserve upstream behavior. |
| B4 | `shipper.go` | HTTP uses `NewRequestWithContext` bound to a shipper context; retry backoff selects on `ctx.Done()`; `stop()` bounds the final drain+flush with `shutdownFlushTimeout` (5s), then cancels. |
| B5 | `shipper.go` `stop()` | Guarded with `sync.Once` — safe to call repeatedly. Covered by `TestShipperStopIsIdempotent`. |
| B6 | README | Corrected to `X-Api-Key`. |
| B7 | `client.go` `RoundTrip` | Clones the request (`req.Clone(ctx)`) before setting headers. |
| B8 | `shipper.go` drain | Shutdown drain chunks events into `BatchSize`-sized batches. |
| — | `ids.go` / `levels.go` | `generateShortID` now returns an 8 hex-char token (used for `job_id`/`request_id`); `Fatal` synchronously `Flush()`es after emitting (does not call `os.Exit`). |
| — | `IMPLEMENTATION.md` | Corrected: retries 3×, buffer is `BatchSize*2`. |

---

## 10. Verification

```bash
gofmt -w -s .
go build ./...
go vet ./...
go test ./...
go test -race ./...   # for concurrency changes
```

CI: `.github/workflows/ci.yml` + `pr.yml`. If a change alters the wire format (§6) or
the public API (§5), update this file **and** coordinate with `monitor-core` /
`monitor-js` in the same effort.

---

## 11. Keeping this file updated

Any change to the pipeline, the wire contract (§6), the public API, or the Config
shape MUST update this file in the same change. When a §9 finding is fixed, delete its
row and correct README.md/IMPLEMENTATION.md to match (per the docs-ship-with-code rule).
