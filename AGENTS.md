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

- Go 1.25 (`go.mod` — `go 1.25.5`).
- **Zero dependencies** — standard library only; `go.mod` has no `require`. IDs come from
  `crypto/rand`. The spool's advisory lock and free-disk check use `syscall` behind build
  tags: `platform_unix.go` (linux, darwin) and `platform_other.go` (everything else).
  Keep it that way — every consumer, including services that must boot before anything
  else on their host, inherits whatever this module imports.

---

## 3. Project structure

```
go-monitor/
  monitor.go         # Init/Emit/Flush/Shutdown; Config; global config, shipper and redactor;
                     #   dispatchEvent (sanitize → recorder | stdout + shipper); source capture;
                     #   stdoutWriter / stderrWriter (the injectable output sinks)
  event.go           # Event struct + ToJSON
  validate.go        # sanitizeEvent (invalid ids cleared → data.invalid_*, level folded,
                     #   name/path bounded); marshalLine (shrinks events over the 1 MB line limit);
                     #   warnThrottled (stderr diagnostics at most once a minute per kind)
  redact.go          # redactor: credential keys and credential-shaped values → [REDACTED]
  shipper.go         # batching + delivery. Memory mode: run → shipLoop. Spool mode: run →
                     #   writeLoop → disk → drainLoop. Status classification, retry policies,
                     #   bisection of rejected requests, quarantine, loss counters
  spool.go           # durable on-disk queue: segments, cursor, eviction, free-disk floor, recovery
  platform_unix.go   # flock + statfs (linux, darwin)
  platform_other.go  # no lock, unmeasurable disk (other platforms)
  zone.go            # Init-time zone assertion: GET {ingest origin}/health vs Config.Zone
  stats.go           # ShipperStats + Stats()
  recorder.go        # StartRecording / Recorder — in-memory capture for tests
  safego.go          # Go / RecoverAndReport / ReportPanic — goroutine panics → panic.recovered
  slog.go            # NewSlogHandler — tee a log/slog handler into Monitor
  levels.go          # Debug/Info/Warn/Error/Fatal helpers + level constants
  context.go         # WithRequestID/WithTraceID/WithUserID/WithJobID + getters
  middleware.go      # Middleware (ids only — emits NOTHING) + MiddlewareWithConfig (http.request events)
  client.go          # WrapHTTPClient/WrapTransport — outbound ids + http.client_request events
  timer.go           # StartTimer / Timer.End for duration events
  ids.go             # NewRequestID / NewJobID / NewTraceID + ValidCorrelationID
  errors.go          # CaptureError / CaptureErrorAs
  doc.go             # package doc
  *_test.go          # bisect_test.go holds fakeIngest, an all-or-nothing stand-in for monitor-core
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
go test -race ./...                              # before shipping concurrency changes
GOOS=linux go vet ./... && GOOS=windows go vet ./...   # both sides of the build tags
```

What the suite pins, by file: the stdout branch (`dispatch_test.go`); stop-idempotency and
retry schedule (`shipper_test.go`); bisection, its request budget and status
classification (`bisect_test.go`); the spool end to end — outage hold, restart recovery,
401 hold, quarantine, eviction, free-disk floor, one-process lock, torn-write trim,
cursor resume, spool-mode `Flush` (`spool_test.go`); `Flush` ordering (`flush_test.go`);
sanitization and the 1 MB shrink (`validate_test.go`); redaction rules and that no
destination sees a secret (`redact_test.go`); the SDK's id rule pinned to monitor-core's
(`ids_contract_test.go`); the zone assertion in all four outcomes (`zone_test.go`); loss
accounting including nil and panicking `OnDrop` (`stats_test.go`); middleware query
handling, route templates, levels, panics and inbound ids
(`middleware_monitor_test.go`); `SafeGo`, `slog`, and the recorder.

Test mechanics worth knowing:
- Tests swap `stdoutWriter`/`stderrWriter` to capture output. The background zone check is
  joined through the `zoneVerifyResults` hook rather than a sleep.
- **Package globals persist across tests.** `Init` stays in effect for every later test,
  so a test that `Init`s with an unusual setting (`DisableRedaction`, a spool) must restore
  or `Shutdown` in `t.Cleanup` — the `spoolTest` helper does both and shrinks the spool's
  timings.
- `warnThrottled` remembers keys for a minute; call `resetThrottle(t)` before asserting on
  a throttled warning.

---

