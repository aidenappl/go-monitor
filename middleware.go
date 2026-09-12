package monitor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"time"
)

const (
	// HeaderRequestID is the HTTP header for request ID.
	HeaderRequestID = "X-Request-Id"

	// HeaderTraceID is the HTTP header for trace ID.
	HeaderTraceID = "X-Trace-Id"
)

// propagateIDs extracts or generates request_id, trace_id, and job_id,
// stores them in the context, and sets response headers for debugging.
func propagateIDs(ctx context.Context, r *http.Request, w http.ResponseWriter) context.Context {
	// Inbound ids are caller-controlled. One monitor-core would reject is
	// replaced, not propagated: it would be cleared from every event anyway,
	// leaving the request with no id at all instead of a fresh one.
	requestID := r.Header.Get(HeaderRequestID)
	if requestID == "" || !ValidCorrelationID(requestID) {
		requestID = generateShortID()
	}
	ctx = WithRequestID(ctx, requestID)

	traceID := r.Header.Get(HeaderTraceID)
	if traceID == "" || !ValidCorrelationID(traceID) {
		traceID = generateID()
	}
	ctx = WithTraceID(ctx, traceID)

	jobID := JobID(ctx)
	if jobID == "" {
		if cfg := globalConfig.Load(); cfg != nil {
			jobID = cfg.JobID
		}
	}
	if jobID != "" {
		ctx = WithJobID(ctx, jobID)
	}

	w.Header().Set(HeaderRequestID, requestID)
	w.Header().Set(HeaderTraceID, traceID)

	return ctx
}

// Middleware is an HTTP middleware that ensures request_id and trace_id
// exist on every request. It reads IDs from incoming headers if present,
// otherwise generates new ones. The IDs are stored in the request context
// and also set as response headers for debugging.
//
// It emits no events of its own: pair it with MiddlewareWithConfig, or emit
// from the handler, to get a per-request event.
//
// Compatible with gorilla/mux and any standard net/http router.
//
// Usage:
//
//	r := mux.NewRouter()
//	r.Use(monitor.Middleware)
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := propagateIDs(r.Context(), r, w)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// MiddlewareConfig configures the enhanced HTTP middleware.
type MiddlewareConfig struct {
	// CaptureRequestBody enables capturing the request body in events.
	//
	// Bodies pass through redaction, but redaction recognizes credentials by
	// key and by shape — it cannot know that a free-text field is sensitive.
	// Leave this off on any endpoint that accepts secrets or personal data.
	CaptureRequestBody bool

	// CaptureResponseBody enables capturing the response body in events.
	// The same caution applies.
	CaptureResponseBody bool

	// MaxBodySize is the maximum bytes to capture for request/response bodies.
	// Default: 4096.
	MaxBodySize int

	// SkipPaths is a list of paths to skip monitoring (e.g., "/healthcheck").
	SkipPaths []string

	// CaptureQuery includes the raw query string as request_query. Default:
	// false. Query strings are where OAuth codes, tokens and emails travel, and
	// anything captured is retained for the life of the event store. When
	// enabled, credential-shaped parameters are still redacted.
	CaptureQuery bool

	// RouteTemplate returns the matched route pattern for a request, e.g.
	// "/stacks/{id}". When it returns a non-empty string, that becomes the
	// event's path — the field Monitor groups issues by — and the concrete
	// path is kept as request_path. Without it, every id in a URL splits one
	// failing endpoint into an issue per id.
	//
	// With gorilla/mux (register this middleware with Router.Use so the route
	// is matched before it runs):
	//
	//	RouteTemplate: func(r *http.Request) string {
	//		if rt := mux.CurrentRoute(r); rt != nil {
	//			if t, err := rt.GetPathTemplate(); err == nil {
	//				return t
	//			}
	//		}
	//		return ""
	//	},
	RouteTemplate func(r *http.Request) string
}

