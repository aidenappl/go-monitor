package monitor

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIngest stands in for monitor-core's POST /v1/events. Like the real thing
// it is all-or-nothing: a request carrying one line it refuses is rejected whole.
type fakeIngest struct {
	srv      *httptest.Server
	status   atomic.Int32 // non-zero: answer every request with this status
	requests atomic.Int32
	reject   string // an event with this name 400s the request carrying it

	mu    sync.Mutex
	names []string
}

func newFakeIngest(t *testing.T, reject string) *fakeIngest {
	t.Helper()
	f := &fakeIngest{reject: reject}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIngest) URL() string { return f.srv.URL + "/v1/events" }

func (f *fakeIngest) handle(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if code := f.status.Load(); code != 0 {
		w.WriteHeader(int(code))
		return
	}
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body = gz
	}
	var names []string
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if sc.Text() == "" {
			continue
		}
		var e struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || (f.reject != "" && e.Name == f.reject) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		names = append(names, e.Name)
	}
	if sc.Err() != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.names = append(f.names, names...)
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (f *fakeIngest) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.names...)
}

func testEvents(names ...string) []Event {
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	out := make([]Event, len(names))
	for i, n := range names {
		out[i] = Event{Timestamp: ts, Service: "bisect", Name: n, Level: "info"}
	}
	return out
}

func memShipper(url string) *shipper {
	return newShipper(&Config{Service: "bisect", IngestURL: url, BatchSize: 500, FlushEvery: time.Hour, DisableStdout: true})
}

func TestBisectionIsolatesTheMalformedLine(t *testing.T) {
	withStderrBuffer(t)
	f := newFakeIngest(t, "poison")
	s := memShipper(f.URL())

	names := []string{"e0", "e1", "e2", "e3", "poison", "e5", "e6", "e7", "e8", "e9"}
	s.shipBatch(testEvents(names...))

	got := f.received()
	want := []string{"e0", "e1", "e2", "e3", "e5", "e6", "e7", "e8", "e9"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("delivered %v, want %v in order", got, want)
	}
	if s.flushed.Load() != 9 || s.quarantined.Load() != 1 || s.dropped.Load() != 1 {
		t.Errorf("flushed=%d quarantined=%d dropped=%d, want 9/1/1", s.flushed.Load(), s.quarantined.Load(), s.dropped.Load())
	}
	if max := int32(2*bisectBudget(len(names)) + 1); f.requests.Load() > max {
		t.Errorf("used %d requests to isolate one line, want at most %d", f.requests.Load(), max)
	}
}

func TestBisectionBudgetBoundsASystemicRejection(t *testing.T) {
	withStderrBuffer(t)
	f := newFakeIngest(t, "")
	f.status.Store(http.StatusBadRequest)
	s := memShipper(f.URL())

	evs := make([]string, 64)
	for i := range evs {
		evs[i] = fmt.Sprintf("e%d", i)
	}
	s.shipBatch(testEvents(evs...))

	if s.quarantined.Load() != 64 || s.dropped.Load() != 64 {
		t.Errorf("quarantined=%d dropped=%d, want 64/64", s.quarantined.Load(), s.dropped.Load())
	}
	if max := int32(2*bisectBudget(64) + 1); f.requests.Load() > max {
		t.Errorf("sent %d requests for a batch ingest rejects wholesale, want at most %d", f.requests.Load(), max)
	}
}

func TestTooManyRequestsIsRetriedNotDropped(t *testing.T) {
	withStderrBuffer(t)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := memShipper(srv.URL)
	s.shipBatch(testEvents("throttled"))
	if s.flushed.Load() != 1 || s.dropped.Load() != 0 {
		t.Errorf("flushed=%d dropped=%d, want the 429 retried and delivered", s.flushed.Load(), s.dropped.Load())
	}
}

func TestClassifyStatus(t *testing.T) {
	for code, want := range map[int]shipResult{
		200: resultDelivered, 202: resultDelivered,
		400: resultRejected, 413: resultRejected, 422: resultRejected,
		401: resultMisconfigured, 403: resultMisconfigured, 404: resultMisconfigured, 405: resultMisconfigured,
		408: resultRetryable, 429: resultRetryable, 500: resultRetryable, 502: resultRetryable, 503: resultRetryable,
	} {
		if got := classifyStatus(code); got != want {
			t.Errorf("classifyStatus(%d) = %d, want %d", code, got, want)
		}
	}
}

func TestTakeChunk(t *testing.T) {
	line := make([]byte, 10)
	lines := [][]byte{line, line, line, line}
	if got := takeChunk(lines, 3, 1000); got != 3 {
		t.Errorf("maxLines 3: took %d", got)
	}
	if got := takeChunk(lines, 10, 25); got != 2 {
		t.Errorf("maxBytes 25 (11 bytes a line): took %d, want 2", got)
	}
	if got := takeChunk(lines, 10, 5); got != 1 {
		t.Errorf("a line bigger than maxBytes must still go alone: took %d, want 1", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("5"); got != 5*time.Second {
		t.Errorf("seconds form: %v", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("empty: %v", got)
	}
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 || got > 31*time.Second {
		t.Errorf("date form: %v", got)
	}
}
