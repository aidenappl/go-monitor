package monitor

import "net/http"

const (
	// HeaderRequestID is the HTTP header for request ID.
	HeaderRequestID = "X-Request-Id"

	// HeaderTraceID is the HTTP header for trace ID.
	HeaderTraceID = "X-Trace-Id"
)

// Middleware is an HTTP middleware that ensures request_id and trace_id
// exist on every request. It reads IDs from incoming headers if present,
// otherwise generates new ones. The IDs are stored in the request context
// and also set as response headers for debugging.
//
// Compatible with gorilla/mux and any standard net/http router.
//
// Usage:
//
//	r := mux.NewRouter()
//	r.Use(monitor.Middleware)
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// Get or generate request ID
		requestID := r.Header.Get(HeaderRequestID)
		if requestID == "" {
			requestID = generateShortID()
		}
		ctx = WithRequestID(ctx, requestID)

		// Get or generate trace ID
		traceID := r.Header.Get(HeaderTraceID)
		if traceID == "" {
			traceID = generateID()
		}
		ctx = WithTraceID(ctx, traceID)

		// Get job ID from context or use global
		jobID := JobID(ctx)
		if jobID == "" {
			if cfg := globalConfig.Load(); cfg != nil {
				jobID = cfg.JobID
			}
		}
		if jobID != "" {
			ctx = WithJobID(ctx, jobID)
		}

		// Set response headers for debugging
		w.Header().Set(HeaderRequestID, requestID)
		w.Header().Set(HeaderTraceID, traceID)

		// Continue with the updated context
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
