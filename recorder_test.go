package monitor

import (
	"context"
	"testing"
	"time"
)

// recording starts a Recorder for the duration of t.
func recording(t *testing.T) *Recorder {
	t.Helper()
	r := StartRecording()
	t.Cleanup(r.Stop)
	return r
}

// withoutConfig makes the package look un-Initialized for the duration of t.
func withoutConfig(t *testing.T) {
	t.Helper()
	cfg, ship := globalConfig.Load(), globalShipper.Load()
	globalConfig.Store(nil)
	globalShipper.Store(nil)
	t.Cleanup(func() {
		globalConfig.Store(cfg)
		globalShipper.Store(ship)
	})
}

func TestRecorderWorksWithoutInit(t *testing.T) {
	withoutConfig(t)
	rec := recording(t)
	Emit(context.Background(), "no.init.emit", map[string]any{"a": 1})
	Info(context.Background(), "no.init.info", nil)
	if n := len(rec.Events()); n != 2 {
		t.Fatalf("recorded %d events without Init, want 2", n)
	}
}

func TestRecorderSuppressesShipping(t *testing.T) {
	withStderrBuffer(t)
	f := newFakeIngest(t, "")
	if err := Init(Config{Service: "rec-ship", IngestURL: f.URL(), BatchSize: 10, FlushEvery: time.Hour, DisableStdout: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(Shutdown)

	rec := StartRecording()
	Emit(context.Background(), "recorded.only", nil)
	Flush()
	if f.requests.Load() != 0 {
		t.Errorf("ingest was called %d times while recording", f.requests.Load())
	}
	if len(rec.Named("recorded.only")) != 1 {
		t.Error("recorder missed the event")
	}

	rec.Stop()
	Emit(context.Background(), "shipped", nil)
	Flush()
	if f.requests.Load() == 0 {
		t.Error("shipping should resume once the recorder stops")
	}
}

func TestRecorderStopOnlyRemovesItself(t *testing.T) {
	first := StartRecording()
	second := StartRecording()
	first.Stop()
	if activeRecorder.Load() != second {
		t.Error("stopping a replaced recorder must not remove the active one")
	}
	second.Stop()
	if activeRecorder.Load() != nil {
		t.Error("Stop should uninstall the active recorder")
	}
}

func TestRecorderReset(t *testing.T) {
	rec := recording(t)
	Emit(context.Background(), "gone", nil)
	rec.Reset()
	if len(rec.Events()) != 0 {
		t.Error("Reset should discard recorded events")
	}
}
