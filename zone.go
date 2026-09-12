package monitor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// zoneVerifyTimeout bounds the whole /health round trip. The check runs off the
// caller's goroutine anyway, but a deadline of its own means a hung monitor
// cannot leave a goroutine (and its socket) parked for the life of the process.
const zoneVerifyTimeout = 3 * time.Second

// zoneVerifyBodyLimit caps how much of /health is read. The endpoint is
// unauthenticated, so whatever answers on that origin is not necessarily the
// monitor-core we meant — an unbounded ReadAll would hand an arbitrary host a
// way to balloon a consumer's memory at startup.
const zoneVerifyBodyLimit = 64 << 10

// zoneVerifyOutcome is what the /health probe concluded.
//
// The vocabulary deliberately mirrors monitor-core's ZoneReachability, and the
// distinction that earns it is unverified vs mismatched: a 200 proves *a*
// monitor-core is listening, not that it is THIS zone's monitor-core. Collapsing
// the two would either shout at every consumer pointed at an older build, or
// stay quiet about the one case that actually corrupts data.
type zoneVerifyOutcome string

const (
	// zoneVerifyMatched — the origin identified itself as the expected zone.
	zoneVerifyMatched zoneVerifyOutcome = "matched"

	// zoneVerifyMismatched — it answered and named a DIFFERENT zone. Every event
	// this process ships is being filed under another tenant.
	zoneVerifyMismatched zoneVerifyOutcome = "mismatched"

	// zoneVerifyUnverified — something answered, but would not say which zone it
	// is (an older monitor-core, a proxy answering on its behalf, or an unrelated
	// service returning 200). Not evidence of a problem, and not evidence of
	// correctness either.
	zoneVerifyUnverified zoneVerifyOutcome = "unverified"

	// zoneVerifyUnreachable — the probe got no usable answer at all, or the
	// ingest URL could not be turned into a health URL.
	zoneVerifyUnreachable zoneVerifyOutcome = "unreachable"
)

// zoneVerifyResults, when non-nil, receives the outcome of every check started
// by startZoneVerification. Nil in production; tests set it so they can join the
// goroutine Init starts instead of sleeping and hoping.
var zoneVerifyResults chan zoneVerifyOutcome

// healthURLFromIngestURL derives {scheme}://{host}/health from an ingest URL.
//
// IngestURL is the FULL endpoint (".../v1/events") — the SDK appends no path to
// it — so the health URL has to be rebuilt from the origin. Concatenating
// instead would request "/v1/events/health", collect a 404, and quietly downgrade
// the assertion to a permanent "unverified" that no one would ever action.
func healthURLFromIngestURL(ingestURL string) (string, error) {
	u, err := url.Parse(ingestURL)
	if err != nil {
		return "", fmt.Errorf("ingest URL %q is not a URL: %w", ingestURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("ingest URL %q has no scheme or host", ingestURL)
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/health"}).String(), nil
}

// startZoneVerification runs the zone assertion on its own goroutine.
//
// Init must return promptly no matter what: this is a diagnostic, never a gate.
// A service whose startup could be delayed — or worse, failed — by an
// unreachable monitor would have traded silent misrouting for an outage.
func startZoneVerification(cfg *Config) {
	go func() {
		outcome := verifyZone(cfg)
		// Read the hook once, then send: this ordering is what lets a test join
		// this goroutine (its receive happens-after every write above) rather
		// than racing the package-level writers it swapped.
		if ch := zoneVerifyResults; ch != nil {
			select {
			case ch <- outcome:
			default:
			}
		}
	}()
}

// verifyZone asks the ingest origin which zone it is and reports disagreement.
//
// Config.Zone is never sent on the wire — tenancy is stamped server-side from
// the api_keys row behind the API key — so this is the only moment a service can
// find out it is pointed at the wrong zone. Everything downstream succeeds:
// events are accepted, the dashboard shows them, and they are filed under
// another tenant with nothing invalid anywhere.
func verifyZone(cfg *Config) zoneVerifyOutcome {
	healthURL, err := healthURLFromIngestURL(cfg.IngestURL)
	if err != nil {
		fmt.Fprintf(stderrWriter, "monitor: zone check skipped: %v\n", err)
		return zoneVerifyUnreachable
	}

	client := &http.Client{Timeout: zoneVerifyTimeout}
	resp, err := client.Get(healthURL)
	if err != nil {
		// A briefly unreachable health endpoint says nothing about which zone
		// this is. Log it quietly and carry on — the shipper is already running
		// and must keep running.
		fmt.Fprintf(stderrWriter, "monitor: could not verify zone %q: %s unreachable: %v\n", cfg.Zone, healthURL, err)
		return zoneVerifyUnreachable
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, zoneVerifyBodyLimit))
	if err != nil {
		fmt.Fprintf(stderrWriter, "monitor: could not verify zone %q: reading %s failed: %v\n", cfg.Zone, healthURL, err)
		return zoneVerifyUnreachable
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderrWriter, "monitor: could not verify zone %q: %s returned status %d\n", cfg.Zone, healthURL, resp.StatusCode)
		return zoneVerifyUnverified
	}

	var health struct {
		Zone string `json:"zone"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		fmt.Fprintf(stderrWriter, "monitor: could not verify zone %q: %s returned unparseable JSON: %v\n", cfg.Zone, healthURL, err)
		return zoneVerifyUnverified
	}

	if health.Zone == "" {
		// 200 with no zone identity: an older monitor-core, or something else
		// entirely answering on that origin. Saying nothing louder than this is
		// deliberate — shouting here would train everyone to ignore the banner
		// below, which is the one message that matters.
		fmt.Fprintf(stderrWriter, "monitor: could not verify zone %q: %s did not report a zone\n", cfg.Zone, healthURL)
		return zoneVerifyUnverified
	}

	if health.Zone != cfg.Zone {
		fmt.Fprintf(stderrWriter,
			"monitor: ======================== ZONE MISMATCH ========================\n"+
				"monitor: Config.Zone is %q but %s reports zone %q (role %q).\n"+
				"monitor: Service %q is shipping into the WRONG ZONE.\n"+
				"monitor: Nothing downstream will error: the server stamps the project\n"+
				"monitor: from the api_keys row behind the API key and ignores anything\n"+
				"monitor: the client claims, so misdirected events look entirely normal\n"+
				"monitor: and CANNOT be reattributed to the right zone afterwards.\n"+
				"monitor: Fix: set IngestURL to the %q zone's ingest URL AND use an API\n"+
				"monitor: key minted on that zone — a key from another zone or from the\n"+
				"monitor: control plane binds to that zone's project, silently.\n"+
				"monitor: ==============================================================\n",
			cfg.Zone, healthURL, health.Zone, health.Role, cfg.Service, cfg.Zone)
		return zoneVerifyMismatched
	}

	if cfg.Debug {
		fmt.Fprintf(stderrWriter, "monitor: zone verified: %s reports zone %q (role %q)\n", healthURL, health.Zone, health.Role)
	}
	return zoneVerifyMatched
}
