package monitor

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withStderrBuffer redirects the SDK's diagnostics to a buffer for the duration
// of the test and restores the original writer afterwards.
func withStderrBuffer(t *testing.T) *bytes.Buffer {
	t.Helper()
	orig := stderrWriter
	buf := &bytes.Buffer{}
	stderrWriter = buf
	t.Cleanup(func() { stderrWriter = orig })
	return buf
}

// healthServer serves a /health payload and records how many times it was hit.
func healthServer(t *testing.T, status int, body string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if r.URL.Path != "/health" {
			// The whole point of deriving from the origin: a request landing
			// anywhere else means the SDK concatenated instead of stripping.
			t.Errorf("health probe requested %q, want /health", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHealthURLFromIngestURL(t *testing.T) {
	tests := []struct {
		name      string
		ingestURL string
		want      string
		wantErr   bool
	}{
		{"trailblaze ingest url", "https://api.monitor.appleby.cloud/v1/events", "https://api.monitor.appleby.cloud/health", false},
		{"appleby ingest url", "https://appleby-monitor-api.appleby.cloud/v1/events", "https://appleby-monitor-api.appleby.cloud/health", false},
		{"local with port", "http://localhost:8000/v1/events", "http://localhost:8000/health", false},
		{"query and fragment stripped", "https://host/v1/events?a=1#f", "https://host/health", false},
		{"origin only", "https://host", "https://host/health", false},
		{"trailing slash", "https://host/", "https://host/health", false},
		{"empty", "", "", true},
		{"no scheme or host", "/v1/events", "", true},
		{"unparseable", "://nope", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := healthURLFromIngestURL(tt.ingestURL)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("healthURLFromIngestURL(%q) = %q, want error", tt.ingestURL, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("healthURLFromIngestURL(%q) error = %v", tt.ingestURL, err)
			}
			if got != tt.want {
				t.Errorf("healthURLFromIngestURL(%q) = %q, want %q", tt.ingestURL, got, tt.want)
			}
			// The failure this guards: appending to the ingest URL yields
			// /v1/events/health, a 404 that silently disables the assertion.
			if strings.Contains(got, "/v1/events") {
				t.Errorf("health URL %q still contains the ingest path", got)
			}
		})
	}
}

func TestVerifyZone(t *testing.T) {
	tests := []struct {
		name        string
		expectZone  string
		status      int
		body        string
		wantOutcome zoneVerifyOutcome
		wantLog     []string
		notWantLog  []string
	}{
		{
			name:        "zone matches",
			expectZone:  "trailblaze",
			status:      http.StatusOK,
			body:        `{"status":"ok","role":"both","zone":"trailblaze"}`,
			wantOutcome: zoneVerifyMatched,
			notWantLog:  []string{"MISMATCH", "could not verify"},
		},
		{
			name:        "zone mismatches",
			expectZone:  "appleby",
			status:      http.StatusOK,
			body:        `{"status":"ok","role":"zone","zone":"trailblaze"}`,
			wantOutcome: zoneVerifyMismatched,
			wantLog:     []string{"ZONE MISMATCH", `"appleby"`, `"trailblaze"`, "WRONG ZONE"},
		},
		{
			name:        "control plane answers instead of the zone",
			expectZone:  "appleby",
			status:      http.StatusOK,
			body:        `{"status":"ok","role":"control","zone":"trailblaze"}`,
			wantOutcome: zoneVerifyMismatched,
			wantLog:     []string{"ZONE MISMATCH", `role "control"`},
		},
		{
			name:        "health reports no zone",
			expectZone:  "trailblaze",
			status:      http.StatusOK,
			body:        `{"status":"ok"}`,
			wantOutcome: zoneVerifyUnverified,
			wantLog:     []string{"could not verify", "did not report a zone"},
			notWantLog:  []string{"MISMATCH"},
		},
		{
			name:        "health is not JSON",
			expectZone:  "trailblaze",
			status:      http.StatusOK,
			body:        `<html>gateway</html>`,
			wantOutcome: zoneVerifyUnverified,
			wantLog:     []string{"unparseable JSON"},
			notWantLog:  []string{"MISMATCH"},
		},
		{
			name:        "health returns non-200",
			expectZone:  "trailblaze",
			status:      http.StatusServiceUnavailable,
			body:        `{"status":"degraded","zone":"trailblaze"}`,
			wantOutcome: zoneVerifyUnverified,
			wantLog:     []string{"returned status 503"},
			notWantLog:  []string{"MISMATCH"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := withStderrBuffer(t)
			srv := healthServer(t, tt.status, tt.body, nil)

			cfg := &Config{
				Service:   "zone-test",
				Zone:      tt.expectZone,
				IngestURL: srv.URL + "/v1/events",
			}

			got := verifyZone(cfg)
			if got != tt.wantOutcome {
				t.Errorf("verifyZone() = %q, want %q (log: %s)", got, tt.wantOutcome, buf.String())
			}
			for _, want := range tt.wantLog {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("log %q does not contain %q", buf.String(), want)
				}
			}
			for _, notWant := range tt.notWantLog {
				if strings.Contains(buf.String(), notWant) {
					t.Errorf("log %q should not contain %q", buf.String(), notWant)
				}
			}
		})
	}

	t.Run("health unreachable does not stop anything", func(t *testing.T) {
		buf := withStderrBuffer(t)

		// A closed listener: the probe gets a connection refused.
		dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		deadURL := dead.URL
		dead.Close()

		cfg := &Config{Service: "zone-test", Zone: "trailblaze", IngestURL: deadURL + "/v1/events"}

		if got := verifyZone(cfg); got != zoneVerifyUnreachable {
			t.Errorf("verifyZone() = %q, want %q", got, zoneVerifyUnreachable)
		}
		if !strings.Contains(buf.String(), "could not verify") {
			t.Errorf("log %q should report the unreachable probe", buf.String())
		}
		if strings.Contains(buf.String(), "MISMATCH") {
			t.Error("an unreachable health endpoint must not be reported as a mismatch")
		}
	})

	t.Run("malformed ingest url is skipped, not fatal", func(t *testing.T) {
		buf := withStderrBuffer(t)

		cfg := &Config{Service: "zone-test", Zone: "trailblaze", IngestURL: "not-a-url"}

		if got := verifyZone(cfg); got != zoneVerifyUnreachable {
			t.Errorf("verifyZone() = %q, want %q", got, zoneVerifyUnreachable)
		}
		if !strings.Contains(buf.String(), "zone check skipped") {
			t.Errorf("log %q should say the check was skipped", buf.String())
		}
	})

	t.Run("debug logs a confirmation on match", func(t *testing.T) {
		buf := withStderrBuffer(t)
		srv := healthServer(t, http.StatusOK, `{"zone":"trailblaze","role":"both"}`, nil)

		cfg := &Config{
			Service:   "zone-test",
			Zone:      "trailblaze",
			IngestURL: srv.URL + "/v1/events",
			Debug:     true,
		}

		if got := verifyZone(cfg); got != zoneVerifyMatched {
			t.Fatalf("verifyZone() = %q, want %q", got, zoneVerifyMatched)
		}
		if !strings.Contains(buf.String(), "zone verified") {
			t.Errorf("log %q should confirm the zone in debug mode", buf.String())
		}
	})
}

// TestInitZoneVerification covers the wiring: Init runs the check when (and only
// when) it is configured, and never blocks on it.
func TestInitZoneVerification(t *testing.T) {
	t.Run("runs when Zone and IngestURL are set", func(t *testing.T) {
		var hits atomic.Int32
		srv := healthServer(t, http.StatusOK, `{"zone":"trailblaze","role":"both"}`, &hits)

		results := make(chan zoneVerifyOutcome, 1)
		zoneVerifyResults = results
		t.Cleanup(func() { zoneVerifyResults = nil })

		if err := Init(Config{
			Service:       "init-zone",
			Zone:          "trailblaze",
			IngestURL:     srv.URL + "/v1/events",
			DisableStdout: true,
		}); err != nil {
			t.Fatalf("Init() error = %v", err)
		}
		t.Cleanup(Shutdown)

		select {
		case got := <-results:
			if got != zoneVerifyMatched {
				t.Errorf("outcome = %q, want %q", got, zoneVerifyMatched)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("zone verification never completed")
		}

		if n := hits.Load(); n != 1 {
			t.Errorf("health hits = %d, want exactly 1", n)
		}
	})

	t.Run("mismatch does not stop Init or the shipper", func(t *testing.T) {
		withStderrBuffer(t)
		srv := healthServer(t, http.StatusOK, `{"zone":"appleby","role":"zone"}`, nil)

		results := make(chan zoneVerifyOutcome, 1)
		zoneVerifyResults = results
		t.Cleanup(func() { zoneVerifyResults = nil })

		if err := Init(Config{
			Service:       "init-zone-mismatch",
			Zone:          "trailblaze",
			IngestURL:     srv.URL + "/v1/events",
			DisableStdout: true,
		}); err != nil {
			t.Fatalf("Init() error = %v, want nil — a wrong zone is a diagnostic, not a boot failure", err)
		}
		t.Cleanup(Shutdown)

		select {
		case got := <-results:
			if got != zoneVerifyMismatched {
				t.Errorf("outcome = %q, want %q", got, zoneVerifyMismatched)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("zone verification never completed")
		}

		if globalShipper.Load() == nil {
			t.Error("shipper should still be running after a zone mismatch")
		}
	})

	t.Run("skipped when DisableZoneVerify is set", func(t *testing.T) {
		var hits atomic.Int32
		srv := healthServer(t, http.StatusOK, `{"zone":"trailblaze","role":"both"}`, &hits)

		if err := Init(Config{
			Service:           "init-zone-off",
			Zone:              "trailblaze",
			IngestURL:         srv.URL + "/v1/events",
			DisableZoneVerify: true,
			DisableStdout:     true,
		}); err != nil {
			t.Fatalf("Init() error = %v", err)
		}
		t.Cleanup(Shutdown)

		if n := hits.Load(); n != 0 {
			t.Errorf("health hits = %d, want 0 when verification is disabled", n)
		}
	})

	t.Run("skipped when Zone is unset", func(t *testing.T) {
		var hits atomic.Int32
		srv := healthServer(t, http.StatusOK, `{"zone":"trailblaze","role":"both"}`, &hits)

		if err := Init(Config{
			Service:       "init-no-zone",
			IngestURL:     srv.URL + "/v1/events",
			DisableStdout: true,
		}); err != nil {
			t.Fatalf("Init() error = %v", err)
		}
		t.Cleanup(Shutdown)

		if n := hits.Load(); n != 0 {
			t.Errorf("health hits = %d, want 0 when Zone is unset", n)
		}
	})
}
