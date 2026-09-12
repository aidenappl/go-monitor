package monitor

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Spool defaults. Each is overridable through Config.
const (
	defaultSpoolMaxBytes     = 64 << 20
	defaultSpoolMinFreeBytes = 512 << 20
	defaultSpoolSyncEvery    = 500 * time.Millisecond
	defaultMaxBackoff        = 5 * time.Minute
	defaultDrainRate         = 5.0
)

const (
	// poisonMaxBytes caps poison.ndjson; the previous file is kept as
	// poison.1.ndjson, so quarantine can never use more than twice this.
	poisonMaxBytes = 4 << 20
	// diskCheckEvery is how often the free-disk floor is re-measured.
	diskCheckEvery = 5 * time.Second
)

// Tunables kept as variables so tests can shrink them.
var (
	// spoolSegmentBytes is the size at which the active segment is sealed even
	// if the drain has not asked for it. While Monitor is down the drain asks
	// for nothing, so this is what bounds the number of files an outage leaves.
	spoolSegmentBytes int64 = 4 << 20
	// spoolMaxSegments bounds the file count independently of bytes. A spool
	// that degrades into thousands of tiny files is a failure mode of its own:
	// Fluent Bit's filesystem buffers do exactly that, then fail to restart
	// with "too many open files".
	spoolMaxSegments = 256
	// spoolStartupJitter is the upper bound of the random delay before a
	// restarted process starts draining its backlog.
	spoolStartupJitter = 3 * time.Second
)

func spoolMaxBytes(cfg *Config) int64 {
	if cfg.SpoolMaxBytes > 0 {
		return cfg.SpoolMaxBytes
	}
	return defaultSpoolMaxBytes
}

// spoolMinFree returns the free-disk floor; a negative value disables the check.
func spoolMinFree(cfg *Config) int64 {
	if cfg.SpoolMinFreeBytes != 0 {
		return cfg.SpoolMinFreeBytes
	}
	return defaultSpoolMinFreeBytes
}

func spoolSyncEvery(cfg *Config) time.Duration {
	if cfg.SpoolSyncEvery > 0 {
		return cfg.SpoolSyncEvery
	}
	return defaultSpoolSyncEvery
}

func maxBackoff(cfg *Config) time.Duration {
	if cfg.MaxBackoff > 0 {
		return cfg.MaxBackoff
	}
	return defaultMaxBackoff
}

func drainRate(cfg *Config) float64 {
	if cfg.DrainRate > 0 {
		return cfg.DrainRate
	}
	return defaultDrainRate
}

// segment is one spool file. Once sealed its contents never change.
type segment struct {
	seq   uint64
	path  string
	bytes int64
	lines int64
}

// spool is a bounded, segmented, append-only NDJSON queue on local disk.
//
// Layout of <SpoolDir>/<service>/:
//
//	LOCK                    advisory lock: one process per spool directory
//	seg-00000000000000000001.ndjson ...   segments, oldest first
//	cursor                  "<seq> <offset>": bytes already delivered from the oldest segment
//	poison.ndjson           lines ingest refused as malformed
//
// Three limits keep it from ever harming the host it runs on — the failure
// that turns a telemetry buffer into an outage:
//   - bytes (SpoolMaxBytes): the oldest events are evicted, and counted as
//     dropped, before the cap is exceeded;
//   - files (spoolMaxSegments): same eviction, by count;
//   - free disk (SpoolMinFreeBytes): below the floor nothing more is written
//     at all. The cap alone is not enough on a host whose disk is already
//     near full for reasons of its own.
//
// Durability is batched: writes are fsynced every SpoolSyncEvery, never per
// event. A process crash loses nothing that was written (the page cache
// survives it); a host crash can lose at most that window.
type spool struct {
	dir          string
	maxBytes     int64
	minFree      int64
	syncEvery    time.Duration
	segmentBytes int64
	maxSegments  int
	lock         *os.File
	onDrop       func(int64)

	mu        sync.Mutex
	sealed    []*segment // oldest first
	active    *segment   // being appended to; nil when empty
	nextSeq   uint64
	draining  uint64 // seq the drain is working on (never evicted); 0 = none
	cursorSeq uint64
	cursorOff int64

	// Writer-goroutine state: touched only by writeLoop.
	activeFile    *os.File
	dirty         bool
	freeCheckedAt time.Time
	freeOK        bool

	pendingBytes atomic.Int64
	pendingLines atomic.Int64
	spooled      atomic.Int64 // lifetime lines written

	notify  chan struct{}      // wakes the drain after a write
	sealReq chan chan struct{} // drain → writer: seal the active segment

	poisonMu sync.Mutex
}

