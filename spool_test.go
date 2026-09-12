package monitor

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s (stats %+v)", timeout, what, Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// spoolTest shrinks the spool's timing for one test and restores it after.
func spoolTest(t *testing.T) {
	t.Helper()
	withStderrBuffer(t)
	jitter, shutdown, flush, seg := spoolStartupJitter, shutdownFlushTimeout, flushDeliverTimeout, spoolSegmentBytes
	spoolStartupJitter = 0
	shutdownFlushTimeout = 300 * time.Millisecond
	flushDeliverTimeout = 3 * time.Second
	t.Cleanup(func() {
		Shutdown()
		spoolStartupJitter, shutdownFlushTimeout, flushDeliverTimeout, spoolSegmentBytes = jitter, shutdown, flush, seg
	})
}

func spoolConfig(dir, url, service string) Config {
	return Config{
		Service:           service,
		IngestURL:         url,
		SpoolDir:          dir,
		BatchSize:         500,
		FlushEvery:        20 * time.Millisecond,
		DisableStdout:     true,
		MaxBackoff:        100 * time.Millisecond,
		DrainRate:         1000,
		SpoolMinFreeBytes: -1,
	}
}

func emitN(prefix string, n int) {
	for i := 0; i < n; i++ {
		Emit(context.Background(), fmt.Sprintf("%s.%d", prefix, i), nil)
	}
}

func spooledLinesOnDisk(t *testing.T, dir string) int {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(dir, "seg-*.ndjson"))
	n := 0
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		n += countLines(b)
	}
	return n
}

func TestSpoolHoldsEventsUntilMonitorAnswers(t *testing.T) {
	spoolTest(t)
	f := newFakeIngest(t, "")
	f.status.Store(http.StatusServiceUnavailable)
	if err := Init(spoolConfig(t.TempDir(), f.URL(), "spool-outage")); err != nil {
		t.Fatal(err)
	}

	emitN("held", 40)
	waitFor(t, 3*time.Second, "all events spooled", func() bool { return Stats().Spooled == 40 })
	waitFor(t, 3*time.Second, "a delivery attempt", func() bool { return f.requests.Load() >= 2 })

	// Monitor is down: nothing delivered, nothing lost.
	if st := Stats(); st.Flushed != 0 || st.Dropped != 0 || st.Pending != 40 {
		t.Fatalf("while down: %+v, want flushed 0, dropped 0, pending 40", st)
	}

	f.status.Store(0)
	waitFor(t, 5*time.Second, "backlog delivered", func() bool { return Stats().Flushed == 40 })
	if st := Stats(); st.Pending != 0 || st.PendingBytes != 0 || st.Dropped != 0 {
		t.Errorf("after recovery: %+v, want nothing pending or dropped", st)
	}
	if got := len(f.received()); got != 40 {
		t.Errorf("ingest received %d events, want 40", got)
	}
}

func TestSpoolSurvivesAProcessRestart(t *testing.T) {
	spoolTest(t)
	dir := t.TempDir()
	f := newFakeIngest(t, "")
	f.status.Store(http.StatusServiceUnavailable)

	if err := Init(spoolConfig(dir, f.URL(), "spool-restart")); err != nil {
		t.Fatal(err)
	}
	emitN("restart", 25)
	waitFor(t, 3*time.Second, "events spooled", func() bool { return Stats().Spooled == 25 })
	Shutdown() // Monitor never came back while this process lived.

	if got := spooledLinesOnDisk(t, filepath.Join(dir, "spool-restart")); got != 25 {
		t.Fatalf("%d events on disk after shutdown, want all 25", got)
	}

	f.status.Store(0)
	if err := Init(spoolConfig(dir, f.URL(), "spool-restart")); err != nil { // the next process
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "recovered events delivered", func() bool { return len(f.received()) == 25 })
	waitFor(t, 2*time.Second, "spool emptied", func() bool { return Stats().Pending == 0 })
}

func TestSpoolHoldsEventsWhenCredentialsAreRefused(t *testing.T) {
	spoolTest(t)
	f := newFakeIngest(t, "")
	f.status.Store(http.StatusUnauthorized)
	if err := Init(spoolConfig(t.TempDir(), f.URL(), "spool-401")); err != nil {
		t.Fatal(err)
	}

	emitN("auth", 10)
	waitFor(t, 3*time.Second, "401s observed", func() bool { return f.requests.Load() >= 3 })
	if st := Stats(); st.Dropped != 0 || st.Pending != 10 {
		t.Fatalf("with a refused key: %+v, want events held (pending 10, dropped 0)", st)
	}

	f.status.Store(0) // the key was fixed
	waitFor(t, 5*time.Second, "held events delivered", func() bool { return Stats().Flushed == 10 })
}

func TestSpoolQuarantinesTheMalformedAndDeliversTheRest(t *testing.T) {
	spoolTest(t)
	dir := t.TempDir()
	f := newFakeIngest(t, "poison")
	if err := Init(spoolConfig(dir, f.URL(), "spool-poison")); err != nil {
		t.Fatal(err)
	}

	emitN("before", 5)
	Emit(context.Background(), "poison", nil)
	emitN("after", 5)
	waitFor(t, 5*time.Second, "good events delivered", func() bool { return Stats().Flushed == 10 })

	if st := Stats(); st.Quarantined != 1 || st.Dropped != 1 {
		t.Errorf("%+v, want exactly the malformed event quarantined", st)
	}
	poison, err := os.ReadFile(filepath.Join(dir, "spool-poison", "poison.ndjson"))
	if err != nil || !strings.Contains(string(poison), `"name":"poison"`) {
		t.Errorf("poison.ndjson = %q (%v), want the quarantined line kept for inspection", poison, err)
	}
}

