package monitor

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWrapTransportLeavesTheQueryOut(t *testing.T) {
	rec := recording(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()

	resp, err := WrapHTTPClient(nil).Get(srv.URL + "/v2/_catalog?api_key=sekret&n=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	evs := rec.Named("http.client_request")
	if len(evs) == 0 {
		t.Fatal("no http.client_request event")
	}
	d := evs[len(evs)-1].Data.(map[string]any)
	if u := d["request_url"].(string); strings.Contains(u, "sekret") || strings.Contains(u, "?") || !strings.HasSuffix(u, "/v2/_catalog") {
		t.Errorf("request_url = %q, want the URL without its query", u)
	}
	if d["request_host"] == "" {
		t.Error("request_host should identify the upstream")
	}
}
