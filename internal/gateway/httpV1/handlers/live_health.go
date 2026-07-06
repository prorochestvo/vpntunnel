package handlers

import (
	"vpntunnel/internal/tunnel"
)

// NewLiveHealthModel returns a LiveHealthModel that aggregates health from the
// streaming supervisor and the on-demand scheduler. Both sources are required.
// *lazy.StreamingSupervisor satisfies streaming and *lazy.OnDemandScheduler
// satisfies onDemand; local interfaces avoid importing the lazy package upward.
func NewLiveHealthModel(streaming liveHealther, onDemand liveHealther) *LiveHealthModel {
	return &LiveHealthModel{streaming: streaming, onDemand: onDemand}
}

// LiveHealthModel aggregates the streaming supervisor and the on-demand
// scheduler into the method set consumed by the health handler.
// Reports() is safe for concurrent use; it does not hold any locks itself —
// safety is delegated to the source's LiveHealth().
//
// Response shape (CLAUDE.md invariant): the returned slices contain
// tunnel.TunnelHealth values; the health handler projects them to the fixed
// JSON body shape. This type never adds fields or exposes Err text in any
// handler-visible output.
type LiveHealthModel struct {
	streaming liveHealther
	onDemand  liveHealther
}

// liveHealther is the minimal live-health contract. Both StreamingSupervisor
// and OnDemandScheduler satisfy it; keeping this unexported avoids leaking the
// shape outside the handlers package.
type liveHealther interface {
	// LiveHealth returns a health snapshot of the currently live device.
	// ok is false when no device is live.
	LiveHealth() (tunnel.TunnelHealth, bool)
}

// Reports returns the live health snapshot for the health handler.
//
// Ordering: streaming first, then on-demand IFF one is currently live. A
// streaming device with no live on-demand returns a single-element slice; both
// live returns two elements.
func (m *LiveHealthModel) Reports() []tunnel.TunnelHealth {
	streamHealth, streamOk := m.streaming.LiveHealth()
	odHealth, odOk := m.onDemand.LiveHealth()

	switch {
	case streamOk && odOk:
		return []tunnel.TunnelHealth{streamHealth, odHealth}
	case streamOk:
		return []tunnel.TunnelHealth{streamHealth}
	case odOk:
		return []tunnel.TunnelHealth{odHealth}
	default:
		return []tunnel.TunnelHealth{}
	}
}

// compile-time assertion: LiveHealthModel must satisfy the tunnelPool interface
// that healthHandler depends on.
var _ tunnelPool = (*LiveHealthModel)(nil)
