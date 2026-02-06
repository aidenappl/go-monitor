// Package monitor provides structured event monitoring with guaranteed fields.
//
// Every event emitted contains: job_id, request_id, trace_id, service, timestamp.
//
// Basic usage:
//
//	monitor.Init(monitor.Config{
//	    Service: "my-service",
//	    Env:     "prod",
//	})
//
//	monitor.Emit(ctx, "user.created", map[string]any{"id": 123})
//
// With HTTP middleware (gorilla/mux compatible):
//
//	r := mux.NewRouter()
//	r.Use(monitor.Middleware)
//
// The middleware ensures request_id and trace_id are present on every request,
// reading from X-Request-Id and X-Trace-Id headers if present, or generating new ones.
package monitor
