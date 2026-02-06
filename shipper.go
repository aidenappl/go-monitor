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

	// Prepare request body (optionally gzipped)
	var body io.Reader = &buf
	contentEncoding := ""

	if s.cfg.GzipEnabled {
		var gzipBuf bytes.Buffer
		gw := gzip.NewWriter(&gzipBuf)
		if _, err := gw.Write(buf.Bytes()); err != nil {
			fmt.Fprintf(os.Stderr, "monitor: gzip write failed: %v\n", err)
			return
		}
		if err := gw.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "monitor: gzip close failed: %v\n", err)
			return
		}
		body = &gzipBuf
		contentEncoding = "gzip"
	}

	// Send HTTP request
	req, err := http.NewRequest(http.MethodPost, s.cfg.IngestURL, body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "monitor: failed to create request: %v\n", err)
		return
	}

	req.Header.Set("Content-Type", "application/x-ndjson")
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	if s.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "monitor: failed to ship events: %v\n", err)
		return
	}
	defer resp.Body.Close()

	// Drain response body to allow connection reuse
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "monitor: ingest returned status %d\n", resp.StatusCode)
	}
}
