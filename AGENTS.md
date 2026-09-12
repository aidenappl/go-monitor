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
  monitor.go       # Init/Emit/Flush/Shutdown; global config + shipper; dispatchEvent; source capture;
                   #   stdoutWriter / stderrWriter (the injectable output sinks)
  event.go         # Event struct + ToJSON (NDJSON line serialization)
  shipper.go       # Async batching shipper: buffered channel → batch → gzip → POST with retries;
                   #   enqueued/dropped/flushed counters + recordDrop (OnDrop notification)
  zone.go          # Init-time zone assertion: GET {ingest origin}/health, compare against Config.Zone
  stats.go         # ShipperStats + Stats() — the public view of the shipper's counters
  levels.go        # Debug/Info/Warn/Error/Fatal helpers + WithLevel option
  context.go       # WithRequestID/WithTraceID/WithUserID/WithJobID + getters
  middleware.go    # net/http Middleware (auto-emit http.request) + WrapHTTPClient/WrapTransport
  client.go        # RoundTripper that injects X-Trace-Id/X-Request-Id on outbound calls
  timer.go         # StartTimer / Timer.End for duration events
  ids.go           # UUID + short ID generation
  errors.go        # CaptureError helper
  doc.go           # package doc
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

The suite is green and covers the stdout branch (`dispatch_test.go`), the
panic-recovery middleware, stop-idempotency, the zone assertion in all four outcomes
(`zone_test.go`), and drop/flush accounting including a nil and a panicking `OnDrop`
(`stats_test.go`). Run `go test -race ./...` after any change to
`shipper.go`/`monitor.go`/`zone.go`/`middleware.go`.

Tests swap the package-level `stdoutWriter`/`stderrWriter` to capture output. The
background zone check is joined through the `zoneVerifyResults` test hook rather than a
sleep — that receive is what orders the goroutine's writes before a later test restores
those writers.

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
`ResolveEmitOptions`, `Stats`/`ShipperStats`.

### Config (monitor.go)

| Field | Default | Notes |
|---|---|---|
| `Service` | — | **Required.** |
| `Env` | `` | Optional. |
| `JobID` | auto | Process-level job ID. |
| `IngestURL` | `` | Full ingest endpoint URL of **one zone**. **If empty, the shipper is disabled** and events go only to stdout (unless `DisableStdout`). |
| `APIKey` | `` | Sent as `X-Api-Key` (not `Authorization: Bearer`). **Must be minted on the zone `IngestURL` points at** — see §6. |
| `Zone` | `` | Expected zone slug (`trailblaze`, `appleby`). **Never sent on the wire.** Verified once at `Init` against `{ingest origin}/health`; empty disables the check. |
| `DisableZoneVerify` | false | Opt out of the `Zone` assertion while keeping `Zone` set for documentation. |
| `BatchSize` | 200 | Channel cap is `BatchSize*2`. |
| `FlushEvery` | 1s | |
| `GzipEnabled` | false | Adds `Content-Encoding: gzip`. |
| `DisableStdout` | false | |
| `Debug` | false | Also logs a one-line confirmation when the zone check matches. |
| `CaptureSource` | true (`*bool`) | Adds `source_file`/`source_line`/`source_func`. |
| `OnDrop` | nil | `func(total int64)` — called with the running drop total whenever the shipper loses events. |

### Zone assertion (zone.go)

`Config.Zone` is a **startup assertion, not a wire field**. When `Zone` and `IngestURL`
are both set and `DisableZoneVerify` is false, `Init` starts one goroutine that `GET`s
`{scheme}://{host}/health` — derived from the ingest URL's **origin**, since `IngestURL`
is the full endpoint (`…/v1/events`) and the SDK appends no path to it. Concatenating
would request `/v1/events/health` and turn the check into a permanent no-op.

Outcomes mirror `monitor-core`'s `ZoneReachability` vocabulary, and the distinction that
earns them is *unverified* vs *mismatched* — a 200 proves *a* monitor-core is listening,
not that it is **this** zone's:

