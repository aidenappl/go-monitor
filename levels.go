package monitor

import "context"

// Log level constants.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
	LevelFatal = "fatal"
)

// Debug emits a debug-level event. Only emits if Config.Debug is true.
func Debug(ctx context.Context, name string, data any) {
	cfg := globalConfig.Load()
	if cfg == nil || !cfg.Debug {
		return
	}
	emitWithCallerDepth(ctx, name, data, LevelDebug, 2)
}

// Info emits an info-level event.
func Info(ctx context.Context, name string, data any) {
	emitWithCallerDepth(ctx, name, data, LevelInfo, 2)
}

// Warn emits a warn-level event.
func Warn(ctx context.Context, name string, data any) {
	emitWithCallerDepth(ctx, name, data, LevelWarn, 2)
}

// Error emits an error-level event.
func Error(ctx context.Context, name string, data any) {
	emitWithCallerDepth(ctx, name, data, LevelError, 2)
}

// Fatal emits a fatal-level event and then synchronously flushes the shipper
// buffer so the event is not lost if the process exits immediately after.
//
// Fatal does NOT call os.Exit — process control is left to the caller. Because
// it blocks on Flush (which ships any buffered batch over HTTP, with retries),
// Fatal can take longer to return than the other level helpers.
func Fatal(ctx context.Context, name string, data any) {
	emitWithCallerDepth(ctx, name, data, LevelFatal, 2)
	Flush()
}
