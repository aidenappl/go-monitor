package monitor

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// maxLineBytes keeps one serialized event under monitor-core's per-line limit.
//
// Ingest scans NDJSON with a 1 MiB token buffer and rejects the WHOLE request
// when a single line overflows it. An oversized event therefore has to be shrunk
// before it is batched: discovering it afterwards costs a 400, a bisection, and
// the event itself.
const maxLineBytes = 1_000_000

// maxFieldChars bounds each string kept when an oversized event is shrunk.
const maxFieldChars = 4096

// maxNameChars / maxPathChars match the widths of monitor-core's issues table
// (name VARCHAR(255), path VARCHAR(1000)). The event row would still land, but
// under strict mode an over-length value fails the issue upsert server-side and
// the error is never grouped — the worst possible outcome for an error event.
const (
	maxNameChars = 255
	maxPathChars = 1000
)

// groupingKeys are the data keys monitor-core's issue fingerprint reads. When an
// event has to be shrunk these survive (truncated), so it still groups with its
// siblings instead of forming an issue of its own.
var groupingKeys = []string{
	"error", "error_message", "message", "path", "uri", "method", "reason",
	"status_code", "source_file", "source_func", "source_line",
}

// sanitizeEvent makes e safe to batch. Every check here exists because
// monitor-core ingest is all-or-nothing: one line it rejects takes every other
// event in the request with it.
//
// It never drops the event. A malformed field is repaired or cleared, and the
// original is kept in data where it is useful for debugging — losing a bad
// request_id is always better than losing the event that carried it.
func sanitizeEvent(e *Event, r *redactor) {
	if e.Name == "" {
		e.Name = "event.unnamed"
	}
	e.Name = truncateString(e.Name, maxNameChars)
	e.Level = normalizeLevel(e.Level)

	e.JobID = checkCorrelationID(e, "job_id", e.JobID)
	e.RequestID = checkCorrelationID(e, "request_id", e.RequestID)
	e.TraceID = checkCorrelationID(e, "trace_id", e.TraceID)

	if r != nil {
		e.Data = r.redactData(e.Data)
	}

	if m, ok := e.Data.(map[string]any); ok {
		if p, ok := m["path"].(string); ok && len(p) > maxPathChars {
			m = copyMap(m)
			m["path"] = truncateString(p, maxPathChars)
			e.Data = m
		}
	}
}

// normalizeLevel folds the spellings monitor-core would otherwise store verbatim.
//
// The server does not validate level, and issue tracking matches "error" and
// "fatal" exactly — so an event emitted at "ERROR" or "Error" lands, looks
// fine, and is silently never grouped into an issue. "warning" is the other
// common spelling; Trailblaze shipped it twice.
func normalizeLevel(level string) string {
	if level == "" {
		return LevelInfo
	}
	l := strings.ToLower(level)
	if l == "warning" {
		return LevelWarn
	}
	return l
}

// checkCorrelationID returns id if monitor-core would accept it and "" if not,
// keeping the rejected value in data as invalid_<field>.
func checkCorrelationID(e *Event, field, id string) string {
	if ValidCorrelationID(id) {
		return id
	}
	setDataField(e, "invalid_"+field, truncateString(id, 128))
	warnThrottled("invalid-id-"+field,
		"monitor: cleared an invalid %s (monitor-core accepts a UUID or 8-64 hex characters); the value is kept in data.invalid_%s\n",
		field, field)
	return ""
}

// setDataField sets key on e.Data without mutating a map the caller still owns.
func setDataField(e *Event, key string, value any) {
	switch d := e.Data.(type) {
	case nil:
		e.Data = map[string]any{key: value}
	case map[string]any:
		m := copyMap(d)
		m[key] = value
		e.Data = m
	default:
		e.Data = map[string]any{"_data": d, key: value}
	}
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Back off to a rune boundary so the result stays valid UTF-8.
	for n > 0 && n < len(s) && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

// marshalLine serializes e as one NDJSON line, shrinking it if it would exceed
// maxLineBytes. It is the only place an event becomes bytes on its way to
// ingest, so it is the one place the per-line limit can be enforced.
func marshalLine(e Event) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	if len(b) <= maxLineBytes {
		return b, nil
	}
	shrunk, err := json.Marshal(shrinkEvent(e, len(b)))
	if err != nil {
		return nil, err
	}
	if len(shrunk) > maxLineBytes {
		return nil, fmt.Errorf("event %q is %d bytes even after shrinking", e.Name, len(shrunk))
	}
	return shrunk, nil
}

// shrinkEvent replaces an oversized event's data with its grouping fields,
// truncated, plus markers saying what happened.
func shrinkEvent(e Event, size int) Event {
	kept := map[string]any{
		"truncated":           true,
		"original_size_bytes": size,
	}
	if m, ok := e.Data.(map[string]any); ok {
		for _, k := range groupingKeys {
			switch v := m[k].(type) {
			case string:
				kept[k] = truncateString(v, maxFieldChars)
			case int, int64, float64, bool:
				kept[k] = v
			}
		}
	}
	e.Data = kept
	return e
}

// warnThrottled writes a diagnostic to stderr at most once a minute per key.
//
// The SDK's own complaints are the one thing guaranteed to be emitted exactly
// when the system is unhealthy — a retry loop that logs each attempt grows the
// log at the retry rate, not the work rate. Throttling keeps a Monitor outage
// from filling the disk of the host reporting it.
func warnThrottled(key, format string, args ...any) {
	now := time.Now()
	throttleMu.Lock()
	last, seen := throttleLast[key]
	if seen && now.Sub(last) < throttleEvery {
		throttleMu.Unlock()
		return
	}
	throttleLast[key] = now
	throttleMu.Unlock()
	fmt.Fprintf(stderrWriter, format, args...)
}

var (
	throttleMu    sync.Mutex
	throttleLast  = map[string]time.Time{}
	throttleEvery = time.Minute
)
