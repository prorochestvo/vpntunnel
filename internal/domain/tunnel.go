package domain

import (
	"errors"
	"regexp"
	"time"
)

// ParseTunnelID validates s as a derived tunnel id (64 lowercase hex chars) and
// returns it as a TunnelID. Use it at trust boundaries — e.g. the {id} path
// segment of an incoming request — where the input is not known to be well-formed.
func ParseTunnelID(s string) (TunnelID, error) {
	if !tunnelIDPattern.MatchString(s) {
		return "", errors.New("tunnel id must be 64 lowercase hex characters")
	}
	return TunnelID(s), nil
}

// TunnelID is the stable, non-secret per-host identifier for a tunnel: the
// hex-encoded HMAC-SHA256 of a config basename under the host key (derived by
// internal/tools/hmackey.DeriveID). It is published in /v1/tunnels and used as
// the {id} path segment, so inputs crossing a trust boundary are validated with
// ParseTunnelID.
//
// TODO(T08c): thread TunnelID through the EligibleSet catalog and the {id}
// path-segment handler so the id stops travelling as a bare string end to end.
type TunnelID string

// String returns the id as a plain string.
func (id TunnelID) String() string { return string(id) }

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

// tunnelIDPattern is the shape of a derived tunnel id: 64 lowercase hex chars.
var tunnelIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
