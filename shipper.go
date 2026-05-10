package monitor

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// shipper handles async batching and shipping of events to an ingest URL.
type shipper struct {
	cfg      *Config
	client   *http.Client
	events   []Event
	mu       sync.Mutex
	stopCh   chan struct{}
	doneCh   chan struct{}
	flushCh  chan chan struct{}
	eventsCh chan Event
}

// newShipper creates a new shipper with the given config.
func newShipper(cfg *Config) *shipper {
	return &shipper{
		cfg:      cfg,
		client:   &http.Client{Timeout: 30 * time.Second},
		events:   make([]Event, 0, cfg.BatchSize),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
		flushCh:  make(chan chan struct{}),
		eventsCh: make(chan Event, cfg.BatchSize*2),
	}
}

// start begins the shipper's background goroutine.
func (s *shipper) start() {
	go s.run()
}

// stop signals the shipper to stop and waits for it to finish.
func (s *shipper) stop() {
	close(s.stopCh)
	<-s.doneCh
}

// send queues an event for shipping.
func (s *shipper) send(event Event) {
	select {
	case s.eventsCh <- event:
	default:
		// Channel full, drop event (could log this in debug mode)
		fmt.Fprintf(os.Stderr, "monitor: shipper buffer full, dropping event\n")
	}
}

// flush synchronously flushes all buffered events.
func (s *shipper) flush() {
	done := make(chan struct{})
	select {
	case s.flushCh <- done:
		<-done
	case <-s.stopCh:
	}
}

// run is the main loop for the shipper goroutine.
func (s *shipper) run() {
	defer close(s.doneCh)

	ticker := time.NewTicker(s.cfg.FlushEvery)
	defer ticker.Stop()

	for {
		select {
		case event := <-s.eventsCh:
			s.mu.Lock()
			s.events = append(s.events, event)
			shouldFlush := len(s.events) >= s.cfg.BatchSize
			s.mu.Unlock()

			if shouldFlush {
				s.doFlush()
			}

		case <-ticker.C:
			s.doFlush()

		case done := <-s.flushCh:
			s.doFlush()
			close(done)

		case <-s.stopCh:
			// Drain remaining events from channel
			for {
				select {
				case event := <-s.eventsCh:
					s.mu.Lock()
					s.events = append(s.events, event)
					s.mu.Unlock()
				default:
					s.doFlush()
					return
				}
			}
		}
	}
}

// doFlush sends the current batch to the ingest URL.
func (s *shipper) doFlush() {
	s.mu.Lock()
	if len(s.events) == 0 {
		s.mu.Unlock()
		return
	}

	// Take the current batch
	batch := s.events
	s.events = make([]Event, 0, s.cfg.BatchSize)
	s.mu.Unlock()

	// Build NDJSON payload
	var buf bytes.Buffer
	for _, event := range batch {
		jsonBytes, err := json.Marshal(event)
		if err != nil {
			fmt.Fprintf(os.Stderr, "monitor: failed to marshal event: %v\n", err)
			continue
		}
		buf.Write(jsonBytes)
		buf.WriteByte('\n')
	}

	if buf.Len() == 0 {
		return
	}

	// Prepare payload bytes for potential retries
	payload := buf.Bytes()

	// Compress once before the retry loop if gzip is enabled
	var shipPayload []byte
	if s.cfg.GzipEnabled {
		var gzipBuf bytes.Buffer
		gw := gzip.NewWriter(&gzipBuf)
		if _, err := gw.Write(payload); err != nil {
			fmt.Fprintf(os.Stderr, "monitor: gzip write failed: %v\n", err)
			return
		}
		if err := gw.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "monitor: gzip close failed: %v\n", err)
			return
		}
		shipPayload = gzipBuf.Bytes()
	} else {
		shipPayload = payload
	}

	const maxRetries = 3

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			fmt.Fprintf(os.Stderr, "monitor: retrying flush (attempt %d/%d) after %v\n", attempt, maxRetries, backoff)
			time.Sleep(backoff)
		}

		req, err := http.NewRequest(http.MethodPost, s.cfg.IngestURL, bytes.NewReader(shipPayload))
		if err != nil {
			fmt.Fprintf(os.Stderr, "monitor: failed to create request: %v\n", err)
			return
		}

		req.Header.Set("Content-Type", "application/x-ndjson")
		if s.cfg.GzipEnabled {
			req.Header.Set("Content-Encoding", "gzip")
		}
		if s.cfg.APIKey != "" {
			req.Header.Set("X-Api-Key", s.cfg.APIKey)
		}

		resp, err := s.client.Do(req)
		if err != nil {
			// Network error — retry
			fmt.Fprintf(os.Stderr, "monitor: failed to ship events: %v\n", err)
			if attempt == maxRetries {
				fmt.Fprintf(os.Stderr, "monitor: dropping batch after %d retries\n", maxRetries)
				return
			}
			continue
		}

		// Drain response body to allow connection reuse
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode < 400 {
			return // Success
		}

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// Client error — don't retry
			fmt.Fprintf(os.Stderr, "monitor: ingest returned status %d, not retrying\n", resp.StatusCode)
			return
		}

		// 5xx — retry
		fmt.Fprintf(os.Stderr, "monitor: ingest returned status %d\n", resp.StatusCode)
		if attempt == maxRetries {
			fmt.Fprintf(os.Stderr, "monitor: dropping batch after %d retries\n", maxRetries)
			return
		}
	}
}
