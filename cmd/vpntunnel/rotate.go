package main

import (
	"context"

	"vpntunnel/internal/application"
	"vpntunnel/internal/application/tunnelpool"
	"vpntunnel/internal/tools/rotation"
)

// rotateAdapter bridges *tunnelpool.StreamingSupervisor.RotateIfIdle and
// *application.ProxyService.ActiveSessions to the transport-local
// rotation.Rotator interface, so the router package never imports tunnelpool
// (mirroring how the handlers package keeps tunnelpool out via Router/ZoneChecker/
// TunnelCatalog). It has exactly one consumer — this binary — so it lives
// under cmd/ rather than internal/, per the project's package-placement rule.
type rotateAdapter struct {
	sup *tunnelpool.StreamingSupervisor
	svc *application.ProxyService
}

// Rotate implements rotation.Rotator by delegating the gate decision to
// RotateIfIdle (busy = svc.ActiveSessions() > 0) and mapping the outcome.
// ActiveSessions is re-read here, separately from the busy closure, because it
// is a display-only field on the RotationSkippedActive response body; a
// session finishing between the gate's authoritative recheck (under the
// supervisor's lock) and this read is a harmless race — the gating decision
// itself already happened correctly.
func (a rotateAdapter) Rotate(ctx context.Context, force bool) (rotation.RotationResult, error) {
	res, err := a.sup.RotateIfIdle(ctx, force, func() bool { return a.svc.ActiveSessions() > 0 })
	if err != nil {
		return rotation.RotationResult{}, err
	}

	out := rotation.RotationResult{Country: res.Country}
	switch res.Outcome {
	case tunnelpool.RotateRotated:
		out.Outcome = rotation.RotationRotated
	case tunnelpool.RotateSkippedActive:
		out.Outcome = rotation.RotationSkippedActive
		out.ActiveSessions = a.svc.ActiveSessions()
	case tunnelpool.RotateUnavailable:
		out.Outcome = rotation.RotationUnavailable
	}
	return out, nil
}
