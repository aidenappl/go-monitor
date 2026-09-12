package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newFullShipper returns a shipper whose buffer is already full: it is never
// started, so nothing drains eventsCh and every send past the capacity drops.
// Capacity is BatchSize*2 (see newShipper).
func newFullShipper(t *testing.T, cfg *Config) *shipper {
	t.Helper()
	s := newShipper(cfg)
	capacity := cfg.BatchSize * 2
	for i := 0; i < capacity; i++ {
		s.send(Event{Name: "fill", Service: cfg.Service, Level: "info"})
	}
	if got := s.enqueued.Load(); got != int64(capacity) {
		t.Fatalf("enqueued = %d, want %d before any drop", got, capacity)
	}
	if got := s.dropped.Load(); got != 0 {
		t.Fatalf("dropped = %d, want 0 before the buffer is full", got)
	}
	return s
}

func TestShipperDropAccounting(t *testing.T) {
	t.Run("full buffer increments the drop counter", func(t *testing.T) {
		withStderrBuffer(t)

		cfg := &Config{Service: "drop-test", IngestURL: "http://127.0.0.1:0/never", BatchSize: 2, FlushEvery: time.Hour, DisableStdout: true}
		s := newFullShipper(t, cfg)

		for i := 0; i < 3; i++ {
			s.send(Event{Name: "overflow", Service: cfg.Service, Level: "info"})
		}

		if got := s.dropped.Load(); got != 3 {
			t.Errorf("dropped = %d, want 3", got)
		}
		if got := s.enqueued.Load(); got != 4 {
			t.Errorf("enqueued = %d, want 4 (capacity only)", got)
		}
	})

	t.Run("stderr line is kept alongside the counter", func(t *testing.T) {
		buf := withStderrBuffer(t)

		cfg := &Config{Service: "drop-stderr", IngestURL: "http://127.0.0.1:0/never", BatchSize: 1, FlushEvery: time.Hour, DisableStdout: true}
		s := newFullShipper(t, cfg)
		s.send(Event{Name: "overflow", Service: cfg.Service, Level: "info"})

		if !strings.Contains(buf.String(), "shipper buffer full, dropping event") {
			t.Errorf("log %q should still carry the stderr line", buf.String())
		}
	})

	t.Run("OnDrop fires with the running total", func(t *testing.T) {
		withStderrBuffer(t)

		var totals []int64
		cfg := &Config{
			Service:       "drop-callback",
			IngestURL:     "http://127.0.0.1:0/never",
			BatchSize:     1,
			FlushEvery:    time.Hour,
			DisableStdout: true,
			OnDrop:        func(total int64) { totals = append(totals, total) },
		}
		s := newFullShipper(t, cfg)

		for i := 0; i < 3; i++ {
			s.send(Event{Name: "overflow", Service: cfg.Service, Level: "info"})
		}

		want := []int64{1, 2, 3}
		if len(totals) != len(want) {
			t.Fatalf("OnDrop called %d times (%v), want %d", len(totals), totals, len(want))
		}
		for i := range want {
			if totals[i] != want[i] {
				t.Errorf("OnDrop call %d got total %d, want %d", i, totals[i], want[i])
			}
		}
	})

	t.Run("nil OnDrop is safe", func(t *testing.T) {
		withStderrBuffer(t)

		cfg := &Config{Service: "drop-nil-cb", IngestURL: "http://127.0.0.1:0/never", BatchSize: 1, FlushEvery: time.Hour, DisableStdout: true}
		s := newFullShipper(t, cfg)

		s.send(Event{Name: "overflow", Service: cfg.Service, Level: "info"})

		if got := s.dropped.Load(); got != 1 {
			t.Errorf("dropped = %d, want 1", got)
		}
	})

	t.Run("panicking OnDrop does not take the shipper down", func(t *testing.T) {
		buf := withStderrBuffer(t)

		var calls atomic.Int32
		cfg := &Config{
			Service:       "drop-panic-cb",
			IngestURL:     "http://127.0.0.1:0/never",
			BatchSize:     1,
			FlushEvery:    time.Hour,
			DisableStdout: true,
			OnDrop: func(total int64) {
				calls.Add(1)
				panic("callback is broken")
			},
		}
		s := newFullShipper(t, cfg)

		// Two drops: the second only happens if the first panic was contained.
		s.send(Event{Name: "overflow", Service: cfg.Service, Level: "info"})
		s.send(Event{Name: "overflow", Service: cfg.Service, Level: "info"})

		if got := calls.Load(); got != 2 {
			t.Errorf("OnDrop called %d times, want 2", got)
		}
		if got := s.dropped.Load(); got != 2 {
			t.Errorf("dropped = %d, want 2", got)
		}
		if !strings.Contains(buf.String(), "OnDrop callback panicked") {
			t.Errorf("log %q should report the panicking callback", buf.String())
		}
	})

	t.Run("a 4xx counts the batch as dropped", func(t *testing.T) {
		withStderrBuffer(t)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		var total atomic.Int64
		cfg := &Config{
			Service:       "drop-4xx",
			IngestURL:     srv.URL,
			BatchSize:     10,
			FlushEvery:    time.Hour,
			DisableStdout: true,
			OnDrop:        func(n int64) { total.Store(n) },
		}
		s := newShipper(cfg)

		s.shipBatch([]Event{
			{Name: "a", Service: cfg.Service, Level: "info", Timestamp: time.Now().UTC().Format(time.RFC3339Nano)},
			{Name: "b", Service: cfg.Service, Level: "info", Timestamp: time.Now().UTC().Format(time.RFC3339Nano)},
		})

		if got := s.dropped.Load(); got != 2 {
			t.Errorf("dropped = %d, want 2 (an expired key must not read as zero loss)", got)
		}
		if got := s.flushed.Load(); got != 0 {
			t.Errorf("flushed = %d, want 0", got)
		}
		if got := total.Load(); got != 2 {
			t.Errorf("OnDrop total = %d, want 2", got)
		}
	})

	t.Run("a successful ship counts as flushed", func(t *testing.T) {
		withStderrBuffer(t)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		cfg := &Config{Service: "flush-ok", IngestURL: srv.URL, BatchSize: 10, FlushEvery: time.Hour, DisableStdout: true}
		s := newShipper(cfg)

		s.shipBatch([]Event{
			{Name: "a", Service: cfg.Service, Level: "info", Timestamp: time.Now().UTC().Format(time.RFC3339Nano)},
			{Name: "b", Service: cfg.Service, Level: "info", Timestamp: time.Now().UTC().Format(time.RFC3339Nano)},
		})

		if got := s.flushed.Load(); got != 2 {
			t.Errorf("flushed = %d, want 2", got)
		}
		if got := s.dropped.Load(); got != 0 {
			t.Errorf("dropped = %d, want 0", got)
		}
	})
}