| Outcome | When | Log level |
|---|---|---|
| `matched` | `/health`'s `zone` equals `Config.Zone` | silent (one line under `Debug`) |
| `mismatched` | it names a **different** zone | **loud multi-line banner** |
| `unverified` | 200 but no `zone` key, unparseable body, or non-200 (older build, a proxy, or something else on that origin) | one quiet line |
| `unreachable` | connection error, or `IngestURL` isn't a URL | one quiet line |

Non-negotiables: the check **never** blocks `Init` (own goroutine, 3s client timeout,
64 KiB body cap) and **never** fails it. A wrong zone is a diagnostic; an unreachable
health endpoint must not stop a service booting or shipping.

### Loss accounting (shipper.go, stats.go)

Every path in the shipper that abandons events calls `recordDrop`, which bumps the
atomic counter and invokes `Config.OnDrop` with the running total (nil-guarded, and
`recover()`ed — a broken callback must not kill the goroutine shipping everything
else). `Stats()` returns `ShipperStats{Enqueued, Dropped, Flushed}` from the **active**
shipper; the zero value when no shipper is running, and counters restart on re-`Init`.

`Dropped` covers the full buffer **and** unserializable events **and** batches abandoned
after retries or rejected with a `4xx` — a counter that only tracked the full-buffer case
would report `0` while an expired API key silently discarded every batch. `OnDrop` runs
on the goroutine that hit the drop (for a full buffer, the caller of `Emit`), so it must
not block. The pre-existing stderr lines are kept: they are the only signal a consumer
that sets neither gets.

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

### Tenancy — there is no `project` field, and there must never be one

Monitor is multi-tenant: **zones** are isolated deployments (own API process, own
ClickHouse, own MariaDB) and each contains **projects**. None of that appears on the
wire:

- The event has **no `project` field and no `zone` field**, and must never gain either.
  `monitor-core` stamps `project` from the `api_keys` row behind the credential and
  **deliberately overwrites anything a client sends**. That overwrite is the tenancy
  boundary — it is what makes tenancy unforgeable by a producer. A wire field would
  be, at best, ignored; at worst, a boundary that could be argued about.
- A service therefore targets a zone by **where it points and what it presents**:
  `IngestURL` = that zone's registered ingest URL, `APIKey` = a key **minted on that
  zone**.
- A key minted on the control plane (or on another zone) binds to *that* zone's
  project. Events ship, return `<400`, and appear in a dashboard — the wrong tenant's.
  Nothing errors anywhere, which is exactly why `Config.Zone` exists.

### One process ships to one destination — deliberately

`globalShipper` is a single `atomic.Pointer[shipper]` and `Init` replaces it. There is
no fan-out and none should be built: **a process belongs to one zone.** Re-`Init`
retargets the whole process (stopping the previous shipper and resetting `Stats()`), it
does not add a second destination. If two destinations ever seem necessary, the answer
is two processes — or a zone-side change — not a slice of shippers here.

---

## 7. Ecosystem & related repos

| Repo | Relationship |
|---|---|
| `monitor-core` | Ingestion target. §6 is the exact contract; `monitor-core`'s `POST /v1/events` + `IngestAuthMiddleware` are the other side. |
| `monitor-js` | The TypeScript SDK — keep the wire format in sync. |
| `monitor-web` | Displays the events this SDK ships. |

---

## 8. Operations

Not deployed — it's a dependency. Consumers set `IngestURL` to **their zone's ingest
URL** and `APIKey` to an ingest-scoped key **minted on that zone**. Call `Init` once at
startup and `Shutdown`/`Flush` on exit to drain the buffer. `Shutdown` bounds its final
drain+flush with a 5s timeout, cancelling any in-flight/retrying request past that.

### Targeting a zone