// MiddlewareWithConfig returns an HTTP middleware that captures detailed
// request/response information and emits "http.request" events.
// It also performs the same ID propagation as the basic Middleware.
func MiddlewareWithConfig(cfg MiddlewareConfig) func(http.Handler) http.Handler {
	if cfg.MaxBodySize <= 0 {
		cfg.MaxBodySize = 4096
	}

	skipSet := make(map[string]bool, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skipSet[p] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := propagateIDs(r.Context(), r, w)

			// Check skip paths
			if skipSet[r.URL.Path] {
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			start := time.Now()

			// Optionally capture request body
			var reqBody string
			if cfg.CaptureRequestBody && r.Body != nil {
				bodyBytes, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				if len(bodyBytes) > cfg.MaxBodySize {
					reqBody = string(bodyBytes[:cfg.MaxBodySize])
				} else {
					reqBody = string(bodyBytes)
				}
			}

			// Wrap response writer to capture status and optionally body
			rw := &captureResponseWriter{
				ResponseWriter: w,
				statusCode:     http.StatusOK,
				captureBody:    cfg.CaptureResponseBody,
				maxBodySize:    cfg.MaxBodySize,
			}

			// The event is emitted from a deferred function so that duration and
			// status are always captured — including when the handler panics.
			// On panic we emit a fatal event with the panic value + stack and
			// then re-panic so upstream behavior (e.g. the server's own recovery)
			// is preserved.
			defer func() {
				rec := recover()

				duration := time.Since(start)

				status := rw.statusCode
				if rec != nil && !rw.wroteHeader {
					// A panicking handler that never wrote a status is a 500.
					status = http.StatusInternalServerError
				}

				// path is what Monitor groups issues by. The route template
				// ("/stacks/{id}") keeps one failing endpoint one issue; the raw
				// path would split it per id and turn the ids into a dimension.
				path := r.URL.Path
				route := ""
				if cfg.RouteTemplate != nil {
					if route = cfg.RouteTemplate(r); route != "" {
						path = route
					}
				}

				data := map[string]any{
					"method":                r.Method,
					"path":                  path,
					"status_code":           status,
					"duration_ms":           duration.Milliseconds(),
					"request_method":        r.Method,
					"request_path":          r.URL.Path,
					"response_status":       status,
					"response_content_type": rw.Header().Get("Content-Type"),
					"request_headers": map[string]string{
						"Content-Type": r.Header.Get("Content-Type"),
						"User-Agent":   r.Header.Get("User-Agent"),
					},
				}
				if route != "" {
					data["route"] = route
				}
				if cfg.CaptureQuery && r.URL.RawQuery != "" {
					data["request_query"] = r.URL.RawQuery
				}
				if cfg.CaptureRequestBody && reqBody != "" {
					data["request_body"] = reqBody
				}
				if cfg.CaptureResponseBody && rw.body.Len() > 0 {
					data["response_body"] = rw.body.String()
				}

				level := LevelInfo
				switch {
				case status >= 500:
					level = LevelError
				case status >= 400:
					level = LevelWarn
				}

				if rec != nil {
					msg := fmt.Sprintf("%v", rec)
					data["panic"] = msg
					// error is what Monitor's fingerprint reads: it makes each
					// distinct panic its own issue instead of one "500" bucket.
					data["error"] = msg
					data["stack"] = string(debug.Stack())
					level = LevelError
				}

				emitInternal(ctx, "http.request", data, level)

				// Preserve upstream behavior by re-panicking.
				if rec != nil {
					panic(rec)
				}
			}()

			next.ServeHTTP(rw, r.WithContext(ctx))
		})
	}
}

// captureResponseWriter wraps http.ResponseWriter to capture the status code
// and optionally the response body.
type captureResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	captureBody bool
	maxBodySize int
	body        bytes.Buffer
}

func (w *captureResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.statusCode = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	if w.captureBody && w.body.Len() < w.maxBodySize {
		remaining := w.maxBodySize - w.body.Len()
		if len(b) <= remaining {
			w.body.Write(b)
		} else {
			w.body.Write(b[:remaining])
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *captureResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *captureResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}
