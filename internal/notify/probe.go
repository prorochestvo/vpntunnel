package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"vpntunnel/internal/tunnel"
)

// defaultProbeEndpoint is the exit-IP lookup service queried through the
// tunnel dialer. am.i.mullvad.net returns {ip, country, city,
// mullvad_exit_ip, mullvad_exit_ip_hostname}; for non-Mullvad providers the
// endpoint still returns at least ip, with country/city possibly empty.
const defaultProbeEndpoint = "https://am.i.mullvad.net/json"

// probeTimeout bounds the exit-IP probe regardless of the caller's ctx
// deadline, so a stalled endpoint never delays the async sender indefinitely.
const probeTimeout = 5 * time.Second

// probeBodyLimit caps the bytes read from the probe response so a rogue
// endpoint cannot stream an unbounded body into the decoder.
const probeBodyLimit = 64 << 10

// exitInfo is the subset of the probe endpoint's JSON response the notifier
// uses to enrich a notification.
type exitInfo struct {
	IP      string `json:"ip"`
	Country string `json:"country"`
	City    string `json:"city"`
}

// probeExitIP dials endpoint through d and decodes the exit IP/country/city.
// It is best-effort: any failure (nil dialer, dial error, non-200, decode
// error) returns a zero exitInfo and a non-nil error; callers must treat that
// as "omit the exit line", never as fatal. The probe is bounded by
// probeTimeout independent of ctx's own deadline. d.DialContext is used
// directly as the transport's dialer so the request always routes through
// the tunnel and never through any process-wide proxy.
func probeExitIP(ctx context.Context, d tunnel.Dialer, endpoint string) (exitInfo, error) {
	if d == nil {
		return exitInfo{}, errors.New("notify: nil dialer")
	}
	if endpoint == "" {
		endpoint = defaultProbeEndpoint
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	// the client is single-use and discarded when probeExitIP returns; disable
	// keep-alives so the connection is not parked in an idle pool that nothing
	// closes — otherwise every probe (fired on each reconnect / zone switch)
	// would leak a persistConn goroutine + fd for a long-running daemon.
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             func(*http.Request) (*url.URL, error) { return nil, nil },
			DialContext:       d.DialContext,
			DisableKeepAlives: true,
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return exitInfo{}, fmt.Errorf("notify: build probe request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return exitInfo{}, fmt.Errorf("notify: probe request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return exitInfo{}, fmt.Errorf("notify: probe returned status %d", resp.StatusCode)
	}

	var info exitInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, probeBodyLimit)).Decode(&info); err != nil {
		return exitInfo{}, fmt.Errorf("notify: decode probe response: %w", err)
	}

	return info, nil
}
