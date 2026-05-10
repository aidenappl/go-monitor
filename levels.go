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

// Fatal emits a fatal-level event.
func Fatal(ctx context.Context, name string, data any) {
	emitWithCallerDepth(ctx, name, data, LevelFatal, 2)
}
