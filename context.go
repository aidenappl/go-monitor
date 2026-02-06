package monitor

import "context"

// Context keys for storing IDs.
type ctxKey int

const (
	ctxKeyJobID ctxKey = iota
	ctxKeyRequestID
	ctxKeyTraceID
	ctxKeyUserID
)

// WithJobID returns a new context with the given job ID.
func WithJobID(ctx context.Context, jobID string) context.Context {
	return context.WithValue(ctx, ctxKeyJobID, jobID)
}

// WithRequestID returns a new context with the given request ID.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, requestID)
}

// WithTraceID returns a new context with the given trace ID.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, ctxKeyTraceID, traceID)
}

// JobID returns the job ID from the context, or empty string if not set.
func JobID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyJobID).(string); ok {
		return v
	}
	return ""
}

// RequestID returns the request ID from the context, or empty string if not set.
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// TraceID returns the trace ID from the context, or empty string if not set.
func TraceID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyTraceID).(string); ok {
		return v
	}
	return ""
}

// WithUserID returns a new context with the given user ID.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxKeyUserID, userID)
}

// UserID returns the user ID from the context, or empty string if not set.
func UserID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyUserID).(string); ok {
		return v
	}
	return ""
}
