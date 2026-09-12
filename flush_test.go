package monitor

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestFlushIncludesEventsEmittedBeforeIt pins Flush's contract: everything
// emitted before the call ships before it returns. It once raced — run()'s
// select could service the flush request ahead of a buffered event — which
// made Fatal's emit-then-flush unreliable exactly when it mattered.
func TestFlushIncludesEventsEmittedBeforeIt(t *testing.T) {
	withStderrBuffer(t)
	f := newFakeIngest(t, "")
	if err := Init(Config{Service: "flush-order", IngestURL: f.URL(), BatchSize: 100, FlushEvery: time.Hour, DisableStdout: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(Shutdown)

	for i := 1; i <= 50; i++ {
		Emit(context.Background(), fmt.Sprintf("ordered.%d", i), nil)
		Flush()
		if got := Stats().Flushed; got != int64(i) {
			t.Fatalf("after emit #%d and Flush, flushed = %d: Flush returned before shipping an event emitted ahead of it", i, got)
		}
	}
}
