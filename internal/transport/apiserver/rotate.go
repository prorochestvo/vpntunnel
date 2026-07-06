package apiserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"vpntunnel/internal/gateway/httpV1/handlers"
)

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

// RotationResult is the transport-local outcome of a Rotate call. It never
// carries endpoint, key material, PSK, or peer public key — only the fields
// the /v1/admin/rotate response contract allows: the outcome (mapped to
// "status"), the 2-letter country code (on RotationRotated), and the active
// session count (on RotationSkippedActive).
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
// *lazy.StreamingSupervisor.RotateIfIdle — so the transport layer never
// imports lazy, mirroring how Router/ZoneChecker/TunnelCatalog keep lazy out
// of the handlers package.
type Rotator interface {
	// Rotate triggers a graceful streaming rotation. force=false gates the
	// rotation on active sessions (see RotationSkippedActive); force=true
	// rotates unconditionally, still via break-before-make + settle. err is
	// non-nil only when ctx was cancelled before the attempt completed.
	Rotate(ctx context.Context, force bool) (RotationResult, error)
}

// compile-time assertion: noopRotator must satisfy Rotator.
var _ Rotator = noopRotator{}

// noopRotator is the zero-allocation default used when Options.Rotator is
// nil. It always reports RotationUnavailable so /v1/admin/rotate answers with
// a clean 503 instead of a nil-pointer panic when no rotator is wired.
type noopRotator struct{}

// Rotate implements Rotator by always reporting RotationUnavailable.
func (noopRotator) Rotate(context.Context, bool) (RotationResult, error) {
	return RotationResult{Outcome: RotationUnavailable}, nil
}

// rotateResponse is the JSON body for /v1/admin/rotate. Country and
// ActiveSessions are omitted from the encoded body unless meaningful for the
// outcome (see RotationResult), keeping the three response shapes exactly as
// documented in CLAUDE.md/README.md.
type rotateResponse struct {
	Status         string `json:"status"`
	Country        string `json:"country,omitempty"`
	ActiveSessions *int64 `json:"active_sessions,omitempty"`
}

// rotator returns the configured Rotator, defaulting to noopRotator{} so
// handleRotate never needs to nil-check Options.Rotator.
func (s *Server) rotator() Rotator {
	if s.opts.Rotator != nil {
		return s.opts.Rotator
	}
	return noopRotator{}
}

// handleRotate serves /v1/admin/rotate. The route is registered methodless
// (see buildMux) because the "/" catch-all defeats Go's automatic 405 for
// method-scoped patterns, so the method check lives here instead.
//
// force is parsed from the ?force query parameter via strconv.ParseBool; an
// empty value defaults to false, an invalid value is a 400. The response body
// never includes endpoint, key material, PSK, peer public key, or
// TunnelHealth.Err — only status, country (on rotated), and active_sessions
// (on skipped). The authoritative old→new-country log line is emitted by the
// supervisor itself; this handler logs only the terse HTTP-level outcome, to
// avoid double-logging the same event.
func (s *Server) handleRotate(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDFromContext(r.Context())

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		handlers.WriteError(w, "method_not_allowed", http.StatusMethodNotAllowed, reqID)
		return
	}

	force, err := parseForceParam(r.URL.Query().Get("force"))
	if err != nil {
		handlers.WriteError(w, "bad_request", http.StatusBadRequest, reqID)
		return
	}

	result, err := s.rotator().Rotate(r.Context(), force)
	if err != nil {
		// the caller's ctx was cancelled (e.g. client disconnected) before the
		// rotation attempt completed — there is no client left to answer.
		return
	}

	var resp rotateResponse
	status := http.StatusOK
	switch result.Outcome {
	case RotationRotated:
		resp = rotateResponse{Status: "rotated", Country: result.Country}
		s.log.Info("rotate: streaming tunnel rotated",
			slog.String("request_id", reqID),
			slog.String("country", result.Country),
		)
	case RotationSkippedActive:
		active := result.ActiveSessions
		resp = rotateResponse{Status: "skipped_active", ActiveSessions: &active}
		s.log.Info("rotate: skipped, sessions active",
			slog.String("request_id", reqID),
			slog.Int64("active_sessions", result.ActiveSessions),
		)
	case RotationUnavailable:
		status = http.StatusServiceUnavailable
		resp = rotateResponse{Status: "unavailable"}
		s.log.Info("rotate: unavailable", slog.String("request_id", reqID))
	default:
		s.log.Error("rotate: rotator returned an unknown outcome", slog.String("request_id", reqID))
		handlers.WriteError(w, "internal_error", http.StatusInternalServerError, reqID)
		return
	}

	data, err := json.Marshal(resp)
	if err != nil {
		s.log.Error("rotate: failed to marshal response", slog.String("err", err.Error()))
		handlers.WriteError(w, "internal_error", http.StatusInternalServerError, reqID)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data) // unrecoverable once WriteHeader sent
}

// parseForceParam parses the ?force query value. An empty value means force
// was not requested (false, the gated path); any non-empty value must parse
// via strconv.ParseBool or the request is rejected as bad input.
func parseForceParam(raw string) (bool, error) {
	if raw == "" {
		return false, nil
	}
	return strconv.ParseBool(raw)
}