## 5. How code is written here — the pipeline

```
Emit(ctx, name, data, ...opts)
  → newEvent            fills timestamp/service/env/ids from ctx + config
  → attachSourceLocation (CaptureSource; non-map data is wrapped under _data)
  → dispatchEvent
       → sanitizeEvent  ids monitor-core would reject → cleared (kept in data.invalid_*);
                        level folded ("ERROR"→"error", "warning"→"warn"); name ≤255,
                        data.path ≤1000 chars; data REDACTED
       ├─ Recorder active? → record it, stop (nothing printed, nothing shipped)
       ├─ stdout branch (unless DisableStdout)
       └─ shipper.send → buffered channel (cap BatchSize*2; full → counted drop)
shipper.run()     accumulates batches (BatchSize / FlushEvery / Flush / stop); no I/O
  memory mode:  shipLoop → shipBatch → deliver (memory policy) → whatever fails is dropped
  spool mode:   writeLoop → spool.append (disk)      drainLoop → oldest segment → deliver
                                                      (spool policy) → advance cursor → delete
deliver           POST ≤8 MB chunks; a CONTENT rejection is bisected until the bad line is
                  alone, then quarantined; everything else follows the retry policy
```

Public API: `Init(Config)`, `Emit`, `WithLevel`, `ResolveEmitOptions`, level helpers
(`Debug`/`Info`/`Warn`/`Error`/`Fatal`), `CaptureError`/`CaptureErrorAs`,
`StartTimer`/`Timer.End`, context helpers, `NewRequestID`/`NewJobID`/`NewTraceID`,
`ValidCorrelationID`, `Middleware`/`MiddlewareWithConfig`, `WrapHTTPClient`/`WrapTransport`,
`Go`/`RecoverAndReport`/`ReportPanic`, `NewSlogHandler`, `StartRecording`/`Recorder`,
`Flush`, `Shutdown`, `Stats`/`ShipperStats`, `RedactedValue`.

### Config (monitor.go)

