package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"vpntunnel/internal/gateway/httpV1/dto"
	"vpntunnel/internal/tools/rotation"
)

// NewRotateHandler returns the http.Handler for /v1/admin/rotate. It mirrors the
// other v1 handlers (health, tunnels, proxy) so the router registers all four
// uniformly and never imports dto. rotator triggers the graceful streaming-tunnel
// rotation; a nil rotator defaults to rotation.NoopRotator so the endpoint answers
// with a clean 503 instead of panicking when none is wired.
//
// The route is registered methodless (see the router mux) because the "/"
// catch-all defeats Go's automatic 405 for method-scoped patterns, so the POST
// check lives in the handler. The response body never includes endpoint, key
// material, PSK, peer public key, or TunnelHealth.Err — only status, country (on
// rotated), and active_sessions (on skipped). The authoritative old→new-country
// log line is emitted by the supervisor; this handler logs only the terse
// HTTP-level outcome, to avoid double-logging the same event.
func NewRotateHandler(rotator rotation.Rotator, log *slog.Logger) http.Handler {
	if rotator == nil {
		rotator = rotation.NoopRotator{}
	}
	return &rotateHandler{rotator: rotator, log: log}
}

type rotateHandler struct {
	rotator rotation.Rotator
	log     *slog.Logger
}

func (h *rotateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// the withRequestID middleware set X-Request-Id on the response header before
	// this handler runs; read it back (matches the other handlers).
	reqID := w.Header().Get("X-Request-Id")

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		WriteError(w, "method_not_allowed", http.StatusMethodNotAllowed, reqID)
		return
	}

	force, err := parseForceParam(r.URL.Query().Get("force"))
	if err != nil {
		WriteError(w, "bad_request", http.StatusBadRequest, reqID)
		return
	}

	result, err := h.rotator.Rotate(r.Context(), force)
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
		h.log.Info("rotate: streaming tunnel rotated",
			slog.String("request_id", reqID),
			slog.String("country", result.Country),
		)
	case rotation.RotationSkippedActive:
		active := result.ActiveSessions
		resp = dto.RotateResponse{Status: "skipped_active", ActiveSessions: &active}
		h.log.Info("rotate: skipped, sessions active",
			slog.String("request_id", reqID),
			slog.Int64("active_sessions", result.ActiveSessions),
		)
	case rotation.RotationUnavailable:
		status = http.StatusServiceUnavailable
		resp = dto.RotateResponse{Status: "unavailable"}
		h.log.Info("rotate: unavailable", slog.String("request_id", reqID))
	default:
		h.log.Error("rotate: rotator returned an unknown outcome", slog.String("request_id", reqID))
		WriteError(w, "internal_error", http.StatusInternalServerError, reqID)
		return
	}

	data, err := json.Marshal(resp)
	if err != nil {
		h.log.Error("rotate: failed to marshal response", slog.String("err", err.Error()))
		WriteError(w, "internal_error", http.StatusInternalServerError, reqID)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data) // unrecoverable once WriteHeader sent
}

// parseForceParam parses the ?force query value. An empty value means force was
// not requested (false, the gated path); any non-empty value must parse via
// strconv.ParseBool or the request is rejected as bad input.
func parseForceParam(raw string) (bool, error) {
	if raw == "" {
		return false, nil
	}
	return strconv.ParseBool(raw)
}
