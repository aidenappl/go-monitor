package monitor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// shutdownFlushTimeout bounds how long stop() waits for the flush worker to
// ship any queued/in-flight batches before cancelling the shipper context to
// abort outstanding HTTP work. This keeps Shutdown() from blocking on the full
// 30s client timeout + retry backoff (see AGENTS.md §9 B4).
const shutdownFlushTimeout = 5 * time.Second

// flushWork is a unit of work handed from the accumulator goroutine (run) to
// the dedicated flush worker goroutine (shipLoop) over an ordered channel.
// A non-nil batch is shipped; a non-nil done channel is closed once all work
// enqueued before it has been processed (used to implement synchronous flush).
type flushWork struct {
	batch []Event
	done  chan struct{}
}

// shipper handles async batching and shipping of events to an ingest URL.
//
// It runs two goroutines:
//   - run():      drains eventsCh, accumulates a batch, and hands ready batches
//     to the flush worker. It never performs network I/O, so it can
//     keep draining eventsCh even while a flush is retrying.
//   - shipLoop(): a single worker that consumes batchCh in order and performs
//     the HTTP POST (with retries). A single worker preserves batch
//     ordering.
type shipper struct {
	cfg    *Config
	client *http.Client

	eventsCh   chan Event         // producer → run
	batchCh    chan flushWork     // run → shipLoop (ordered)
	flushReqCh chan chan struct{} // synchronous flush requests → run
	stopCh     chan struct{}      // closed by stop()
	stopOnce   sync.Once          // guards stopCh close (B5)
	runDoneCh  chan struct{}      // closed when run() returns
	flushDone  chan struct{}      // closed when shipLoop() returns

	ctx    context.Context // cancelled on stop to abort in-flight HTTP (B4)
	cancel context.CancelFunc

	// Lifetime counters, read by Stats(). Atomic because send() runs on every
	// caller's goroutine while shipLoop() runs on its own.
	enqueued atomic.Int64
	dropped  atomic.Int64
	flushed  atomic.Int64
}

// newShipper creates a new shipper with the given config.
func newShipper(cfg *Config) *shipper {
	ctx, cancel := context.WithCancel(context.Background())
	return &shipper{
		cfg:        cfg,
		client:     &http.Client{Timeout: 30 * time.Second},
		eventsCh:   make(chan Event, cfg.BatchSize*2),
		batchCh:    make(chan flushWork, 8),
		flushReqCh: make(chan chan struct{}),
		stopCh:     make(chan struct{}),
		runDoneCh:  make(chan struct{}),
		flushDone:  make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
	}
}

// start begins the shipper's background goroutines.
func (s *shipper) start() {
	go s.shipLoop()
	go s.run()
}

// stop signals the shipper to stop, waits for a final bounded drain+flush, and
// releases resources. Safe to call multiple times (B5).
func (s *shipper) stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)

		// Bound the total shutdown wait: if the flush worker hasn't finished
		// shipping queued batches within the timeout, cancel the context to
		// abort in-flight/pending HTTP so we don't block ~30-37s (B4).
		timer := time.AfterFunc(shutdownFlushTimeout, s.cancel)

		<-s.runDoneCh // run() drained eventsCh and closed batchCh
		<-s.flushDone // shipLoop() shipped everything (or was cancelled)

		timer.Stop()
		s.cancel()
	})
}

// send queues an event for shipping.
func (s *shipper) send(event Event) {
	select {
	case s.eventsCh <- event:
		s.enqueued.Add(1)
	default:
		// Buffer full: this event is gone. The stderr line stays because it is
		// the only signal a consumer that sets neither OnDrop nor reads Stats()
		// will ever get — but it must not be the only one available, since the
		// service that would have reported the problem is the one dropping.
		fmt.Fprintf(stderrWriter, "monitor: shipper buffer full, dropping event\n")
		s.recordDrop(1)
	}
}

// recordDrop accounts for n events the shipper will never deliver and notifies
// the consumer. Every path that abandons events routes through here so Stats()
// and OnDrop can never disagree with reality.
func (s *shipper) recordDrop(n int64) {
	if n <= 0 {
		return
	}
	total := s.dropped.Add(n)

	cb := s.cfg.OnDrop
	if cb == nil {
		return
	}
	// A consumer's callback must not be able to kill the goroutine that ships
	// everything else: losing some events is bad, losing the shipper is worse.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderrWriter, "monitor: OnDrop callback panicked: %v\n", r)
		}
	}()
	cb(total)
}

// flush synchronously flushes all buffered events, blocking until any batch
// enqueued before this call has been shipped.
func (s *shipper) flush() {
	done := make(chan struct{})
	select {
	case s.flushReqCh <- done:
		<-done
	case <-s.stopCh:
	}
}