func TestStats(t *testing.T) {
	t.Run("zero without a shipper", func(t *testing.T) {
		if err := Init(Config{Service: "stats-no-shipper", DisableStdout: true}); err != nil {
			t.Fatalf("Init() error = %v", err)
		}
		if got := Stats(); got != (ShipperStats{}) {
			t.Errorf("Stats() = %+v, want zero value when no shipper is configured", got)
		}
	})

	t.Run("reports enqueued and flushed through the public API", func(t *testing.T) {
		withStderrBuffer(t)

		var received atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			received.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		if err := Init(Config{
			Service:       "stats-service",
			IngestURL:     srv.URL,
			BatchSize:     10,
			FlushEvery:    time.Hour,
			DisableStdout: true,
		}); err != nil {
			t.Fatalf("Init() error = %v", err)
		}

		ctx := context.Background()
		Emit(ctx, "stats.one", nil)
		Emit(ctx, "stats.two", nil)

		// run() services flush requests from the same select that drains
		// eventsCh, so one Flush() can win the race against an event still in
		// the buffer. Poll until the counters settle instead of asserting on
		// that ordering.
		deadline := time.Now().Add(3 * time.Second)
		var got ShipperStats
		for {
			Flush()
			got = Stats()
			if got.Flushed == 2 || time.Now().After(deadline) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}

		if got.Enqueued != 2 {
			t.Errorf("Stats().Enqueued = %d, want 2", got.Enqueued)
		}
		if got.Flushed != 2 {
			t.Errorf("Stats().Flushed = %d, want 2", got.Flushed)
		}
		if got.Dropped != 0 {
			t.Errorf("Stats().Dropped = %d, want 0", got.Dropped)
		}
		if received.Load() == 0 {
			t.Error("ingest endpoint was never called")
		}

		Shutdown()

		if after := Stats(); after != (ShipperStats{}) {
			t.Errorf("Stats() after Shutdown = %+v, want zero value", after)
		}
	})
}
