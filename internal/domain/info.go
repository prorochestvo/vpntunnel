package domain

import "time"

// TunnelHealth is a single-tunnel health snapshot returned by the pool.
// Err is non-nil when the underlying HealthReporter failed to query its
// transport; LastHandshake is unspecified in that case.
type TunnelHealth struct {
	// ID is the tunnel basename without ".conf".
	ID string
	// LastHandshake is the most recent handshake time. Unspecified when Err != nil.
	LastHandshake time.Time
	// Err is non-nil when the HealthReporter returned an error for this tunnel.
	Err error
}

// TunnelInfo is the public metadata view of a single tunnel, returned by
// the pool to the /v1/tunnels endpoint. EndpointCountry is derived from
// the first hyphen-separated segment of ID when that segment is exactly
// two lowercase ASCII letters; otherwise empty.
type TunnelInfo struct {
	// ID is the tunnel basename without ".conf".
	ID string
	// Name is the human-readable name for the tunnel. Currently identical to ID;
	// kept as a separate field for future-proofing.
	Name string
	// EndpointCountry is the two-letter country code derived from the ID, or empty
	// when the first hyphen-separated segment is not exactly two lowercase ASCII letters.
	// Example: "se-sto-wg-001" yields "SE"; "mullvad-ch-zrh-wg-001" yields "".
	EndpointCountry string
}
