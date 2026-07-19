// Package egress defines the outbound-connection port interfaces shared across
// the vpntunnel internal packages. These are hexagonal egress ports: they
// appear in the signatures of cross-package interfaces (a router returns a
// Dialer and Resolver; a forwarder accepts them), so they must be one shared
// named contract each rather than redeclared per consumer.
//
// Callers use Dialer exclusively; implementations that own resources (e.g. a
// userspace WireGuard device) also implement DialerCloser; implementations that
// report tunnel health also implement HealthReporter; those that support DNS
// resolution inside the tunnel also implement Resolver. The package has no I/O
// and no imports beyond the standard library.
package egress

import (
	"context"
	"net"
	"net/netip"
	"time"
)

// Dialer opens TCP connections to remote hosts. Implementations route
// through whatever egress they represent (WireGuard, etc.).
// Implementations that own underlying resources should also implement
// DialerCloser to allow callers to tear them down on shutdown.
type Dialer interface {
	// DialContext opens a connection to address on network, honouring ctx
	// cancellation. network is always "tcp" in production use.
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// DialerCloser is implemented by Dialers that own resources — for example,
// a userspace WireGuard device, an open file handle, or a connection pool —
// and must be torn down explicitly on shutdown. Callers that need lifecycle
// management should type-assert to DialerCloser at shutdown rather than
// requiring all Dialers to carry a no-op Close.
type DialerCloser interface {
	Dialer
	// Close releases all resources held by the dialer. It must be safe to
	// call more than once (subsequent calls are no-ops).
	Close() error
}

// Resolver looks up the IP addresses that a tunnel's DNS would return for a
// hostname. Implementations are concurrent-safe. The returned slice is non-nil
// on success but may be empty if the host resolves to no addresses (NXDOMAIN /
// empty answer — both surface as len() == 0 with err == nil; callers must map
// an empty result to a "no such host" error themselves).
type Resolver interface {
	// LookupHost resolves host to IP addresses using the tunnel's DNS.
	// Returns a nil slice and non-nil error on resolution failure.
	// Returns a non-nil, possibly empty slice and nil error on NXDOMAIN.
	LookupHost(ctx context.Context, host string) ([]netip.Addr, error)
}

// HealthReporter is implemented by Dialers that can report tunnel-level
// health to an external probe. v4 uses this only for the WireGuard
// dialer's WireGuard handshake age; future implementations may report
// different signals (e.g. last successful connect, queue depth).
//
// HealthReporter is intentionally NOT a sub-interface of Dialer: the
// /healthz handler depends only on LastHandshake, not on DialContext,
// and coupling the two would force fake reporters in tests to satisfy
// the full Dialer surface. Callers that need both must type-assert
// separately.
type HealthReporter interface {
	// LastHandshake returns the most recent moment the tunnel's
	// underlying transport completed a handshake with the peer, in the
	// implementation's local clock (typically time.Now()'s clock).
	//
	// A zero time.Time (IsZero() == true) means no handshake has yet
	// completed since the dialer was constructed; callers MUST treat
	// this as "not yet healthy", NOT as "infinitely fresh".
	//
	// Errors returned by LastHandshake represent a failure to query the
	// underlying transport (UAPI parse failure, device closed). The
	// returned time.Time is unspecified when err != nil.
	LastHandshake() (time.Time, error)
}
