// Package rotation defines the transport-agnostic contract for triggering a
// graceful streaming-tunnel rotation. It carries no HTTP or transport
// dependency: the HTTP handler that drives Rotate lives in the gateway and the
// concrete implementation (bridging *tunnelpool.StreamingSupervisor) lives in
// cmd/vpntunnel, so neither the transport layer nor this package imports tunnelpool.
package rotation

import "context"

// RotationOutcome enumerates the possible results of a Rotator.Rotate call.
type RotationOutcome int

const (
	// RotationRotated means the streaming device was torn down and rebuilt
	// against a fresh random exit; RotationResult.Country is set.
	RotationRotated RotationOutcome = iota
	// RotationSkippedActive means the rotation was gated because active
	// streaming sessions were in flight (force was false);
	// RotationResult.ActiveSessions is set.
	RotationSkippedActive
	// RotationUnavailable means no live device could be rotated — no device
	// was live, the post-teardown build failed, or the attempt was aborted by
	// shutdown. Nothing changed observably beyond the normal reconnect path.
	RotationUnavailable
)

// RotationResult is the outcome of a Rotate call. It never carries endpoint,
// key material, PSK, or peer public key — only the fields the /v1/admin/rotate
// response contract allows: the outcome (mapped to "status"), the 2-letter
// country code (on RotationRotated), and the active session count (on
// RotationSkippedActive).
type RotationResult struct {
	// Outcome selects which of Country / ActiveSessions is meaningful.
	Outcome RotationOutcome
	// Country is the lowercase 2-letter country code of the newly built
	// exit. Set only when Outcome == RotationRotated.
	Country string
	// ActiveSessions is the number of in-flight streaming sessions that
	// caused the rotation to be skipped. Set only when
	// Outcome == RotationSkippedActive.
	ActiveSessions int64
}

// Rotator triggers a graceful streaming-tunnel rotation. Implementations live
// outside this package — the cmd/vpntunnel adapter bridges to
// *tunnelpool.StreamingSupervisor.RotateIfIdle — so the transport layer never
// imports tunnelpool, mirroring how Router/ZoneChecker/TunnelCatalog keep tunnelpool out
// of the handlers package.
type Rotator interface {
	// Rotate triggers a graceful streaming rotation. force=false gates the
	// rotation on active sessions (see RotationSkippedActive); force=true
	// rotates unconditionally, still via break-before-make + settle. err is
	// non-nil only when ctx was cancelled before the attempt completed.
	Rotate(ctx context.Context, force bool) (RotationResult, error)
}

// NoopRotator is the zero-allocation default used when no Rotator is wired. It
// always reports RotationUnavailable so /v1/admin/rotate answers with a clean
// 503 instead of a nil-pointer panic.
type NoopRotator struct{}

// Rotate implements Rotator by always reporting RotationUnavailable.
func (NoopRotator) Rotate(context.Context, bool) (RotationResult, error) {
	return RotationResult{Outcome: RotationUnavailable}, nil
}