// openSpool opens (or creates) the spool for cfg and recovers whatever a
// previous process left in it.
func openSpool(cfg *Config, onDrop func(int64)) (*spool, error) {
	dir := filepath.Join(cfg.SpoolDir, safeDirName(cfg.Service))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	lock, err := lockDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%s is in use by another process (one process per spool directory): %w", dir, err)
	}
	sp := &spool{
		dir:          dir,
		maxBytes:     spoolMaxBytes(cfg),
		minFree:      spoolMinFree(cfg),
		syncEvery:    spoolSyncEvery(cfg),
		segmentBytes: spoolSegmentBytes,
		maxSegments:  spoolMaxSegments,
		lock:         lock,
		onDrop:       onDrop,
		nextSeq:      1,
		freeOK:       true,
		notify:       make(chan struct{}, 1),
		sealReq:      make(chan chan struct{}),
	}
	// At least four segments fit under the cap, so there is always a sealed
	// segment to evict ahead of the active one, which cannot be.
	if limit := sp.maxBytes / 4; sp.segmentBytes > limit {
		sp.segmentBytes = max(limit, 1)
	}
	if err := sp.recover(); err != nil {
		lock.Close()
		return nil, fmt.Errorf("recovering %s: %w", dir, err)
	}
	return sp, nil
}

// safeDirName turns a service name into a single path segment.
func safeDirName(s string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, s)
	if out == "" || out == "." || out == ".." {
		return "default"
	}
	return out
}

func segmentName(seq uint64) string { return fmt.Sprintf("seg-%020d.ndjson", seq) }

func parseSegmentName(name string) (uint64, bool) {
	if !strings.HasPrefix(name, "seg-") || !strings.HasSuffix(name, ".ndjson") {
		return 0, false
	}
	seq, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, "seg-"), ".ndjson"), 10, 64)
	return seq, err == nil && seq > 0
}

// recover rebuilds the spool's bookkeeping from the files on disk.
func (sp *spool) recover() error {
	entries, err := os.ReadDir(sp.dir)
	if err != nil {
		return err
	}
	var segs []*segment
	for _, e := range entries {
		seq, ok := parseSegmentName(e.Name())
		if !ok || e.IsDir() {
			continue
		}
		segs = append(segs, &segment{seq: seq, path: filepath.Join(sp.dir, e.Name())})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].seq < segs[j].seq })

	var kept []*segment
	var firstData []byte
	for _, sg := range segs {
		if sg.seq >= sp.nextSeq {
			sp.nextSeq = sg.seq + 1
		}
		data, err := os.ReadFile(sg.path)
		if err != nil {
			warnThrottled("spool-recover", "monitor: skipping unreadable spool segment %s: %v\n", sg.path, err)
			continue
		}
		// A crash mid-write leaves a torn final line. It was never whole, so
		// it was never going to be accepted; trim it rather than let it poison
		// a request.
		if n := len(data); n > 0 && data[n-1] != '\n' {
			cut := bytes.LastIndexByte(data, '\n') + 1
			if err := os.Truncate(sg.path, int64(cut)); err != nil {
				return fmt.Errorf("trimming torn write in %s: %w", sg.path, err)
			}
			data = data[:cut]
		}
		if len(data) == 0 {
			_ = os.Remove(sg.path)
			continue
		}
		sg.bytes = int64(len(data))
		sg.lines = int64(countLines(data))
		if len(kept) == 0 {
			firstData = data
		}
		kept = append(kept, sg)
	}
	sp.sealed = kept

	var total, lines int64
	for _, sg := range kept {
		total += sg.bytes
		lines += sg.lines
	}

	// Resume inside the oldest segment where the last process left off, so a
	// restart re-sends at most the one batch that was in flight.
	if len(kept) > 0 {
		if seq, off, ok := sp.readCursor(); ok && seq == kept[0].seq && off > 0 && off <= kept[0].bytes && firstData[off-1] == '\n' {
			sp.cursorSeq, sp.cursorOff = seq, off
			total -= off
			lines -= int64(countLines(firstData[:off]))
		}
	}
	sp.pendingBytes.Store(total)
	sp.pendingLines.Store(lines)

	if lines > 0 {
		fmt.Fprintf(stderrWriter, "monitor: spool recovered %d event(s) from a previous run in %s; delivering them in the background\n", lines, sp.dir)
	}
	return nil
}

