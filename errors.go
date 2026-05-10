package monitor

import (
	"context"
	"reflect"
	"runtime"
)

// CaptureError emits an error-level event named "error.captured" with error details,
// stack trace, and optional additional data.
func CaptureError(ctx context.Context, err error, data ...map[string]any) {
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

	emitWithCallerDepth(ctx, "error.captured", eventData, LevelError, 2)
}
