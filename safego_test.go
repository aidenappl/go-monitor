package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGoReportsAPanicInsteadOfCrashing(t *testing.T) {
	rec := recording(t)
	Go(context.Background(), "snapshot-worker", func(context.Context) {
		panic("assignment to entry in nil map")
	})
	waitFor(t, 2*time.Second, "panic.recovered", func() bool { return len(rec.Named("panic.recovered")) == 1 })

	d := rec.Named("panic.recovered")[0].Data.(map[string]any)
	if d["goroutine"] != "snapshot-worker" || d["error"] != "assignment to entry in nil map" {
		t.Errorf("data = %v", d)
	}
	if st, _ := d["stacktrace"].(string); !strings.Contains(st, "safego_test.go") {
		t.Errorf("stacktrace should point at the panicking code, got %q", st)
	}
	if rec.Named("panic.recovered")[0].Level != LevelError {
		t.Error("a recovered panic is an error")
	}
}

func TestRecoverAndReportScrubsThePanicValue(t *testing.T) {
	rec := recording(t)
	func() {
		defer RecoverAndReport(context.Background(), "direct")
		panic(errors.New("connect password=hunter2 refused"))
	}()
	evs := rec.Named("panic.recovered")
	if len(evs) != 1 {
		t.Fatalf("recorded %d panics, want 1", len(evs))
	}
	if msg := evs[0].Data.(map[string]any)["error"].(string); strings.Contains(msg, "hunter2") {
		t.Errorf("panic message leaked a credential: %q", msg)
	}
}

func TestReportPanicToleratesANilContext(t *testing.T) {
	rec := recording(t)
	//nolint:staticcheck // deliberately nil
	ReportPanic(nil, "nil-ctx", "boom")
	if len(rec.Named("panic.recovered")) != 1 {
		t.Error("ReportPanic with a nil context should still report")
	}
}