func TestSpoolEvictsTheOldestWhenFull(t *testing.T) {
	spoolTest(t)
	spoolSegmentBytes = 1024
	f := newFakeIngest(t, "")
	f.status.Store(http.StatusServiceUnavailable)

	cfg := spoolConfig(t.TempDir(), f.URL(), "spool-full")
	cfg.SpoolMaxBytes = 8 << 10
	if err := Init(cfg); err != nil {
		t.Fatal(err)
	}

	const emitted = 300
	emitN("evict", emitted)
	waitFor(t, 5*time.Second, "writer caught up", func() bool {
		st := Stats()
		return st.Pending+st.Dropped == emitted
	})
	st := Stats()
	if st.PendingBytes > cfg.SpoolMaxBytes {
		t.Errorf("spool holds %d bytes, over its %d cap", st.PendingBytes, cfg.SpoolMaxBytes)
	}
	if st.Dropped == 0 {
		t.Fatal("expected the oldest events to be evicted")
	}

	f.status.Store(0)
	waitFor(t, 5*time.Second, "survivors delivered", func() bool { return Stats().Pending == 0 })
	got := f.received()
	if int64(len(got))+Stats().Dropped != emitted {
		t.Errorf("delivered %d + dropped %d != emitted %d", len(got), Stats().Dropped, emitted)
	}
	if len(got) == 0 {
		t.Fatal("nothing survived to be delivered")
	}
	if got[len(got)-1] != fmt.Sprintf("evict.%d", emitted-1) {
		t.Errorf("newest event %q should survive eviction", got[len(got)-1])
	}
}

func TestSpoolRespectsTheFreeDiskFloor(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("free space is not measurable on this platform")
	}
	spoolTest(t)
	f := newFakeIngest(t, "")
	cfg := spoolConfig(t.TempDir(), f.URL(), "spool-floor")
	cfg.SpoolMinFreeBytes = math.MaxInt64
	if err := Init(cfg); err != nil {
		t.Fatal(err)
	}
	emitN("floor", 10)
	waitFor(t, 3*time.Second, "events refused", func() bool { return Stats().Dropped == 10 })
	if st := Stats(); st.Spooled != 0 {
		t.Errorf("%+v, want nothing written below the floor", st)
	}
}

func TestSpoolAllowsOneProcessPerDirectory(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("advisory locking is not available on this platform")
	}
	buf := withStderrBuffer(t)
	cfg := &Config{Service: "locked", SpoolDir: t.TempDir(), IngestURL: "http://127.0.0.1:0", BatchSize: 10}

	first, err := openSpool(cfg, func(int64) {})
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()

	second := newShipper(cfg)
	if second.spool != nil {
		t.Fatal("a second process on the same spool directory must fall back to memory")
	}
	if !strings.Contains(buf.String(), "spool disabled") {
		t.Errorf("stderr %q should say the spool was disabled", buf.String())
	}
}

func TestSpoolRecoveryTrimsATornWrite(t *testing.T) {
	withStderrBuffer(t)
	dir := t.TempDir()
	sdir := filepath.Join(dir, "torn")
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	seg := filepath.Join(sdir, segmentName(1))
	if err := os.WriteFile(seg, []byte("{\"name\":\"a\"}\n{\"name\":\"b\"}\n{\"name\":\"to"), 0o600); err != nil {
		t.Fatal(err)
	}

	sp, err := openSpool(&Config{Service: "torn", SpoolDir: dir}, func(int64) {})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close()

	if got := sp.pendingLines.Load(); got != 2 {
		t.Errorf("pending = %d, want the 2 whole lines", got)
	}
	data, _ := os.ReadFile(seg)
	if !bytes.HasSuffix(data, []byte("\"b\"}\n")) {
		t.Errorf("segment = %q, want the torn tail trimmed", data)
	}
}

func TestSpoolResumesFromTheCursor(t *testing.T) {
	spoolTest(t)
	dir := t.TempDir()
	sdir := filepath.Join(dir, "cursor")
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	var seg bytes.Buffer
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&seg, `{"timestamp":%q,"service":"cursor","name":"c.%d","level":"info"}`+"\n", ts, i)
	}
	first := bytes.IndexByte(seg.Bytes(), '\n') + 1
	if err := os.WriteFile(filepath.Join(sdir, segmentName(1)), seg.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdir, "cursor"), []byte(fmt.Sprintf("1 %d\n", first)), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newFakeIngest(t, "")
	if err := Init(spoolConfig(dir, f.URL(), "cursor")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "resumed delivery", func() bool { return len(f.received()) == 2 })
	time.Sleep(100 * time.Millisecond)
	if got := f.received(); fmt.Sprint(got) != "[c.1 c.2]" {
		t.Errorf("delivered %v, want only what the last process had not delivered", got)
	}
}

func TestFlushDeliversSpooledEvents(t *testing.T) {
	spoolTest(t)
	f := newFakeIngest(t, "")
	if err := Init(spoolConfig(t.TempDir(), f.URL(), "spool-flush")); err != nil {
		t.Fatal(err)
	}
	Emit(context.Background(), "flush.me", nil)
	Flush()
	if st := Stats(); st.Flushed != 1 || st.Pending != 0 {
		t.Errorf("after Flush: %+v, want the event delivered", st)
	}
}
