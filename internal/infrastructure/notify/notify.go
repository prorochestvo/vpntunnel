// Package notify sends operator-facing notifications when the tunnel the
// service is using changes. The only shipped implementation posts to the
// Telegram Bot API; Nop is a no-op used when notifications are disabled so
// callers never need to nil-check.
package notify

import (
	"context"
	"net"

	"vpntunnel/internal/domain"
)

// Notifier reports tunnel-change events to an operator-facing channel.
// Notify must never block the caller: implementations that perform I/O do so
// asynchronously and drop events rather than stall the reconnect loop or a
// proxy request.
type Notifier interface {
	// Notify reports ev. Implementations must return without blocking on
	// network I/O. ctx is not a cancellation handle for the send — a
	// compliant implementation performs the send on its own lifetime, so
	// cancelling ctx does not abort an in-flight notification; the parameter
	// is reserved for future request attribution/tracing.
	Notify(ctx context.Context, ev domain.TunnelChangeEvent)
}

// dialer is the minimal outbound-connection contract this package needs. It is
// declared here, in the consumer, and satisfied structurally by whatever tunnel
// dialer the caller attached to a domain.TunnelChangeEvent; probeExitIP is its
// only user.
type dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Nop is a Notifier that discards every event. It is the default used when
// notifications are not configured, so call sites never need to nil-check.
type Nop struct{}

// Notify implements Notifier by doing nothing.
func (Nop) Notify(context.Context, domain.TunnelChangeEvent) {}
