package domain

import (
	"context"
	"net"
)

// SourceStreaming and SourceOnDemand identify which tunnel-management
// component raised a TunnelChangeEvent. SourceStreaming events (always-on
// supervisor: startup, every successful reconnect) are never rate-limited.
// SourceOnDemand events (on-demand scheduler zone switches) are subject to the
// notifier's dedup/rate-limit policy, because zone switches can be frequent.
const (
	SourceStreaming TunnelChangeSource = iota
	SourceOnDemand
)

// TunnelChangeSource identifies which tunnel-management component raised a
// TunnelChangeEvent.
type TunnelChangeSource int

// TunnelChangeEvent describes one tunnel-change occurrence a notifier may
// report.
type TunnelChangeEvent struct {
	// Source identifies which component raised the event; it determines
	// whether the notifier's dedup/rate-limit policy applies.
	Source TunnelChangeSource
	// Title is a short, caller-provided label (e.g. "started",
	// "switched tunnel", "on-demand: se").
	Title string
	// Country is the lowercase two-letter tunnel country code; may be "".
	Country string
	// Filename is the tunnel's .conf basename (e.g. "se-sto-wg-001.conf").
	Filename string
	// Dialer is the freshly built tunnel dialer, captured for the
	// best-effort exit-IP probe. May be nil, in which case the probe is
	// skipped.
	Dialer dialer
}

// dialer is the minimal outbound-connection contract the exit-IP probe needs,
// and the one behavioural port this package declares (see the package doc). It
// stays unexported because no caller has to name it: a concrete tunnel dialer
// satisfies it structurally on assignment to TunnelChangeEvent.Dialer, and a
// notifier consuming the field assigns it on to its own local contract.
type dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}
