package monitor

import (
	"context"
	"reflect"
	"runtime"
)

// CaptureError emits an error-level event named "error.captured" with error details,
// stack trace, and optional additional data.
//
// Prefer CaptureErrorAs: every CaptureError in a service shares one event name,
// so Monitor can tell them apart by message alone.
func CaptureError(ctx context.Context, err error, data ...map[string]any) {
	captureError(ctx, "error.captured", err, data)
}

// CaptureErrorAs is CaptureError with an event name chosen by the caller.
//
// Monitor groups errors into issues by service, event name, path and message.
// A name that says what failed — "deploy.rollout.failed",
// "secret.decrypt.failed" — groups by operation and reads correctly on every
// dashboard; "error.captured" groups by whatever the message happens to say.
func CaptureErrorAs(ctx context.Context, name string, err error, data ...map[string]any) {
	captureError(ctx, name, err, data)
}

func captureError(ctx context.Context, name string, err error, data []map[string]any) {
	if err == nil {
		return
	}

	// Capture stack trace
	buf := make([]byte, 4096)
	n := runtime.Stack(buf, false)
	stackTrace := string(buf[:n])

	eventData := map[string]any{
		"error":       err.Error(),
		"error_type":  reflect.TypeOf(err).String(),
		"stack_trace": stackTrace,
	}

	// Merge additional data
	for _, d := range data {
		for k, v := range d {
			eventData[k] = v
		}
	}

	// Depth 3: captureError → CaptureError/CaptureErrorAs → the caller.
	emitWithCallerDepth(ctx, name, eventData, LevelError, 3)
}
