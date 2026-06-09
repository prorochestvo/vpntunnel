// Package health implements the HTTP handler for the proxy's /healthz
// liveness probe. The handler queries a tunnel.HealthReporter for the
// last WireGuard handshake time and answers 200/503 with a fixed JSON
// body shape. It is the sole consumer of HealthReporter today.
package health

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"httpproxy/internal/tunnel"
)

// NewHandler returns an http.Handler that responds to GET /healthz.
//
// The handler asks reporter.LastHandshake() for the time of the most
// recent tunnel handshake and compares it against now. When
// (now - handshake) <= maxAge the handler returns 200 with body:
//
//	{"status":"ok","handshake_age_seconds":N}
//
// Otherwise it returns 503 with body:
//
//	{"status":"unhealthy","reason":<tag>,"handshake_age_seconds":N}
//
// where <tag> is one of: "no_handshake" (handshake time is zero),
// "handshake_stale" (age exceeds maxAge), or "reporter_error" (the
// reporter returned a non-nil error). On "no_handshake" and
// "reporter_error" the handshake_age_seconds field is -1 (sentinel —
// no meaningful age).
//
// The handler logs at Debug for ok responses and Warn for 503
// responses. opLog may be nil; if nil, slog.Default() is used.
func NewHandler(reporter tunnel.HealthReporter, maxAge time.Duration, opLog *slog.Logger) http.Handler {
	logger := opLog
	if logger == nil {
		logger = slog.Default()
	}
	return &handler{reporter: reporter, maxAge: maxAge, now: time.Now, log: logger}
}

// handler implements http.Handler for the /healthz liveness probe.
type handler struct {
	reporter tunnel.HealthReporter
	maxAge   time.Duration
	now      func() time.Time
	log      *slog.Logger
}

// ServeHTTP handles the /healthz probe request.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	}
	ts, err := h.reporter.LastHandshake()
	if err != nil {
		h.write(w, http.StatusServiceUnavailable, response{
			Status:              "unhealthy",
			Reason:              "reporter_error",
			HandshakeAgeSeconds: -1,
		})
		h.log.Warn("healthz reporter error")
		return
	}
	if ts.IsZero() {
		h.write(w, http.StatusServiceUnavailable, response{
			Status:              "unhealthy",
			Reason:              "no_handshake",
			HandshakeAgeSeconds: -1,
		})
		h.log.Warn("healthz no handshake yet")
		return
	}
	age := h.now().Sub(ts)
	ageSec := int64(age / time.Second)
	if age > h.maxAge {
		h.write(w, http.StatusServiceUnavailable, response{
			Status:              "unhealthy",
			Reason:              "handshake_stale",
			HandshakeAgeSeconds: ageSec,
		})
		h.log.Warn("healthz handshake stale", slog.Int64("age_seconds", ageSec))
		return
	}
	h.write(w, http.StatusOK, response{
		Status:              "ok",
		HandshakeAgeSeconds: ageSec,
	})
	h.log.Debug("healthz ok", slog.Int64("age_seconds", ageSec))
}

// response is the fixed JSON body shape for /healthz.
type response struct {
	Status              string `json:"status"`
	Reason              string `json:"reason,omitempty"`
	HandshakeAgeSeconds int64  `json:"handshake_age_seconds"`
}

func (h *handler) write(w http.ResponseWriter, status int, body response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // unrecoverable; client gone
}