| Zone | `IngestURL` | `Zone` |
|---|---|---|
| `trailblaze` | `https://api.monitor.appleby.cloud/v1/events` | `"trailblaze"` |
| `appleby` | `https://appleby-monitor-api.appleby.cloud/v1/events` | `"appleby"` |

⚠️ `https://monitor.appleby.cloud` is the **web origin** (the Next.js app), not an API.
It ingests nothing. Earlier revisions of this file named it here; anything copied from
them is pointed at the wrong host.

```go
monitor.Init(monitor.Config{
    Service:   "my-service",
    Env:       "prod",
    Zone:      "trailblaze",                                   // asserted at Init, never sent
    IngestURL: "https://api.monitor.appleby.cloud/v1/events",  // that zone's registered ingest URL
    APIKey:    os.Getenv("MONITOR_API_KEY"),                   // MINTED ON THAT ZONE
})
```

The key is the half that decides tenancy (§6): a control-plane key against a zone's
ingest URL files this service's events under a control-plane project, successfully and
silently. `Zone` is what turns that into a loud line in the boot log.

### Watching for loss

`Stats()` is cheap and lock-free — surface it on the consumer's own health endpoint so
dropped telemetry is visible from outside the process that lost it:

```go
json.NewEncoder(w).Encode(monitor.Stats())  // {"Enqueued":…, "Dropped":…, "Flushed":…}
```

A non-zero `Dropped` means one of: sustained emit rate above what the shipper drains
(raise `BatchSize`, or emit less), or ingest rejecting batches — a `4xx` in the stderr
log is usually a wrong/expired API key.

---

## 9. Rules & guardrails + known issues

**Rules**
- Keep the wire format (§6) in lockstep with `monitor-core` and `monitor-js`.
- **Never add `project` or `zone` to the event.** Tenancy is stamped server-side from
  the `api_keys` row and a client cannot be allowed to influence it (§6).
- **Never build fan-out.** One process, one shipper, one zone (§6).
- Never block the caller in `Emit` — the shipper is async by design. That includes
  anything reachable from `Emit`: `OnDrop` is documented as non-blocking for this
  reason, and the zone check runs on its own goroutine for the same one.
- Never let a diagnostic become a gate: an unreachable `/health` must not fail `Init`.
- Every path that abandons events calls `recordDrop`. A new drop site that skips it
  makes `Stats()` lie, which is worse than not having the counter.
- Run `go test -race ./...` for any change to `shipper.go`/`monitor.go`/`zone.go`.

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

The 2026-09-07 multi-tenancy pass added the zone assertion and loss accounting, and
corrected the live doc bugs behind them:

| Where | Resolution |
|---|---|
| `zone.go` (new) | `Config.Zone` + `DisableZoneVerify`: one `GET {ingest origin}/health` at `Init`, loud banner on mismatch, quiet line when unverified/unreachable. A service pointed at the wrong zone previously had **no** client-side signal of any kind. |
| `shipper.go` / `stats.go` (new) | Drops were a single stderr line — silent loss in the one place it hurts most, since the system that would report it is the one that broke. Now counted, exposed via `Stats()`, and pushed to `Config.OnDrop`. |
| §8 (this file), `README.md`, `IMPLEMENTATION.md` | §8 told consumers to ingest at `https://monitor.appleby.cloud/v1/events` — the **web** origin (Next.js app), which ingests nothing. Replaced with the per-zone ingest URLs; the README's placeholder and IMPLEMENTATION's `"..."` were replaced with a real one too, since a placeholder is what got copied. |

**Open gaps**

- The zone check reads only `zone`; it does not consult `/ready`, so a right-but-degraded
  zone still reports `matched`. Deliberate — `monitor-core` calls that `degraded`, and a
  producer cannot act on it anyway.
- A zone deployed behind a **path prefix** (`https://host/monitor/v1/events`) would have
  its health probed at `https://host/health`. No such deployment exists; if one appears,
  `healthURLFromIngestURL` is the single place to change.

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