func (sp *spool) cursorPath() string { return filepath.Join(sp.dir, "cursor") }

func (sp *spool) readCursor() (uint64, int64, bool) {
	b, err := os.ReadFile(sp.cursorPath())
	if err != nil {
		return 0, 0, false
	}
	var seq uint64
	var off int64
	if _, err := fmt.Sscanf(string(b), "%d %d", &seq, &off); err != nil {
		return 0, 0, false
	}
	return seq, off, true
}

// writeCursor persists delivery progress with write-then-rename, so a crash
// leaves either the old cursor or the new one, never half of one.
func (sp *spool) writeCursor(seq uint64, off int64) {
	tmp := sp.cursorPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d %d\n", seq, off)), 0o600); err != nil {
		warnThrottled("spool-cursor", "monitor: could not persist spool progress (a restart may re-send one batch): %v\n", err)
		return
	}
	if err := os.Rename(tmp, sp.cursorPath()); err != nil {
		warnThrottled("spool-cursor", "monitor: could not persist spool progress (a restart may re-send one batch): %v\n", err)
	}
}

// wake nudges the drain without blocking.
func (sp *spool) wake() {
	select {
	case sp.notify <- struct{}{}:
	default:
	}
}

// diskHasRoom reports whether free space on the spool's filesystem is above
// the floor. Measured at most every diskCheckEvery; a filesystem whose free
// space cannot be measured is assumed to have room. Writer goroutine only.
func (sp *spool) diskHasRoom() bool {
	if sp.minFree < 0 {
		return true
	}
	if time.Since(sp.freeCheckedAt) < diskCheckEvery {
		return sp.freeOK
	}
	sp.freeCheckedAt = time.Now()
	free, ok := diskFree(sp.dir)
	sp.freeOK = !ok || free >= sp.minFree
	return sp.freeOK
}

// append writes lines to the spool. Writer goroutine only.
//
// It never blocks on the network and never lets the spool outgrow its limits.
// Lines are placed one at a time: when the byte cap is reached the oldest
// spooled events are evicted to make room, and only a line that still cannot
// fit is refused. A batch is never dropped whole for being bigger than the room
// left, and segments are rotated mid-batch so each stays small enough to evict
// and to drain in one read. Everything evicted or refused is counted.
func (sp *spool) append(lines [][]byte) {
	if len(lines) == 0 {
		return
	}
	if !sp.diskHasRoom() {
		sp.onDrop(int64(len(lines)))
		warnThrottled("spool-disk-low",
			"monitor: free disk under %s is below %d bytes; dropping events instead of spooling them\n",
			sp.dir, sp.minFree)
		return
	}

	var buf bytes.Buffer
	var bufLines, refused, evicted int64

	// flush writes the buffered lines to the active segment in one write.
	flush := func() {
		if bufLines == 0 {
			return
		}
		if !sp.writeActive(buf.Bytes(), bufLines) {
			refused += bufLines
		}
		buf.Reset()
		bufLines = 0
	}

	for _, l := range lines {
		size := int64(len(l) + 1)

		if sp.activeFile != nil {
			if used := sp.activeBytes() + int64(buf.Len()); used > 0 && used+size > sp.segmentBytes {
				flush()
				sp.sealActive()
			}
		}
		if sp.activeFile == nil {
			sp.mu.Lock()
			evicted += sp.makeRoomLocked(0, true)
			canOpen := len(sp.sealed)+1 <= sp.maxSegments
			sp.mu.Unlock()
			if !canOpen {
				refused++
				continue
			}
			if err := sp.openActive(); err != nil {
				warnThrottled("spool-open", "monitor: could not open a spool segment in %s: %v\n", sp.dir, err)
				refused++
				continue
			}
		}

		sp.mu.Lock()
		need := int64(buf.Len()) + size
		evicted += sp.makeRoomLocked(need, false)
		fits := sp.pendingBytes.Load()+need <= sp.maxBytes
		sp.mu.Unlock()
		if !fits {
			refused++
			continue
		}
		buf.Write(l)
		buf.WriteByte('\n')
		bufLines++
	}
	flush()

	if evicted > 0 {
		sp.onDrop(evicted)
		warnThrottled("spool-evict",
			"monitor: spool at %s reached its limit; discarded the oldest spooled events to make room\n", sp.dir)
	}
	if refused > 0 {
		sp.onDrop(refused)
		warnThrottled("spool-full", "monitor: spool at %s could not take %d new event(s); they were dropped\n", sp.dir, refused)
	}
	sp.wake()
}

