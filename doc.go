// Package monitor provides structured event monitoring for Go services.
//
// Every event carries service, timestamp, name and level, plus the job_id,
// request_id, trace_id and user_id found in its context.
//
// Basic usage:
//
//	monitor.Init(monitor.Config{
//	    Service:   "my-service",
//	    IngestURL: "https://appleby-monitor-api.appleby.cloud/v1/events",
//	    APIKey:    os.Getenv("MONITOR_API_KEY"),
//	    SpoolDir:  "/var/lib/my-service/monitor-spool", // survive Monitor outages
//	})
//	defer monitor.Shutdown()
//
//	monitor.Emit(ctx, "user.create.success", map[string]any{"id": 123})
//
// Init never fails on the network and never blocks, so a service can start
// before the Monitor it reports to is reachable. Events are sanitized and
// redacted before they are printed, spooled or sent.
//
// With HTTP middleware (gorilla/mux compatible):
//
//	r := mux.NewRouter()
//	r.Use(monitor.MiddlewareWithConfig(monitor.MiddlewareConfig{}))
//
// Middleware only propagates X-Request-Id and X-Trace-Id; MiddlewareWithConfig
// also emits one http.request event per request.
package monitor
