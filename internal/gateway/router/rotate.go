package router

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"vpntunnel/internal/gateway/httpV1/dto"
	"vpntunnel/internal/gateway/httpV1/handlers"
	"vpntunnel/internal/tools/rotation"
)

// rotator returns the configured Rotator, defaulting to rotation.NoopRotator{}
// so handleRotate never needs to nil-check Options.Rotator.
func (s *Server) rotator() rotation.Rotator {
	if s.opts.Rotator != nil {
		return s.opts.Rotator
	}
	return rotation.NoopRotator{}
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

	var resp dto.RotateResponse
	status := http.StatusOK
	switch result.Outcome {
	case rotation.RotationRotated:
		resp = dto.RotateResponse{Status: "rotated", Country: result.Country}
		s.log.Info("rotate: streaming tunnel rotated",
			slog.String("request_id", reqID),
			slog.String("country", result.Country),
		)
	case rotation.RotationSkippedActive:
		active := result.ActiveSessions
		resp = dto.RotateResponse{Status: "skipped_active", ActiveSessions: &active}
		s.log.Info("rotate: skipped, sessions active",
			slog.String("request_id", reqID),
			slog.Int64("active_sessions", result.ActiveSessions),
		)
	case rotation.RotationUnavailable:
		status = http.StatusServiceUnavailable
		resp = dto.RotateResponse{Status: "unavailable"}
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
