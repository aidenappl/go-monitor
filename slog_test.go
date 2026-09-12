package monitor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestSlogHandlerTeesAndEmits(t *testing.T) {
	rec := recording(t)
	var out bytes.Buffer
	log := slog.New(NewSlogHandler(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}), nil))
	ctx := WithRequestID(context.Background(), "0123456789abcdef")

	log.InfoContext(ctx, "stack deployed", "event", "stack.deploy.success", "stack_id", 7)
	log.DebugContext(ctx, "noisy detail")

	if !strings.Contains(out.String(), "stack deployed") || !strings.Contains(out.String(), "noisy detail") {
		t.Errorf("wrapped handler should see every record, got %q", out.String())
	}
	evs := rec.Named("stack.deploy.success")
	if len(evs) != 1 {
		t.Fatalf("recorded %d stack.deploy.success events, want 1", len(evs))
	}
	e := evs[0]
	d := e.Data.(map[string]any)
	if d["message"] != "stack deployed" || d["stack_id"] != int64(7) {
		t.Errorf("data = %v", d)
	}
	if _, ok := d["event"]; ok {
		t.Error("the event attribute names the event; it should not also be data")
	}
	if e.RequestID != "0123456789abcdef" || e.Level != LevelInfo {
		t.Errorf("request_id=%q level=%q", e.RequestID, e.Level)
	}
	if d["source_file"] != "slog_test.go" {
		t.Errorf("source_file = %v, want the logging call site", d["source_file"])
	}
	if len(rec.Named("log.debug")) != 0 {
		t.Error("debug is below the default level and must not reach Monitor")
	}
}

func TestSlogHandlerLevelsAndDefaultNames(t *testing.T) {
	rec := recording(t)
	log := slog.New(NewSlogHandler(nil, nil))

	log.Error("db down", "error", errors.New("dial monitor:hunter2@tcp(db:3306)/x"))
	log.Log(context.Background(), slog.LevelError+4, "unrecoverable")

	errs := rec.Named("log.error")
	if len(errs) != 1 || errs[0].Level != LevelError {
		t.Fatalf("log.error events = %v", errs)
	}
	if msg, _ := errs[0].Data.(map[string]any)["error"].(string); strings.Contains(msg, "hunter2") || msg == "" {
		t.Errorf("error attr = %q, want the scrubbed message as a string", msg)
	}
	if f := rec.Named("log.fatal"); len(f) != 1 || f[0].Level != LevelFatal {
		t.Errorf("log.fatal events = %v", f)
	}
}

func TestSlogHandlerGroupsAndAttrs(t *testing.T) {
	rec := recording(t)
	log := slog.New(NewSlogHandler(nil, nil)).With("svc", "lattice-api").WithGroup("db").With("instance", 3)
	log.Warn("slow query", "ms", 1200, slog.Group("q", "table", "stacks"))

	evs := rec.Named("log.warn")
	if len(evs) != 1 {
		t.Fatalf("recorded %d log.warn events", len(evs))
	}
	d := evs[0].Data.(map[string]any)
	for k, want := range map[string]any{"svc": "lattice-api", "db.instance": int64(3), "db.ms": int64(1200), "db.q.table": "stacks"} {
		if d[k] != want {
			t.Errorf("%s = %#v, want %#v (data %v)", k, d[k], want, d)
		}
	}
}

func TestSlogHandlerEnabled(t *testing.T) {
	h := NewSlogHandler(nil, &SlogOptions{Level: slog.LevelWarn})
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("info is below the configured level and there is no wrapped handler")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("error is above the configured level")
	}
}
