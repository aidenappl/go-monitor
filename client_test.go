package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWrapHTTPClient(t *testing.T) {
	if err := Init(Config{Service: "test-client", DisableStdout: true}); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	t.Run("wraps nil client", func(t *testing.T) {
		client := WrapHTTPClient(nil)
		if client == nil {
			t.Fatal("WrapHTTPClient(nil) should return a non-nil client")
		}
	})

	t.Run("wraps existing client", func(t *testing.T) {
		original := &http.Client{Timeout: 5000}
		wrapped := WrapHTTPClient(original)
		if wrapped.Timeout != original.Timeout {
			t.Errorf("wrapped client timeout = %v, want %v", wrapped.Timeout, original.Timeout)
		}
	})

	t.Run("emits event on request", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		client := WrapHTTPClient(&http.Client{})
		req, _ := http.NewRequestWithContext(context.Background(), "GET", server.URL+"/test", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do() error = %v", err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("propagates trace and request IDs", func(t *testing.T) {
		var gotTraceID, gotRequestID string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotTraceID = r.Header.Get(HeaderTraceID)
			gotRequestID = r.Header.Get(HeaderRequestID)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		ctx := WithTraceID(context.Background(), "trace-abc")
		ctx = WithRequestID(ctx, "req-xyz")

		client := WrapHTTPClient(&http.Client{})
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/test", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("client.Do() error = %v", err)
		}
		resp.Body.Close()

		if gotTraceID != "trace-abc" {
			t.Errorf("trace ID = %v, want trace-abc", gotTraceID)
		}
		if gotRequestID != "req-xyz" {
			t.Errorf("request ID = %v, want req-xyz", gotRequestID)
		}
	})
}

func TestWrapTransport(t *testing.T) {
	t.Run("wraps nil transport", func(t *testing.T) {
		rt := WrapTransport(nil)
		if rt == nil {
			t.Fatal("WrapTransport(nil) should return a non-nil transport")
		}
	})
}
