package monitor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// shutdownFlushTimeout bounds how long stop() waits for queued work to be
// delivered before cancelling the shipper context to abort outstanding HTTP.
// It keeps Shutdown() from blocking on the full 30s client timeout plus retry
// backoff (see AGENTS.md §9 B4). With a spool, nothing is lost when it expires:
// whatever was not delivered stays on disk for the next start.
var shutdownFlushTimeout = 5 * time.Second

// flushDeliverTimeout bounds how long Flush waits, with a spool, for events
// already on disk to be delivered.
var flushDeliverTimeout = 5 * time.Second

// maxBodyBytes keeps one request under monitor-core's 10 MB body limit. The
// server applies that limit to the bytes on the wire BEFORE decompressing, so
// measuring the uncompressed payload is conservative whether or not gzip is on.
const maxBodyBytes = 8 << 20

// maxRetryAfterInMemory caps how long the in-memory shipper honours a
// Retry-After: it holds the only copy of the batch and blocks the flush worker
// for as long as it waits.
const maxRetryAfterInMemory = 10 * time.Second

// flushWork is a unit of work handed from the accumulator goroutine (run) to
// the goroutine that consumes batchCh, over an ordered channel. A non-nil batch
// is shipped (or spooled); a non-nil done channel is closed once all work
// enqueued before it has been processed (used to implement synchronous flush).
type flushWork struct {
	batch []Event
	done  chan struct{}
}

// shipper handles async batching and delivery of events to an ingest URL.
//
// Without a spool it runs two goroutines:
//   - run():      drains eventsCh, accumulates a batch, and hands ready batches
//     to the flush worker. It never performs I/O, so it keeps draining
//     eventsCh even while a flush is retrying.
//   - shipLoop(): a single worker that consumes batchCh in order and performs
//     the HTTP POST (with retries). A single worker preserves batch ordering.
//
// With a spool (Config.SpoolDir), shipLoop is replaced by two others:
//   - writeLoop(): appends every batch to the on-disk spool.
//   - drainLoop(): ships spooled events oldest-first, retrying until they are
//     delivered, and only then deletes them.
//
// Delivery then survives Monitor being down for hours, and the process being
// restarted in the meantime.
type shipper struct {
	cfg    *Config
	client *http.Client

	eventsCh   chan Event         // producer → run
	batchCh    chan flushWork     // run → shipLoop / writeLoop (ordered)
	flushReqCh chan chan struct{} // synchronous flush requests → run
	stopCh     chan struct{}      // closed by stop()
	stopOnce   sync.Once          // guards stopCh close (B5)
	runDoneCh  chan struct{}      // closed when run() returns
	flushDone  chan struct{}      // closed when batchCh's consumer returns
	drainDone  chan struct{}      // closed when drainLoop returns (closed at once without a spool)

	ctx    context.Context // cancelled on stop to abort in-flight HTTP (B4)
	cancel context.CancelFunc

	spool   *spool       // nil when events are buffered in memory only
	limiter *rateLimiter // paces the spool drain

	// Lifetime counters, read by Stats(). Atomic because send() runs on every
	// caller's goroutine while the delivery goroutines run on their own.
	enqueued    atomic.Int64
	dropped     atomic.Int64
	flushed     atomic.Int64
	quarantined atomic.Int64
}

// newShipper creates a new shipper with the given config.
//
// A spool that cannot be opened — an unwritable directory, or one already held
// by another process — degrades to the in-memory shipper with a loud warning.
// It never fails: telemetry must not be able to stop a service from starting.
func newShipper(cfg *Config) *shipper {
	ctx, cancel := context.WithCancel(context.Background())
	s := &shipper{
		cfg:        cfg,
		client:     &http.Client{Timeout: 30 * time.Second},
		eventsCh:   make(chan Event, batchSize(cfg)*2),
		batchCh:    make(chan flushWork, 8),
		flushReqCh: make(chan chan struct{}),
		stopCh:     make(chan struct{}),
		runDoneCh:  make(chan struct{}),
		flushDone:  make(chan struct{}),
		drainDone:  make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
	}
	if cfg.SpoolDir != "" {
		sp, err := openSpool(cfg, s.recordDrop)
		if err != nil {
			fmt.Fprintf(stderrWriter, "monitor: spool disabled — events are buffered in memory only and will be lost if Monitor is unreachable: %v\n", err)
		} else {
			s.spool = sp
			s.limiter = newRateLimiter(drainRate(cfg))
		}
	}
	if s.spool == nil {
		close(s.drainDone)
	}
	return s
}

