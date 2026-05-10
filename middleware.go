package monitor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
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
	requestID := r.Header.Get(HeaderRequestID)
	if requestID == "" {
		requestID = generateShortID()
	}
	ctx = WithRequestID(ctx, requestID)

	traceID := r.Header.Get(HeaderTraceID)
	if traceID == "" {
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
	CaptureRequestBody bool

	// CaptureResponseBody enables capturing the response body in events.
	CaptureResponseBody bool

	// MaxBodySize is the maximum bytes to capture for request/response bodies.
	// Default: 4096.
	MaxBodySize int

	// SkipPaths is a list of paths to skip monitoring (e.g., "/healthcheck").
	SkipPaths []string
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

			next.ServeHTTP(rw, r.WithContext(ctx))

			duration := time.Since(start)

			// Build event data
			data := map[string]any{
				"request_method":        r.Method,
				"request_path":          r.URL.Path,
				"request_query":         r.URL.RawQuery,
				"response_status":       rw.statusCode,
				"duration_ms":           duration.Milliseconds(),
				"response_content_type": rw.Header().Get("Content-Type"),
				"request_headers": map[string]string{
					"Content-Type": r.Header.Get("Content-Type"),
					"User-Agent":   r.Header.Get("User-Agent"),
				},
			}

			if cfg.CaptureRequestBody && reqBody != "" {
				data["request_body"] = reqBody
			}
			if cfg.CaptureResponseBody && rw.body.Len() > 0 {
				data["response_body"] = rw.body.String()
			}

			level := LevelInfo
			if rw.statusCode >= 500 {
				level = LevelError
			}

			emitInternal(ctx, "http.request", data, level)
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
