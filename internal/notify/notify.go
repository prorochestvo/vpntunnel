// Package notify sends operator-facing notifications when the tunnel the
// service is using changes. The only shipped implementation posts to the
// Telegram Bot API; Nop is a no-op used when notifications are disabled so
// callers never need to nil-check.
package notify

import (
	"context"

	"vpntunnel/internal/tunnel"
)

// SourceStreaming and SourceOnDemand identify which tunnel-management
// component raised an Event. SourceStreaming events (always-on supervisor:
// startup, every successful reconnect) are never rate-limited. SourceOnDemand
// events (on-demand scheduler zone switches) are subject to the notifier's
// dedup/rate-limit policy, because zone switches can be frequent.
const (
	SourceStreaming Source = iota
	SourceOnDemand
)

// Source identifies which tunnel-management component raised an Event.
type Source int

// Event describes one tunnel-change occurrence a Notifier may report.
type Event struct {
	// Source identifies which component raised the event; it determines
	// whether the notifier's dedup/rate-limit policy applies.
	Source Source
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
	Dialer tunnel.Dialer
}

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
	Notify(ctx context.Context, ev Event)
}

// Nop is a Notifier that discards every event. It is the default used when
// notifications are not configured, so call sites never need to nil-check.
type Nop struct{}

// Notify implements Notifier by doing nothing.
func (Nop) Notify(context.Context, Event) {}

// compile-time interface check.
var _ Notifier = Nop{}