func batchSize(cfg *Config) int {
	if cfg.BatchSize > 0 {
		return cfg.BatchSize
	}
	return 200
}

// start begins the shipper's background goroutines.
func (s *shipper) start() {
	if s.spool != nil {
		go s.writeLoop()
		go s.drainLoop()
	} else {
		go s.shipLoop()
	}
	go s.run()
}

// stop signals the shipper to stop, waits for a final bounded drain+flush, and
// releases resources. Safe to call multiple times (B5).
func (s *shipper) stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)

		// Bound the total shutdown wait: if queued work isn't delivered within
		// the timeout, cancel the context to abort in-flight/pending HTTP so
		// we don't block ~30-37s (B4).
		timer := time.AfterFunc(shutdownFlushTimeout, s.cancel)

		<-s.runDoneCh // run() drained eventsCh and closed batchCh
		<-s.flushDone // everything was shipped, or (with a spool) written to disk
		<-s.drainDone // the drain delivered what it could in the time left

		timer.Stop()
		s.cancel()
		if s.spool != nil {
			s.spool.close()
		}
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
//
// With a spool the ordered marker only proves the batch reached disk, so flush
// then gives the drain a bounded window to deliver it. Whatever it cannot
// deliver in time is not lost — it is spooled, and ships once Monitor answers.
func (s *shipper) flush() {
	done := make(chan struct{})
	select {
	case s.flushReqCh <- done:
		<-done
	case <-s.stopCh:
		return
	}
	if s.spool == nil {
		return
	}
	s.spool.wake()
	deadline := time.Now().Add(flushDeliverTimeout)
	for s.spool.pendingLines.Load() > 0 && time.Now().Before(deadline) {
		select {
		case <-s.stopCh:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// run accumulates events from eventsCh and hands ready batches to the flush
// worker. It performs no I/O and therefore keeps draining eventsCh even while
// delivery is retrying (B2).
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
			if len(events) >= batchSize(s.cfg) {
				tryHandoff()
			}

		case <-ticker.C:
			tryHandoff()

		case done := <-s.flushReqCh:
			// Take what is already buffered first. select picks among ready
			// cases at random, so a flush request can be serviced ahead of an
			// event that was sent before it — and an event emitted right before
			// Flush (the whole point of Fatal) would miss the flush it preceded.
			// Only run receives from eventsCh, so these receives cannot block.
			for n := len(s.eventsCh); n > 0; n-- {
				events = append(events, <-s.eventsCh)
			}

			// Blocking sends here are intentional: an explicit flush must
			// enqueue the current batch and its marker in order, then the
			// caller waits on done (closed after all prior work).
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
			for len(events) > 0 {
				n := batchSize(s.cfg)
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

// shipLoop is the in-memory flush worker. It consumes batchCh in order,
// preserving batch ordering, and ships each batch.
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

// shipBatch delivers one batch from memory, with the in-memory retry policy.
// Whatever cannot be delivered is counted as dropped — memory is the only copy.
func (s *shipper) shipBatch(batch []Event) {
	lines := s.marshalBatch(batch)
	for len(lines) > 0 {
		n := takeChunk(lines, len(lines), maxBodyBytes)
		resolved, _ := s.deliver(lines[:n], memoryPolicy)
		if resolved < n {
			// Retries exhausted, credentials refused, or shutting down. The
			// rest of this batch would meet the same fate after the same wait.
			s.recordDrop(int64(len(lines) - resolved))
			return
		}
		lines = lines[n:]
	}
}

// marshalBatch serializes events to NDJSON lines. An event that cannot be
// serialized is counted as dropped rather than allowed to fail the batch.
func (s *shipper) marshalBatch(batch []Event) [][]byte {
	lines := make([][]byte, 0, len(batch))
	for _, event := range batch {
		b, err := marshalLine(event)
		if err != nil {
			fmt.Fprintf(stderrWriter, "monitor: failed to marshal event: %v\n", err)
			s.recordDrop(1)
			continue
		}
		lines = append(lines, b)
	}
	return lines
}

// takeChunk returns how many leading lines fit in one request of at most
// maxLines lines and maxBytes bytes. It always takes at least one line: a line
// is never larger than maxLineBytes, which is far below maxBodyBytes.
func takeChunk(lines [][]byte, maxLines, maxBytes int) int {
	size := 0
	for i, l := range lines {
		if i >= maxLines || (i > 0 && size+len(l)+1 > maxBytes) {
			return i
		}
		size += len(l) + 1
	}
	return len(lines)
}

// shipResult classifies one delivery attempt.
type shipResult int

const (
	// resultDelivered: ingest accepted the request.
	resultDelivered shipResult = iota
	// resultRejected: ingest refused the CONTENT (400/413/422). Sending the
	// same bytes again will fail the same way; some line in it is malformed.
	resultRejected
	// resultMisconfigured: ingest refused the credentials or the endpoint
	// (401/403/404/405). Nothing will be accepted until configuration changes.
	resultMisconfigured
	// resultRetryable: a transient failure — network, 5xx, 408, 429.
	resultRetryable
	// resultCancelled: the shipper is stopping.
	resultCancelled
)

func classifyStatus(code int) shipResult {
	switch {
	case code < 400:
		return resultDelivered
	case code == http.StatusRequestTimeout, code == http.StatusTooManyRequests:
		return resultRetryable
	case code == http.StatusUnauthorized, code == http.StatusForbidden,
		code == http.StatusNotFound, code == http.StatusMethodNotAllowed:
		return resultMisconfigured
	case code < 500:
		return resultRejected
	default:
		return resultRetryable
	}
}

// retryPolicy decides how persistently one request is retried.
type retryPolicy struct {
	// maxRetries bounds retries of transient failures; -1 retries until the
	// shipper stops.
	maxRetries int
	// holdMisconfigured keeps retrying (with backoff) when ingest refuses the
	// credentials or endpoint, instead of giving the events up. A rotated API
	// key is the classic cause, and the fix — a redeploy with the new key —
	// comes after the events were emitted.
	holdMisconfigured bool
	// maxRetryAfter caps how long a Retry-After header may delay a retry.
	maxRetryAfter time.Duration
	// backoff returns the delay before retry n (n starts at 1).
	backoff func(n int) time.Duration
}

// memoryPolicy is the in-memory shipper's historical schedule: three retries
// at 1s, 2s and 4s. It is unchanged so a service that never sets SpoolDir sees
// exactly the timing it always has.
var memoryPolicy = retryPolicy{
	maxRetries:    3,
	maxRetryAfter: maxRetryAfterInMemory,
	backoff:       func(n int) time.Duration { return time.Duration(1<<uint(n-1)) * time.Second },
}

// spoolPolicy retries forever with full-jitter exponential backoff capped at
// MaxBackoff. Events are on disk, so waiting costs nothing but disk; giving up
// would throw away the only copy.
//
// Full jitter matters here specifically: every service in a fleet sees Monitor
// come back at the same moment, and a deterministic schedule would bring their
// backlogs back as one synchronized wave — the herd that kept Google Cloud's
// us-central-1 down for ~2h40m in June 2025.
func (s *shipper) spoolPolicy() retryPolicy {
	ceiling := maxBackoff(s.cfg)
	return retryPolicy{
		maxRetries:        -1,
		holdMisconfigured: true,
		maxRetryAfter:     ceiling,
		backoff: func(n int) time.Duration {
			d := ceiling
			if n < 30 {
				if exp := time.Second << uint(n-1); exp < ceiling {
					d = exp
				}
			}
			return 50*time.Millisecond + time.Duration(rand.Int64N(int64(d)))
		},
	}
}

// sendWithRetry posts lines, retrying transient failures per pol. It returns
// the final classification and the last HTTP status seen.
func (s *shipper) sendWithRetry(lines [][]byte, pol retryPolicy) (shipResult, int) {
	var retryAfter time.Duration
	var status int
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			delay := pol.backoff(attempt)
			if retryAfter > delay {
				delay = min(retryAfter, pol.maxRetryAfter)
			}
			if pol.maxRetries >= 0 {
				fmt.Fprintf(stderrWriter, "monitor: retrying flush (attempt %d/%d) after %v\n", attempt, pol.maxRetries, delay)
			}
			select {
			case <-time.After(delay):
			case <-s.ctx.Done():
				return resultCancelled, status
			}
		}

		var res shipResult
		res, retryAfter, status = s.post(lines)
		switch res {
		case resultDelivered, resultRejected, resultCancelled:
			return res, status

		case resultMisconfigured:
			if !pol.holdMisconfigured {
				fmt.Fprintf(stderrWriter, "monitor: ingest returned status %d, not retrying\n", status)
				return res, status
			}
			warnThrottled("ingest-misconfigured",
				"monitor: ingest refused the request with status %d — check IngestURL and APIKey. Events are held in the spool and will ship once it accepts them.\n",
				status)

		case resultRetryable:
			if pol.maxRetries >= 0 && attempt >= pol.maxRetries {
				fmt.Fprintf(stderrWriter, "monitor: dropping batch after %d retries\n", pol.maxRetries)
				return res, status
			}
			if pol.maxRetries < 0 {
				warnThrottled("ingest-unreachable",
					"monitor: ingest unavailable (status %d); spooled events will be retried with backoff up to %v\n",
					status, maxBackoff(s.cfg))
			}
		}
	}
}

// post sends lines as one NDJSON request and classifies the outcome.
func (s *shipper) post(lines [][]byte) (shipResult, time.Duration, int) {
	if s.ctx.Err() != nil {
		return resultCancelled, 0, 0
	}

	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	body := buf.Bytes()
	gzipped := false
	if s.cfg.GzipEnabled {
		var gz bytes.Buffer
		gw := gzip.NewWriter(&gz)
		if _, err := gw.Write(body); err == nil && gw.Close() == nil {
			body = gz.Bytes()
			gzipped = true
		} else {
			// Compression is an optimization; send the batch uncompressed
			// rather than lose it.
			warnThrottled("gzip-failed", "monitor: gzip failed, sending uncompressed\n")
		}
	}

	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.cfg.IngestURL, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(stderrWriter, "monitor: failed to create request: %v\n", err)
		return resultMisconfigured, 0, 0
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if s.cfg.APIKey != "" {
		req.Header.Set("X-Api-Key", s.cfg.APIKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		if s.ctx.Err() != nil {
			return resultCancelled, 0, 0
		}
		if s.spool == nil {
			fmt.Fprintf(stderrWriter, "monitor: failed to ship events: %v\n", err)
		}
		return resultRetryable, 0, 0
	}
	// Drain the body to allow connection reuse.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()

	res := classifyStatus(resp.StatusCode)
	if res == resultRetryable && s.spool == nil {
		fmt.Fprintf(stderrWriter, "monitor: ingest returned status %d\n", resp.StatusCode)
	}
	return res, parseRetryAfter(resp.Header.Get("Retry-After")), resp.StatusCode
}

// parseRetryAfter reads a Retry-After header in either of its two forms.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// deliver ships lines in order, isolating any line ingest refuses by bisecting
// the request that carried it.
//
// monitor-core rejects a whole request when one line is malformed, so without
// this a single bad event takes every other event it was batched with — and a
// spool makes batches bigger and longer-lived, which only widens the blast
// radius. Events are sanitized before they are batched, so this is the safety
// net for whatever sanitization did not anticipate.
//
// It returns how many LEADING lines are resolved — delivered, or quarantined
// as malformed — and why it stopped short if it did.
func (s *shipper) deliver(lines [][]byte, pol retryPolicy) (int, shipResult) {
	budget := bisectBudget(len(lines))
	return s.deliverRange(lines, pol, &budget)
}

// bisectBudget bounds the extra requests spent isolating malformed lines. It
// affords isolating a handful of bad lines; a request with more than that is
// rejected for a systemic reason, and splitting it into single-event requests
// would hammer ingest for no gain.
func bisectBudget(n int) int {
	depth := 0
	for x := n; x > 1; x = (x + 1) / 2 {
		depth++
	}
	return 4*depth + 4
}

func (s *shipper) deliverRange(lines [][]byte, pol retryPolicy, budget *int) (int, shipResult) {
	res, status := s.sendWithRetry(lines, pol)
	switch res {
	case resultDelivered:
		s.flushed.Add(int64(len(lines)))
		return len(lines), resultDelivered

	case resultRejected:
		if len(lines) == 1 || *budget <= 0 {
			s.quarantine(lines, status)
			return len(lines), resultDelivered
		}
		*budget--
		mid := len(lines) / 2
		n, r := s.deliverRange(lines[:mid], pol, budget)
		if n < mid {
			return n, r
		}
		m, r := s.deliverRange(lines[mid:], pol, budget)
		return mid + m, r

	default:
		return 0, res
	}
}

// quarantine gives up on lines ingest refused as malformed. They count as
// dropped — they will never reach Monitor — and, with a spool, are kept in
// poison.ndjson so the defect that produced them can be found and fixed.
func (s *shipper) quarantine(lines [][]byte, status int) {
	n := int64(len(lines))
	s.quarantined.Add(n)
	s.recordDrop(n)
	warnThrottled("quarantine",
		"monitor: ingest rejected %d event(s) as malformed (status %d) and they were quarantined: %s\n",
		n, status, eventNames(lines, 3))
	if s.spool != nil {
		s.spool.writePoison(lines)
	}
}

// eventNames summarizes lines by event name for a diagnostic, without echoing
// their payloads.
func eventNames(lines [][]byte, limit int) string {
	var names []string
	for _, l := range lines {
		if len(names) == limit {
			names = append(names, "…")
			break
		}
		var e struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(l, &e) == nil && e.Name != "" {
			names = append(names, e.Name)
		} else {
			names = append(names, "<unparseable>")
		}
	}
	return strings.Join(names, ", ")
}

// writeLoop is the spool writer: it consumes batchCh in order and appends every
// batch to disk. It owns the active segment file.
func (s *shipper) writeLoop() {
	defer close(s.flushDone)
	sp := s.spool

	ticker := time.NewTicker(sp.syncEvery)
	defer ticker.Stop()

	for {
		select {
		case work, ok := <-s.batchCh:
			if !ok {
				sp.sealActive()
				return
			}
			if len(work.batch) > 0 {
				sp.append(s.marshalBatch(work.batch))
			}
			if work.done != nil {
				sp.syncActive()
				close(work.done)
			}

		case reply := <-sp.sealReq:
			sp.sealActive()
			close(reply)

		case <-ticker.C:
			sp.syncActive()
		}
	}
}

// drainLoop ships spooled events oldest-first until the shipper stops.
func (s *shipper) drainLoop() {
	defer close(s.drainDone)
	sp := s.spool

	// Desynchronize a fleet that restarts together so the backlog of every
	// service does not reach Monitor in the same instant.
	if spoolStartupJitter > 0 {
		select {
		case <-time.After(time.Duration(rand.Int64N(int64(spoolStartupJitter)))):
		case <-s.ctx.Done():
			return
		case <-s.flushDone:
		}
	}

	ticker := time.NewTicker(s.cfg.FlushEvery)
	defer ticker.Stop()

	for {
		if s.ctx.Err() != nil {
			return
		}
		if seg := sp.oldestSealed(); seg != nil {
			if !s.drainSegment(seg) {
				return
			}
			continue
		}
		if sp.activeLines() > 0 {
			s.requestSeal()
			continue
		}
		select {
		case <-s.flushDone:
			// The writer has exited, sealing the active segment on its way
			// out. With nothing left on disk, the drain is finished too.
			if sp.oldestSealed() == nil && sp.activeLines() == 0 {
				return
			}
		case <-sp.notify:
		case <-ticker.C:
		case <-s.ctx.Done():
			return
		}
	}
}

// requestSeal asks the writer to close the active segment so it can be drained.
func (s *shipper) requestSeal() {
	reply := make(chan struct{})
	select {
	case s.spool.sealReq <- reply:
		select {
		case <-reply:
		case <-s.flushDone:
		case <-s.ctx.Done():
		}
	case <-s.flushDone:
	case <-s.ctx.Done():
	}
}

// drainSegment delivers one sealed segment and deletes it. It returns false
// when the shipper is stopping; whatever was not delivered stays on disk behind
// the persisted cursor and is resumed on the next start.
func (s *shipper) drainSegment(seg *segment) bool {
	sp := s.spool
	data, err := readSegment(seg)
	if err != nil {
		// A segment that cannot be read would wedge the drain forever. Count
		// it lost and move on to the rest.
		warnThrottled("spool-read", "monitor: discarding unreadable spool segment %s: %v\n", seg.path, err)
		sp.discard(seg)
		return true
	}

	start := sp.cursorFor(seg.seq, int64(len(data)))
	lines, ends := splitLines(data[start:])
	for i := 0; i < len(lines); {
		n := takeChunk(lines[i:], batchSize(s.cfg), maxBodyBytes)
		if !s.limiter.wait(s.ctx) {
			return false
		}
		resolved, _ := s.deliver(lines[i:i+n], s.spoolPolicy())
		if resolved > 0 {
			sp.advance(seg.seq, start+int64(ends[i+resolved-1]), int64(resolved))
		}
		if resolved < n {
			// The spool policy retries everything except cancellation.
			return false
		}
		i += n
	}
	sp.finish(seg)
	return true
}

// splitLines splits NDJSON into its non-empty lines, returning for each the
// offset (relative to data) just past its newline.
func splitLines(data []byte) ([][]byte, []int) {
	var lines [][]byte
	var ends []int
	pos := 0
	for pos < len(data) {
		i := bytes.IndexByte(data[pos:], '\n')
		if i < 0 {
			// A sealed segment always ends in a newline; a torn tail can only
			// come from a crash mid-write, and recovery trims it. Stop here.
			break
		}
		line := data[pos : pos+i]
		pos += i + 1
		if len(line) == 0 {
			continue
		}
		lines = append(lines, line)
		ends = append(ends, pos)
	}
	return lines, ends
}

// rateLimiter spaces spool deliveries so a large backlog drains at a steady,
// bounded rate instead of arriving at Monitor as one burst.
type rateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newRateLimiter(perSecond float64) *rateLimiter {
	if perSecond <= 0 {
		return &rateLimiter{}
	}
	return &rateLimiter{interval: time.Duration(float64(time.Second) / perSecond)}
}

// wait blocks until the next delivery slot. It returns false if ctx ends first.
func (l *rateLimiter) wait(ctx context.Context) bool {
	if l == nil || l.interval <= 0 {
		return ctx.Err() == nil
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	at := l.next
	l.next = l.next.Add(l.interval)
	l.mu.Unlock()

	d := time.Until(at)
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