| Field | Default | Notes |
|---|---|---|
| `Service` | — | **Required.** Also names the spool subdirectory. |
| `Env` | `` | Optional. |
| `JobID` | auto | Process-level job ID. **Replaced (loudly) at `Init` if monitor-core would reject it** — every event carries it. |
| `IngestURL` | `` | Full ingest endpoint of **one zone**. Empty ⇒ no shipper; stdout only. |
| `APIKey` | `` | Sent as `X-Api-Key`. **Must be minted on the zone `IngestURL` points at** — see §6. |
| `Zone` | `` | Expected zone slug. **Never sent.** Verified once at `Init` against `{ingest origin}/health`. |
| `DisableZoneVerify` | false | Opt out of the `Zone` assertion. |
| `BatchSize` | 200 | Channel cap is `BatchSize*2`; also the spool drain's max events per request. |
| `FlushEvery` | 1s | Also the idle drain tick in spool mode. |
| `GzipEnabled` | false | Adds `Content-Encoding: gzip`; a gzip failure sends uncompressed rather than dropping. |
| `DisableStdout` | false | **Set it on hosts whose container logs are unrotated** — see §8. |
| `Debug` | false | Enables `Debug()` events; confirms a matched zone. |
| `CaptureSource` | true (`*bool`) | `source_file`/`source_line`/`source_func`. |
| `OnDrop` | nil | `func(total int64)`, called on every loss. Must not block (runs on `Emit`'s goroutine for a full buffer). |
| `SpoolDir` | `` | Enables the durable spool at `<SpoolDir>/<Service>/`. Empty ⇒ memory only. |
| `SpoolMaxBytes` | 64 MiB | Oldest events evicted (and counted) past this. |
| `SpoolMinFreeBytes` | 512 MiB | Below this free disk, nothing more is spooled (new events counted as dropped). Negative disables. |
| `SpoolSyncEvery` | 500ms | fsync cadence. Never per event. |
| `MaxBackoff` | 5m | Spool-mode retry ceiling. |
| `DrainRate` | 5/s | Spool-mode requests per second. |
| `RedactKeys` | nil | Extra keys always redacted (e.g. `email` on an IdP). |
| `RedactAllowKeys` | nil | Keys exempt from key-based redaction (e.g. Keyring's `secret_key`, a secret's NAME). |
| `DisableRedaction` | false | Tests only. Errors are still stringified. |

### Sanitization (validate.go)

monitor-core ingest is **all-or-nothing** — one line it rejects fails the whole request.
So every event is made safe to batch before any destination sees it, and nothing is ever
dropped for being malformed at emit time: an invalid `job_id`/`request_id`/`trace_id` is
cleared and its value kept as `data.invalid_<field>` (a caller-supplied `X-Request-Id`
from a proxy is the usual source); an empty name becomes `event.unnamed`; level spellings
the server would store verbatim and never group into an issue are folded; `name`/`path`
are bounded to the issue table's column widths. `marshalLine` enforces the 1 MB line
limit by replacing an oversized event's data with its grouping fields (truncated) plus
`truncated`/`original_size_bytes`, so it still groups with its siblings.

### Redaction (redact.go)

Runs in-process, before stdout, the recorder, the spool and the network — the only point
at which a secret can be kept out of all of them. Two layers:
- **By key** (case- and separator-insensitive): values under keys containing `password`,
  `secret`, `token`, `apikey`, `privatekey`, `authorization`, `cookie`, `credential`,
  `sessionid`, `codeverifier`, `signature`, `dsn`, `encryptionkey`, `signingkey`, or equal
  to `code`/`jwt`/`otp`/`pin`/`nonce`, become `[REDACTED]` — **strings and composites
  only**; numbers and bools are never secrets (`"tokens": 1523` survives).
- **By shape**, in every string: JWTs, PEM private keys, `Bearer`/`Basic` credentials,
  Forta `frt_`/`frtr_` tokens, bcrypt hashes, credential `key=value` pairs (query strings:
  `?code=`, `?id_token_hint=`), credential `"key": "value"` pairs in JSON text, URL
  userinfo, and mysql DSN passwords.

Maps and slices are copied only along changed paths — the caller's data is never mutated.
Structs are normalized through JSON first. `error` values become their (scrubbed)
message; `encoding/json` would otherwise render them `{}`.

### Delivery and bisection (shipper.go)

| Status | Class | Memory mode | Spool mode |
|---|---|---|---|
| `<400` | delivered | counted `Flushed` | cursor advances |
| `400`/`413`/`422`/other `4xx` | **rejected** (content) | bisect → quarantine the bad line(s) | same, plus `poison.ndjson` |
| `401`/`403`/`404`/`405` | misconfigured | drop (counted) | **held**, retried with backoff |
| `408`/`429`/`5xx`/network | retryable | 3 retries, 1s/2s/4s, then drop | forever, full jitter, ≤`MaxBackoff` |

`Retry-After` is honoured (≤10s in memory, ≤`MaxBackoff` spooled). Bisection spends at
most `4·⌈log₂ n⌉+4` extra requests; a request ingest rejects wholesale is quarantined
rather than split into single-event requests. The memory schedule is deliberately
unchanged from v0.0.8 so services without a spool see identical timing.

### Spool (spool.go)

Write-through, not overflow-only: every batch lands on disk; one drain ships oldest-first
and deletes a segment only once all of it is delivered or quarantined. Layout of
`<SpoolDir>/<Service>/`: `LOCK` (flock — a second process on the directory falls back to
memory), `seg-<20-digit seq>.ndjson` segments (sealed at 4 MiB or when the drain asks;
capped at a quarter of `SpoolMaxBytes` so an evictable segment always exists), `cursor`
(`<seq> <offset>`, write-then-rename), `poison.ndjson` (+`.1`, ≤4 MiB each).

Bounded three ways — bytes (`SpoolMaxBytes`), files (256), and free disk
(`SpoolMinFreeBytes`) — with eviction of the oldest, never the segment being drained.
Recovery on `Init` trims a torn final line, resumes at the cursor (a crash re-sends at most
one batch — delivery is at-least-once), and starts draining after a random 0–3s so a fleet
restarting together doesn't return as one wave. `Flush` in spool mode guarantees the
events are on disk, then waits up to 5s for delivery. `Shutdown` never loses spooled
events: what isn't delivered in 5s is there for the next process.

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

Every path that abandons events calls `recordDrop`, which bumps the atomic counter and
invokes `Config.OnDrop` with the running total (nil-guarded and `recover()`ed).
`Stats()` returns `ShipperStats{Enqueued, Dropped, Flushed, Quarantined, Spooled,
Pending, PendingBytes}` from the **active** shipper — zero value when none runs;
counters restart on re-`Init`. `Dropped` covers: full buffer, unserializable, quarantined,
retries exhausted / refused credentials (memory mode), spool eviction, free-disk floor,
and an unreadable segment. The SDK's own stderr complaints go through `warnThrottled` —
a retry loop that logs each attempt grows the log at the retry rate, not the work rate.

### Recorder, panics, slog

- `StartRecording()` intercepts **after** sanitization and redaction, so a test sees
  exactly what would ship; it works without `Init` and suppresses stdout and shipping.
- `Go(ctx, name, fn)` / `defer RecoverAndReport(ctx, name)` turn a goroutine panic — which
  nothing outside that goroutine can recover — into a `panic.recovered` event
  (`goroutine`, `error`, `panic_type`, `stacktrace`). `ReportPanic` is for code with its
  own `recover` that decides whether to restart or exit.
- `NewSlogHandler(next, opts)` tees records to `next` and emits those ≥ `Level` (default
  info). The `event` attribute names the event; without one it is `log.<level>`. Groups
  become dotted keys; source comes from the record's PC.

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
- **Response handling:** see the status table in §5 "Delivery and bisection". In short:
  `<400` delivered; a content `4xx` is bisected; `401/403/404/405` is a configuration
  failure; `408/429/5xx`/network are retried.
- **Limits the SDK enforces before sending**, because ingest rejects the whole request
  otherwise: `job_id`/`request_id`/`trace_id` match `^(UUID|[0-9a-fA-F]{8,64})$`
  (`ValidCorrelationID`, pinned to monitor-core's regex by `ids_contract_test.go`); one
  line ≤ 1,000,000 bytes (monitor-core scans with a 1 MiB buffer); one request ≤ 8 MiB
  uncompressed (monitor-core caps the wire body at 10 MB, measured before gunzip).

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

### Running with a spool

Set `SpoolDir` wherever losing telemetry during a Monitor outage matters — and always on
a service Monitor itself depends on, where a Monitor outage and an incident are the same
event. Placement:

- A **host path or a named volume**, never a container's writable layer: an image-update
  redeploy is a `docker rm`, which is exactly when the backlog would be destroyed.
- **One process per directory.** The lock refuses a second one, which falls back to
  memory with a warning; two replicas need two paths (`Service` is already a
  subdirectory, so distinct service names suffice).
- **Mind the host's disk.** The spool refuses to write below `SpoolMinFreeBytes`, but its
  neighbour matters too: with `DisableStdout` false every event is also printed, and on
  a host with unrotated Docker `json-file` logs that output grows without bound. The
  lattice orchestrator VM ran its root disk to 100% on 2026-09-11 that way. Set
  `DisableStdout: true` there, or cap the container's log first.

```go
monitor.Init(monitor.Config{
    Service:           "lattice-api",
    Zone:              "appleby",
    IngestURL:         "https://appleby-monitor-api.appleby.cloud/v1/events",
    APIKey:            os.Getenv("MONITOR_API_KEY"),
    SpoolDir:          "/var/lib/lattice/monitor-spool",
    DisableStdout:     true,
})
```

### Watching for loss

`Stats()` is cheap and lock-free — surface it on the consumer's own health endpoint so
dropped telemetry is visible from outside the process that lost it:

```go
json.NewEncoder(w).Encode(monitor.Stats())
// {"Enqueued":…,"Dropped":…,"Flushed":…,"Quarantined":…,"Spooled":…,"Pending":…,"PendingBytes":…}
```

How to read it: `Pending` rising while `Flushed` stands still is Monitor being down (the
spool is doing its job); `Dropped` rising is real loss — full buffer, eviction, the
free-disk floor, or (without a spool) a refused key; `Quarantined` rising is a defect in
the emitting code — the events are in `poison.ndjson`.

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
- Run `go test -race ./...` for any change to `shipper.go`/`monitor.go`/`zone.go`/`spool.go`.
- **Every destination receives sanitized, redacted events.** `dispatchEvent` is the one
  place that happens; a new destination (or a new emit path that skips `dispatchEvent`)
  must not bypass it. Nothing unredacted is ever written to stdout or the spool.
- **The spool is bounded three ways** (bytes, files, free disk) and evicts rather than
  grows. A telemetry buffer that fills its host's disk causes the outage it exists to
  record (AWS EBS, 2012: a reporting agent that could not reach its collector OOM'd the
  fleet it observed).
- **No per-attempt logging in retry paths** — use `warnThrottled`.
- **A content 4xx is bisected, never retried unchanged and never dropped whole.**

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
| — | `ids.go` / `levels.go` | `generateShortID` returns a 16 hex-char (8-byte) token for `job_id`/`request_id` (see the `SHORT_ID_BYTES` comment); `Fatal` synchronously `Flush()`es after emitting (does not call `os.Exit`). |
| — | `IMPLEMENTATION.md` | Corrected: retries 3×, buffer is `BatchSize*2`. |

The 2026-09-07 multi-tenancy pass added the zone assertion and loss accounting, and
corrected the live doc bugs behind them:

| Where | Resolution |
|---|---|
| `zone.go` (new) | `Config.Zone` + `DisableZoneVerify`: one `GET {ingest origin}/health` at `Init`, loud banner on mismatch, quiet line when unverified/unreachable. A service pointed at the wrong zone previously had **no** client-side signal of any kind. |
| `shipper.go` / `stats.go` (new) | Drops were a single stderr line — silent loss in the one place it hurts most, since the system that would report it is the one that broke. Now counted, exposed via `Stats()`, and pushed to `Config.OnDrop`. |
| §8 (this file), `README.md`, `IMPLEMENTATION.md` | §8 told consumers to ingest at `https://monitor.appleby.cloud/v1/events` — the **web** origin (Next.js app), which ingests nothing. Replaced with the per-zone ingest URLs; the README's placeholder and IMPLEMENTATION's `"..."` were replaced with a real one too, since a placeholder is what got copied. |

The 2026-09-12 durability pass (for the appleby zone, where Lattice hosts the Monitor it
reports to) changed the delivery model:

| Where | Resolution |
|---|---|
| `shipper.go` | A 4xx dropped the whole batch — one malformed event lost up to 200. Now statuses are classified; a content rejection is bisected and only the bad line quarantined; `408`/`429` are retried with `Retry-After`. |
| `spool.go` (new) | The shipper held events in memory only and dropped them ~7s into any outage. Optional write-through disk spool: survives hours of downtime and process restarts, bounded three ways. |
| `validate.go` (new) | Invalid ids (a proxy's `X-Request-Id`, a `Config.JobID` like `"test-job"`) were sent as-is and 400'd every batch that carried them. Now cleared at emit, value preserved; oversized events shrunk. |
| `redact.go` (new) | No redaction anywhere. `MiddlewareWithConfig` shipped `r.URL.RawQuery` raw — OAuth codes and `id_token_hint` JWTs. Now credential keys and shapes are redacted before any destination. |
| `middleware.go` | Query string off by default (`CaptureQuery`); `RouteTemplate` for issue grouping; `path`/`method`/`status_code`/`error` added so 5xx and panics group; 4xx now `warn`; invalid inbound ids replaced. |
| `shipper.go` `run()` | `Flush()` could return before shipping an event emitted just before it (select ordering) — `Fatal`'s emit-then-flush was unreliable. The buffer is drained before a flush marker. |
| `errors.go`, `safego.go`, `slog.go`, `recorder.go`, `ids.go` (new API) | `CaptureErrorAs`; `Go`/`RecoverAndReport`/`ReportPanic`; `NewSlogHandler`; `StartRecording`; `NewRequestID`/`NewJobID`/`NewTraceID`/`ValidCorrelationID`. |
| docs | `google/uuid` dependency, non-existent files, "Middleware auto-emits", and an 8-char short id were all claimed and all false; corrected. |

**Open gaps**

- **Delivery is at-least-once.** A crash between a successful POST and the cursor write
  re-sends that one batch on restart; there is no idempotency key on the wire.
- **Redaction recognizes credentials by key and by shape.** It cannot know that a
  free-text field holds a secret — don't put secrets in messages, and don't enable
  `CaptureRequestBody` on endpoints that accept them.
- On platforms other than linux/darwin the spool has **no lock and no free-disk floor**.

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
GOOS=linux go vet ./... && GOOS=windows go vet ./...   # the build-tagged platform files
```

CI: `.github/workflows/ci.yml` + `pr.yml`. If a change alters the wire format (§6) or
the public API (§5), update this file **and** coordinate with `monitor-core` /
`monitor-js` in the same effort.

---

## 11. Keeping this file updated

Any change to the pipeline, the wire contract (§6), the public API, or the Config
shape MUST update this file in the same change. When a §9 finding is fixed, delete its
row and correct README.md/IMPLEMENTATION.md to match (per the docs-ship-with-code rule).