// writeActive appends b — n whole lines — to the active segment. Writer only.
func (sp *spool) writeActive(b []byte, n int64) bool {
	written, err := sp.activeFile.Write(b)
	if err != nil || written != len(b) {
		// A short write leaves a torn line; seal so nothing is appended after
		// it. Recovery trims a torn tail and bisection isolates anything else.
		warnThrottled("spool-write", "monitor: spool write to %s failed: %v\n", sp.dir, err)
		sp.sealActive()
		return false
	}
	size := int64(len(b))
	sp.mu.Lock()
	sp.active.bytes += size
	sp.active.lines += n
	sp.mu.Unlock()
	sp.pendingBytes.Add(size)
	sp.pendingLines.Add(n)
	sp.spooled.Add(n)
	sp.dirty = true
	return true
}

// activeBytes is the size of the not-yet-sealed segment.
func (sp *spool) activeBytes() int64 {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.active == nil {
		return 0
	}
	return sp.active.bytes
}

// makeRoomLocked evicts the oldest sealed segments (never the one being
// drained) until need more bytes fit, and until one more file fits if
// newFile. It returns how many events were evicted. Caller holds sp.mu.
func (sp *spool) makeRoomLocked(need int64, newFile bool) int64 {
	var evicted int64
	for {
		over := sp.pendingBytes.Load()+need > sp.maxBytes
		if newFile && len(sp.sealed)+1 > sp.maxSegments {
			over = true
		}
		if !over {
			return evicted
		}
		idx := -1
		for i, sg := range sp.sealed {
			if sg.seq != sp.draining {
				idx = i
				break
			}
		}
		if idx < 0 {
			return evicted
		}
		sg := sp.sealed[idx]
		sp.sealed = append(sp.sealed[:idx], sp.sealed[idx+1:]...)
		_ = os.Remove(sg.path)
		sp.pendingBytes.Add(-sg.bytes)
		sp.pendingLines.Add(-sg.lines)
		evicted += sg.lines
	}
}

// openActive creates the next segment file. Writer goroutine only.
func (sp *spool) openActive() error {
	sp.mu.Lock()
	seq := sp.nextSeq
	sp.nextSeq++
	sp.mu.Unlock()

	path := filepath.Join(sp.dir, segmentName(seq))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	sp.activeFile = f
	sp.mu.Lock()
	sp.active = &segment{seq: seq, path: path}
	sp.mu.Unlock()
	return nil
}

// syncActive fsyncs the active segment if anything was written since the last
// sync. Writer goroutine only.
func (sp *spool) syncActive() {
	if sp.activeFile != nil && sp.dirty {
		_ = sp.activeFile.Sync()
		sp.dirty = false
	}
}

// sealActive closes the active segment and hands it to the drain. Writer
// goroutine only.
func (sp *spool) sealActive() {
	if sp.activeFile == nil {
		return
	}
	sp.syncActive()
	_ = sp.activeFile.Close()
	sp.activeFile = nil

	sp.mu.Lock()
	if sp.active != nil {
		if sp.active.bytes > 0 {
			sp.sealed = append(sp.sealed, sp.active)
		} else {
			_ = os.Remove(sp.active.path)
		}
		sp.active = nil
	}
	sp.mu.Unlock()
	sp.wake()
}

// activeLines is how many events sit in the not-yet-sealed segment.
func (sp *spool) activeLines() int64 {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.active == nil {
		return 0
	}
	return sp.active.lines
}