// run accumulates events from eventsCh and hands ready batches to the flush
// worker. It performs no network I/O and therefore keeps draining eventsCh even
// while the flush worker is retrying (B2).
func (s *shipper) run() {
	defer close(s.runDoneCh)

	ticker := time.NewTicker(s.cfg.FlushEvery)
	defer ticker.Stop()

	var events []Event

	// tryHandoff hands the accumulated batch to the flush worker without
	// blocking. If the flush worker is busy and batchCh is full, the events are
	// retained and retried on the next opportunity rather than dropped.
	tryHandoff := func() {
		if len(events) == 0 {
			return
		}
		select {
		case s.batchCh <- flushWork{batch: events}:
			events = nil
		default:
			// Flush worker busy; keep accumulating and retry next tick.
		}
	}

	for {
		select {
		case event := <-s.eventsCh:
			events = append(events, event)
			if len(events) >= s.cfg.BatchSize {
				tryHandoff()
			}

		case <-ticker.C:
			tryHandoff()

		case done := <-s.flushReqCh:
			// Blocking sends here are intentional: an explicit flush must
			// enqueue the current batch and its marker in order, then the
			// caller waits on done (closed by shipLoop after prior work).
			if len(events) > 0 {
				s.batchCh <- flushWork{batch: events}
				events = nil
			}
			s.batchCh <- flushWork{done: done}

		case <-s.stopCh:
			s.drainAndStop(events)
			return
		}
	}
}

// drainAndStop pulls any remaining events out of eventsCh, appends them to the
// pending accumulator, hands them off in BatchSize-sized chunks (B8), and then
// closes batchCh so the flush worker finishes and exits.
func (s *shipper) drainAndStop(events []Event) {
	for {
		select {
		case event := <-s.eventsCh:
			events = append(events, event)
		default:
			// Chunk the drained events into BatchSize-sized batches.
			for len(events) > 0 {
				n := s.cfg.BatchSize
				if n > len(events) {
					n = len(events)
				}
				s.batchCh <- flushWork{batch: events[:n:n]}
				events = events[n:]
			}
			close(s.batchCh)
			return
		}
	}
}

// shipLoop is the single flush worker. It consumes batchCh in order, preserving
// batch ordering, and performs the HTTP POST for each batch.
func (s *shipper) shipLoop() {
	defer close(s.flushDone)
	for work := range s.batchCh {
		if len(work.batch) > 0 {
			s.shipBatch(work.batch)
		}
		if work.done != nil {
			close(work.done)
		}
	}
}

// shipBatch serializes the batch to NDJSON, optionally gzips it, and POSTs it to
// the ingest URL with up to maxRetries retries. All HTTP work is bound to the
// shipper context so it can be cancelled during shutdown (B4).
func (s *shipper) shipBatch(batch []Event) {
	// Build NDJSON payload. lines counts the events actually serialized — it,
	// not len(batch), is how many events this batch's outcome applies to.
	var buf bytes.Buffer
	var lines int64
	for _, event := range batch {
		jsonBytes, err := json.Marshal(event)
		if err != nil {
			fmt.Fprintf(stderrWriter, "monitor: failed to marshal event: %v\n", err)
			s.recordDrop(1)
			continue
		}
		buf.Write(jsonBytes)
		buf.WriteByte('\n')
		lines++
	}

	if buf.Len() == 0 {
		return
	}

	payload := buf.Bytes()

	// Compress once before the retry loop if gzip is enabled
	var shipPayload []byte
	if s.cfg.GzipEnabled {
		var gzipBuf bytes.Buffer
		gw := gzip.NewWriter(&gzipBuf)
		if _, err := gw.Write(payload); err != nil {
			fmt.Fprintf(stderrWriter, "monitor: gzip write failed: %v\n", err)
			s.recordDrop(lines)
			return
		}
		if err := gw.Close(); err != nil {
			fmt.Fprintf(stderrWriter, "monitor: gzip close failed: %v\n", err)
			s.recordDrop(lines)
			return
		}
		shipPayload = gzipBuf.Bytes()
	} else {
		shipPayload = payload
	}

	const maxRetries = 3

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s. Abort early if the context is
			// cancelled (e.g. during shutdown).
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			fmt.Fprintf(stderrWriter, "monitor: retrying flush (attempt %d/%d) after %v\n", attempt, maxRetries, backoff)
			select {
			case <-time.After(backoff):
			case <-s.ctx.Done():
				fmt.Fprintf(stderrWriter, "monitor: shutdown cancelled retrying flush, dropping batch\n")
				s.recordDrop(lines)
				return
			}
		}

		if s.ctx.Err() != nil {
			s.recordDrop(lines)
			return
		}

		req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.cfg.IngestURL, bytes.NewReader(shipPayload))
		if err != nil {
			fmt.Fprintf(stderrWriter, "monitor: failed to create request: %v\n", err)
			s.recordDrop(lines)
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
			// Network error (or context cancellation) — retry unless cancelled.
			fmt.Fprintf(stderrWriter, "monitor: failed to ship events: %v\n", err)
			if s.ctx.Err() != nil {
				s.recordDrop(lines)
				return
			}
			if attempt == maxRetries {
				fmt.Fprintf(stderrWriter, "monitor: dropping batch after %d retries\n", maxRetries)
				s.recordDrop(lines)
				return
			}
			continue
		}

		// Drain response body to allow connection reuse
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode < 400 {
			s.flushed.Add(lines)
			return // Success
		}

		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// Client error — don't retry. This is where a wrong or expired API
			// key lands, so it must count as loss: a Dropped that stayed at 0
			// through a 401 storm would be the most misleading number in the SDK.
			fmt.Fprintf(stderrWriter, "monitor: ingest returned status %d, not retrying\n", resp.StatusCode)
			s.recordDrop(lines)
			return
		}

		// 5xx — retry
		fmt.Fprintf(stderrWriter, "monitor: ingest returned status %d\n", resp.StatusCode)
		if attempt == maxRetries {
			fmt.Fprintf(stderrWriter, "monitor: dropping batch after %d retries\n", maxRetries)
			s.recordDrop(lines)
			return
		}
	}
}
