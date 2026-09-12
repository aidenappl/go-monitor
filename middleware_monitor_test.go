package monitor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serveThrough(cfg MiddlewareConfig, h http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	rw := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }() // the middleware re-panics by design
		MiddlewareWithConfig(cfg)(h).ServeHTTP(rw, req)
	}()
	return rw
}

func lastHTTPEvent(t *testing.T, rec *Recorder) Event {
	t.Helper()
	evs := rec.Named("http.request")
	if len(evs) == 0 {
		t.Fatal("no http.request event recorded")
	}
	return evs[len(evs)-1]
}

func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func TestMiddlewareLeavesTheQueryStringOutByDefault(t *testing.T) {
	rec := recording(t)
	serveThrough(MiddlewareConfig{}, ok, httptest.NewRequest("GET", "/oauth/logout?id_token_hint="+testJWT+"&state=abc", nil))

	b, _ := json.Marshal(lastHTTPEvent(t, rec))
	for _, leak := range []string{"id_token_hint", "eyJhbGci", "request_query"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("event %s should not contain %q", b, leak)
		}
	}
}

func TestMiddlewareCapturedQueryIsRedacted(t *testing.T) {
	rec := recording(t)
	serveThrough(MiddlewareConfig{CaptureQuery: true}, ok, httptest.NewRequest("GET", "/forta/callback?code=SplxlOBeZQQYbYS6WxSbIA&state=abc", nil))

	q, _ := lastHTTPEvent(t, rec).Data.(map[string]any)["request_query"].(string)
	if !strings.Contains(q, "code=[REDACTED]") || !strings.Contains(q, "state=abc") || strings.Contains(q, "SplxlOBe") {
		t.Errorf("request_query = %q", q)
	}
}

func TestMiddlewareGroupsByRouteTemplate(t *testing.T) {
	rec := recording(t)
	cfg := MiddlewareConfig{RouteTemplate: func(*http.Request) string { return "/stacks/{id}" }}
	serveThrough(cfg, ok, httptest.NewRequest("GET", "/stacks/42", nil))

	d := lastHTTPEvent(t, rec).Data.(map[string]any)
	if d["path"] != "/stacks/{id}" || d["route"] != "/stacks/{id}" || d["request_path"] != "/stacks/42" || d["method"] != "GET" {
		t.Errorf("data = %v", d)
	}
}

func TestMiddlewareLevels(t *testing.T) {
	for status, want := range map[int]string{200: LevelInfo, 404: LevelWarn, 429: LevelWarn, 503: LevelError} {
		rec := recording(t)
		serveThrough(MiddlewareConfig{}, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }, httptest.NewRequest("GET", "/x", nil))
		e := lastHTTPEvent(t, rec)
		if e.Level != want || e.Data.(map[string]any)["status_code"] != status {
			t.Errorf("status %d: level %q status_code %v, want %q", status, e.Level, e.Data.(map[string]any)["status_code"], want)
		}
		rec.Stop()
	}
}

func TestMiddlewarePanicBecomesAGroupableError(t *testing.T) {
	rec := recording(t)
	serveThrough(MiddlewareConfig{}, func(http.ResponseWriter, *http.Request) {
		panic("index out of range [3] with length 3")
	}, httptest.NewRequest("POST", "/deploy", nil))

	e := lastHTTPEvent(t, rec)
	d := e.Data.(map[string]any)
	if e.Level != LevelError || d["status_code"] != http.StatusInternalServerError || d["error"] != "index out of range [3] with length 3" {
		t.Errorf("level=%q data=%v", e.Level, d)
	}
}

func TestMiddlewareReplacesInvalidInboundIDs(t *testing.T) {
	var gotReq, gotTrace string
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set(HeaderRequestID, "../../etc/passwd")
	req.Header.Set(HeaderTraceID, "not-a-trace")

	rw := serveThrough(MiddlewareConfig{}, func(w http.ResponseWriter, r *http.Request) {
		gotReq, gotTrace = RequestID(r.Context()), TraceID(r.Context())
		w.WriteHeader(http.StatusOK)
	}, req)

	for name, id := range map[string]string{"request": gotReq, "trace": gotTrace, "response header": rw.Header().Get(HeaderRequestID)} {
		if id == "" || !ValidCorrelationID(id) {
			t.Errorf("%s id = %q, want a fresh valid id", name, id)
		}
	}
	if gotReq == "../../etc/passwd" || gotTrace == "not-a-trace" {
		t.Error("caller-supplied invalid ids must not propagate")
	}
}