// oldestSealed returns the next segment to drain and protects it from eviction.
func (sp *spool) oldestSealed() *segment {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.sealed) == 0 {
		return nil
	}
	sp.draining = sp.sealed[0].seq
	return sp.sealed[0]
}

// cursorFor returns how many bytes of segment seq were already delivered.
func (sp *spool) cursorFor(seq uint64, size int64) int64 {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.cursorSeq == seq && sp.cursorOff > 0 && sp.cursorOff <= size {
		return sp.cursorOff
	}
	return 0
}

// advance records that segment seq is delivered up to off, covering lines more
// events.
func (sp *spool) advance(seq uint64, off int64, lines int64) {
	sp.mu.Lock()
	prev := int64(0)
	if sp.cursorSeq == seq {
		prev = sp.cursorOff
	}
	sp.cursorSeq, sp.cursorOff = seq, off
	sp.mu.Unlock()

	sp.pendingBytes.Add(-(off - prev))
	sp.pendingLines.Add(-lines)
	sp.writeCursor(seq, off)
}

// finish deletes a fully delivered segment.
func (sp *spool) finish(seg *segment) {
	sp.mu.Lock()
	delivered := int64(0)
	if sp.cursorSeq == seg.seq {
		delivered = sp.cursorOff
	}
	sp.removeSealedLocked(seg.seq)
	sp.cursorSeq, sp.cursorOff = 0, 0
	sp.draining = 0
	sp.mu.Unlock()

	// Anything past the cursor that splitLines skipped (blank lines) is
	// released here so pendingBytes returns to zero.
	if rest := seg.bytes - delivered; rest > 0 {
		sp.pendingBytes.Add(-rest)
	}
	_ = os.Remove(seg.path)
	_ = os.Remove(sp.cursorPath())
}

// discard drops a segment that cannot be drained, counting its events lost.
func (sp *spool) discard(seg *segment) {
	sp.mu.Lock()
	delivered := int64(0)
	if sp.cursorSeq == seg.seq {
		delivered = sp.cursorOff
	}
	sp.removeSealedLocked(seg.seq)
	sp.cursorSeq, sp.cursorOff = 0, 0
	sp.draining = 0
	sp.mu.Unlock()

	sp.pendingBytes.Add(-(seg.bytes - delivered))
	remaining := seg.lines
	if pending := sp.pendingLines.Load(); remaining > pending {
		remaining = pending
	}
	sp.pendingLines.Add(-remaining)
	sp.onDrop(remaining)
	_ = os.Remove(seg.path)
	_ = os.Remove(sp.cursorPath())
}

func (sp *spool) removeSealedLocked(seq uint64) {
	for i, sg := range sp.sealed {
		if sg.seq == seq {
			sp.sealed = append(sp.sealed[:i], sp.sealed[i+1:]...)
			return
		}
	}
}

// countLines counts the lines splitLines would return, so recovery and the
// drain agree on what "pending" means.
func countLines(data []byte) int {
	lines, _ := splitLines(data)
	return len(lines)
}

// readSegment reads a sealed segment in full. Segments are bounded by
// spoolSegmentBytes plus one batch, so holding one in memory is cheap.
func readSegment(seg *segment) ([]byte, error) {
	return os.ReadFile(seg.path)
}

// writePoison appends lines ingest refused as malformed to poison.ndjson.
func (sp *spool) writePoison(lines [][]byte) {
	sp.poisonMu.Lock()
	defer sp.poisonMu.Unlock()

	path := filepath.Join(sp.dir, "poison.ndjson")
	if info, err := os.Stat(path); err == nil && info.Size() >= poisonMaxBytes {
		_ = os.Rename(path, filepath.Join(sp.dir, "poison.1.ndjson"))
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		warnThrottled("spool-poison", "monitor: could not write quarantined events to %s: %v\n", path, err)
		return
	}
	defer f.Close()
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	_, _ = f.Write(buf.Bytes())
}

// close releases the spool. The writer has already sealed and closed its
// segment by the time stop() calls this.
func (sp *spool) close() {
	if sp.activeFile != nil {
		_ = sp.activeFile.Sync()
		_ = sp.activeFile.Close()
		sp.activeFile = nil
	}
	if sp.lock != nil {
		_ = sp.lock.Close()
		sp.lock = nil
	}
}
