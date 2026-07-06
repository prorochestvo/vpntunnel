package main

import (
	"context"

	lazy "vpntunnel/internal/application/lazy"
	"vpntunnel/internal/service"
	"vpntunnel/internal/transport/apiserver"
)

// compile-time assertion: rotateAdapter must satisfy apiserver.Rotator.
var _ apiserver.Rotator = rotateAdapter{}

// rotateAdapter bridges *lazy.StreamingSupervisor.RotateIfIdle and
// *service.ProxyService.ActiveSessions to the transport-local
// apiserver.Rotator interface, so the apiserver package never imports lazy
// (mirroring how the handlers package keeps lazy out via Router/ZoneChecker/
// TunnelCatalog). It has exactly one consumer — this binary — so it lives
// under cmd/ rather than internal/, per the project's package-placement rule.
type rotateAdapter struct {
	sup *lazy.StreamingSupervisor
	svc *service.ProxyService
}

// Rotate implements apiserver.Rotator by delegating the gate decision to
// RotateIfIdle (busy = svc.ActiveSessions() > 0) and mapping the outcome.
// ActiveSessions is re-read here, separately from the busy closure, because it
// is a display-only field on the RotationSkippedActive response body; a
// session finishing between the gate's authoritative recheck (under the
// supervisor's lock) and this read is a harmless race — the gating decision
// itself already happened correctly.
func (a rotateAdapter) Rotate(ctx context.Context, force bool) (apiserver.RotationResult, error) {
	res, err := a.sup.RotateIfIdle(ctx, force, func() bool { return a.svc.ActiveSessions() > 0 })
	if err != nil {
		return apiserver.RotationResult{}, err
	}

	out := apiserver.RotationResult{Country: res.Country}
	switch res.Outcome {
	case lazy.RotateRotated:
		out.Outcome = apiserver.RotationRotated
	case lazy.RotateSkippedActive:
		out.Outcome = apiserver.RotationSkippedActive
		out.ActiveSessions = a.svc.ActiveSessions()
	case lazy.RotateUnavailable:
		out.Outcome = apiserver.RotationUnavailable
	}
	return out, nil
}
