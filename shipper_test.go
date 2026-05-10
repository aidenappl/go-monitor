package monitor

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestShipperRetry(t *testing.T) {
	t.Run("retries on 5xx", func(t *testing.T) {
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count := attempts.Add(1)
			if count <= 2 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		cfg := &Config{
			Service:       "test-retry",
			IngestURL:     server.URL,
			BatchSize:     10,
			FlushEvery:    time.Second,
			DisableStdout: true,
		}

		s := newShipper(cfg)

		// Add an event and flush
		s.events = append(s.events, Event{
			Name:      "test.retry",
			Service:   "test",
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Level:     "info",
		})

		s.doFlush()

		got := int(attempts.Load())
		if got != 3 {
			t.Errorf("attempts = %d, want 3 (2 failures + 1 success)", got)
		}
	})

	t.Run("does not retry on 4xx", func(t *testing.T) {
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		}))
		defer server.Close()

		cfg := &Config{
			Service:       "test-no-retry",
			IngestURL:     server.URL,
			BatchSize:     10,
			FlushEvery:    time.Second,
			DisableStdout: true,
		}

		s := newShipper(cfg)
		s.events = append(s.events, Event{
			Name:      "test.no-retry",
			Service:   "test",
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Level:     "info",
		})

		s.doFlush()

		got := int(attempts.Load())
		if got != 1 {
			t.Errorf("attempts = %d, want 1 (no retry on 4xx)", got)
		}
	})

	t.Run("succeeds on first try", func(t *testing.T) {
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		cfg := &Config{
			Service:       "test-success",
			IngestURL:     server.URL,
			BatchSize:     10,
			FlushEvery:    time.Second,
			DisableStdout: true,
		}

		s := newShipper(cfg)
		s.events = append(s.events, Event{
			Name:      "test.success",
			Service:   "test",
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			Level:     "info",
		})

		s.doFlush()

		got := int(attempts.Load())
		if got != 1 {
			t.Errorf("attempts = %d, want 1", got)
		}
	})
}
