package main

import (
	"context"

	"vpntunnel/internal/application"
	lazy "vpntunnel/internal/application/lazy"
	"vpntunnel/internal/gateway/router"
)

// compile-time assertion: rotateAdapter must satisfy router.Rotator.
var _ router.Rotator = rotateAdapter{}

// rotateAdapter bridges *lazy.StreamingSupervisor.RotateIfIdle and
// *application.ProxyService.ActiveSessions to the transport-local
// router.Rotator interface, so the apiserver package never imports lazy
// (mirroring how the handlers package keeps lazy out via Router/ZoneChecker/
// TunnelCatalog). It has exactly one consumer — this binary — so it lives
// under cmd/ rather than internal/, per the project's package-placement rule.
type rotateAdapter struct {
	sup *lazy.StreamingSupervisor
	svc *application.ProxyService
}

// Rotate implements router.Rotator by delegating the gate decision to
// RotateIfIdle (busy = svc.ActiveSessions() > 0) and mapping the outcome.
// ActiveSessions is re-read here, separately from the busy closure, because it
// is a display-only field on the RotationSkippedActive response body; a
// session finishing between the gate's authoritative recheck (under the
// supervisor's lock) and this read is a harmless race — the gating decision
// itself already happened correctly.
func (a rotateAdapter) Rotate(ctx context.Context, force bool) (router.RotationResult, error) {
	res, err := a.sup.RotateIfIdle(ctx, force, func() bool { return a.svc.ActiveSessions() > 0 })
	if err != nil {
		return router.RotationResult{}, err
	}

	out := router.RotationResult{Country: res.Country}
	switch res.Outcome {
	case lazy.RotateRotated:
		out.Outcome = router.RotationRotated
	case lazy.RotateSkippedActive:
		out.Outcome = router.RotationSkippedActive
		out.ActiveSessions = a.svc.ActiveSessions()
	case lazy.RotateUnavailable:
		out.Outcome = router.RotationUnavailable
	}
	return out, nil
}
