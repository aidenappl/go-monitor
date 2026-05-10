package monitor

import (
	"context"
	"net/http"
	"time"
)

// WrapHTTPClient returns a new http.Client that wraps the given client's transport
// to automatically emit "http.client_request" events for every outbound request.
// If client is nil, http.DefaultClient is used as the base.
func WrapHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	return &http.Client{
		Transport:     WrapTransport(client.Transport),
		CheckRedirect: client.CheckRedirect,
		Jar:           client.Jar,
		Timeout:       client.Timeout,
	}
}

// WrapTransport wraps an http.RoundTripper to emit monitoring events for each request.
// If rt is nil, http.DefaultTransport is used.
func WrapTransport(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &monitorTransport{base: rt}
}

type monitorTransport struct {
	base http.RoundTripper
}

func (t *monitorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	start := time.Now()

	// Propagate trace_id and request_id into outbound headers
	if traceID := TraceID(ctx); traceID != "" {
		req.Header.Set(HeaderTraceID, traceID)
	}
	if requestID := RequestID(ctx); requestID != "" {
		req.Header.Set(HeaderRequestID, requestID)
	}

	resp, err := t.base.RoundTrip(req)
	duration := time.Since(start)

	data := map[string]any{
		"request_method": req.Method,
		"request_url":    req.URL.String(),
		"duration_ms":    duration.Milliseconds(),
	}

	level := LevelInfo

	if err != nil {
		data["error"] = err.Error()
		level = LevelError
	} else {
		data["response_status"] = resp.StatusCode
		if resp.StatusCode >= 500 {
			level = LevelError
		}
	}

	emitInternal(ctx, "http.client_request", data, level)

	return resp, err
}

// emitInternal emits an event without source location capture, used by
// internal SDK components where caller location is not meaningful.
func emitInternal(ctx context.Context, name string, data any, level string) {
	cfg := globalConfig.Load()
	if cfg == nil {
		return
	}

	event := newEvent(ctx, name, data, level)
	dispatchEvent(event)
}
