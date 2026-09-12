package monitor

import (
	"context"
	"fmt"
	"runtime/debug"
)

// Go runs fn on a new goroutine and reports a panic instead of letting it crash
// the process.
//
// A panic in a goroutine cannot be recovered by anything outside that goroutine
// — not HTTP middleware, not main. It kills the whole process with a stack trace
// on stderr, and every event still buffered in memory goes with it. Any
// goroutine a service starts that is not trivially safe should go through here,
// or defer RecoverAndReport itself.
//
// The goroutine then simply returns. If the work must continue — a ticker loop,
// a WebSocket pump — restarting it is the caller's decision.
func Go(ctx context.Context, name string, fn func(ctx context.Context)) {
	go func() {
		defer RecoverAndReport(ctx, name)
		fn(ctx)
	}()
}

// RecoverAndReport recovers a panic in the calling goroutine and reports it as a
// panic.recovered event. It must be deferred directly —
//
//	defer monitor.RecoverAndReport(ctx, "snapshot-scheduler")
//
// — because recover only stops a panic when it is called by the deferred
// function itself; wrapping this in another closure turns it into a no-op.
func RecoverAndReport(ctx context.Context, name string) {
	if rec := recover(); rec != nil {
		ReportPanic(ctx, name, rec)
	}
}

// ReportPanic emits a panic.recovered event for a value the caller has already
// recovered — for code with its own recover that decides what happens next,
// such as restarting a loop, or exiting so a supervisor restarts the process.
//
// If the process is about to exit, call Shutdown after this: it delivers the
// event (or, with a spool, persists it) before returning.
func ReportPanic(ctx context.Context, name string, recovered any) {
	if ctx == nil {
		ctx = context.Background()
	}
	emitInternal(ctx, "panic.recovered", map[string]any{
		"goroutine":  name,
		"error":      fmt.Sprint(recovered),
		"panic_type": fmt.Sprintf("%T", recovered),
		"stacktrace": string(debug.Stack()),
	}, LevelError)
}
