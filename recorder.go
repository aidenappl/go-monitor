package monitor

import (
	"sync"
	"sync/atomic"
)

// Recorder captures events in memory instead of printing or shipping them, so
// tests can assert on what a service emits.
//
// It intercepts after sanitization and redaction, so a recorded event is
// exactly what would have been shipped — a test can prove a secret never
// leaves the process, not just that some code path ran. It works with or
// without Init: nothing is printed and nothing reaches the network while it is
// active.
//
//	rec := monitor.StartRecording()
//	defer rec.Stop()
//	handler.ServeHTTP(w, r)
//	if len(rec.Named("secret.read.failed")) != 1 { t.Fatal("expected one failure event") }
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

var activeRecorder atomic.Pointer[Recorder]

// StartRecording installs a new Recorder, replacing any active one.
func StartRecording() *Recorder {
	r := &Recorder{}
	activeRecorder.Store(r)
	return r
}

// Stop uninstalls r if it is still the active recorder.
func (r *Recorder) Stop() {
	activeRecorder.CompareAndSwap(r, nil)
}

func (r *Recorder) record(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

// Events returns every recorded event, oldest first.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// Named returns the recorded events with the given name, oldest first.
func (r *Recorder) Named(name string) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, e := range r.events {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// Reset discards everything recorded so far.
func (r *Recorder) Reset() {
	r.mu.Lock()
	r.events = nil
	r.mu.Unlock()
}
