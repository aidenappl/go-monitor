package monitor

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiddlewareWithConfig(t *testing.T) {
	if err := Init(Config{Service: "test-mw", DisableStdout: true, JobID: "mw-job"}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	t.Run("basic request capture", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})

		mw := MiddlewareWithConfig(MiddlewareConfig{})
		wrapped := mw(handler)

		req := httptest.NewRequest("GET", "/test?foo=bar", nil)
		req.Header.Set("User-Agent", "test-agent")
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("sets ID headers", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		mw := MiddlewareWithConfig(MiddlewareConfig{})
		wrapped := mw(handler)

		req := httptest.NewRequest("GET", "/test", nil)
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)

		if rec.Header().Get(HeaderRequestID) == "" {
			t.Error("should set X-Request-Id header")
		}
		if rec.Header().Get(HeaderTraceID) == "" {
			t.Error("should set X-Trace-Id header")
		}
	})

	t.Run("skip paths", func(t *testing.T) {
		var handlerCalled bool
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			w.WriteHeader(http.StatusOK)
		})

		mw := MiddlewareWithConfig(MiddlewareConfig{
			SkipPaths: []string{"/healthcheck", "/ready"},
		})
		wrapped := mw(handler)

		req := httptest.NewRequest("GET", "/healthcheck", nil)
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)

		if !handlerCalled {
			t.Error("handler should still be called for skipped paths")
		}
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("capture request body", func(t *testing.T) {
		var receivedBody string
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buf := make([]byte, 1024)
			n, _ := r.Body.Read(buf)
			receivedBody = string(buf[:n])
			w.WriteHeader(http.StatusOK)
		})

		mw := MiddlewareWithConfig(MiddlewareConfig{
			CaptureRequestBody: true,
		})
		wrapped := mw(handler)

		body := `{"name":"test"}`
		req := httptest.NewRequest("POST", "/test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)

		// The handler should still receive the body
		if receivedBody != body {
			t.Errorf("handler received body = %v, want %v", receivedBody, body)
		}
	})

	t.Run("capture response body", func(t *testing.T) {
		responseBody := `{"result":"success"}`
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(responseBody))
		})

		mw := MiddlewareWithConfig(MiddlewareConfig{
			CaptureResponseBody: true,
		})
		wrapped := mw(handler)

		req := httptest.NewRequest("GET", "/test", nil)
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)

		// The actual response should still be written
		if rec.Body.String() != responseBody {
			t.Errorf("response body = %v, want %v", rec.Body.String(), responseBody)
		}
	})

	t.Run("default MaxBodySize", func(t *testing.T) {
		cfg := MiddlewareConfig{}
		mw := MiddlewareWithConfig(cfg)
		if mw == nil {
			t.Fatal("middleware should not be nil")
		}
	})

	t.Run("500 status triggers error level", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})

		mw := MiddlewareWithConfig(MiddlewareConfig{})
		wrapped := mw(handler)

		req := httptest.NewRequest("GET", "/fail", nil)
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("preserves existing request IDs", func(t *testing.T) {
		var gotRequestID, gotTraceID string
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotRequestID = RequestID(r.Context())
			gotTraceID = TraceID(r.Context())
			w.WriteHeader(http.StatusOK)
		})

		mw := MiddlewareWithConfig(MiddlewareConfig{})
		wrapped := mw(handler)

		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set(HeaderRequestID, "existing-req")
		req.Header.Set(HeaderTraceID, "existing-trace")
		rec := httptest.NewRecorder()

		wrapped.ServeHTTP(rec, req)

		if gotRequestID != "existing-req" {
			t.Errorf("request ID = %v, want existing-req", gotRequestID)
		}
		if gotTraceID != "existing-trace" {
			t.Errorf("trace ID = %v, want existing-trace", gotTraceID)
		}
	})
}

func TestCaptureResponseWriter(t *testing.T) {
	t.Run("captures status code", func(t *testing.T) {
		rec := httptest.NewRecorder()
		crw := &captureResponseWriter{
			ResponseWriter: rec,
			statusCode:     http.StatusOK,
		}

		crw.WriteHeader(http.StatusNotFound)
		if crw.statusCode != http.StatusNotFound {
			t.Errorf("statusCode = %d, want 404", crw.statusCode)
		}
	})

	t.Run("only captures first WriteHeader", func(t *testing.T) {
		rec := httptest.NewRecorder()
		crw := &captureResponseWriter{
			ResponseWriter: rec,
			statusCode:     http.StatusOK,
		}

		crw.WriteHeader(http.StatusNotFound)
		crw.WriteHeader(http.StatusInternalServerError) // should be ignored

		if crw.statusCode != http.StatusNotFound {
			t.Errorf("statusCode = %d, want 404 (first call)", crw.statusCode)
		}
	})

	t.Run("captures response body up to max", func(t *testing.T) {
		rec := httptest.NewRecorder()
		crw := &captureResponseWriter{
			ResponseWriter: rec,
			statusCode:     http.StatusOK,
			captureBody:    true,
			maxBodySize:    10,
		}

		_, _ = crw.Write([]byte("hello world this is a long body"))

		if crw.body.Len() != 10 {
			t.Errorf("body len = %d, want 10", crw.body.Len())
		}
		if crw.body.String() != "hello worl" {
			t.Errorf("body = %v, want 'hello worl'", crw.body.String())
		}
	})
}
