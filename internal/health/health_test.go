package health

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"httpproxy/internal/tunnel"
)

// compile-time assertion: fakeReporter satisfies tunnel.HealthReporter.
var _ tunnel.HealthReporter = (*fakeReporter)(nil)

type fakeReporter struct {
	ts  time.Time
	err error
}

func (f *fakeReporter) LastHandshake() (time.Time, error) { return f.ts, f.err }

// newTestHandler constructs a *handler with an injected clock for deterministic tests.
func newTestHandler(reporter tunnel.HealthReporter, maxAge time.Duration, now time.Time) *handler {
	return &handler{
		reporter: reporter,
		maxAge:   maxAge,
		now:      func() time.Time { return now },
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestNewHandler(t *testing.T) {
	t.Parallel()

	// fixed reference clock used in all subtests
	fixedNow := time.Unix(1700000000, 0)
	maxAge := 180 * time.Second

	t.Run("GET /healthz with fresh handshake returns 200 ok", func(t *testing.T) {
		t.Parallel()
		handshakeTime := fixedNow.Add(-30 * time.Second)
		h := newTestHandler(&fakeReporter{ts: handshakeTime}, maxAge, fixedNow)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Len(t, body, 2, "ok body must have exactly 2 fields: status and handshake_age_seconds")
		assert.Equal(t, "ok", body["status"])
		assert.Equal(t, float64(30), body["handshake_age_seconds"])
	})

	t.Run("GET /healthz with zero handshake time returns 503 no_handshake", func(t *testing.T) {
		t.Parallel()
		h := newTestHandler(&fakeReporter{ts: time.Time{}}, maxAge, fixedNow)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Len(t, body, 3, "unhealthy body must have exactly 3 fields")
		assert.Equal(t, "unhealthy", body["status"])
		assert.Equal(t, "no_handshake", body["reason"])
		assert.Equal(t, float64(-1), body["handshake_age_seconds"])
	})

	t.Run("GET /healthz with stale handshake returns 503 handshake_stale", func(t *testing.T) {
		t.Parallel()
		staleAge := 300 * time.Second
		handshakeTime := fixedNow.Add(-staleAge)
		h := newTestHandler(&fakeReporter{ts: handshakeTime}, maxAge, fixedNow)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Len(t, body, 3, "unhealthy body must have exactly 3 fields")
		assert.Equal(t, "unhealthy", body["status"])
		assert.Equal(t, "handshake_stale", body["reason"])
		assert.Equal(t, float64(300), body["handshake_age_seconds"])
	})

	t.Run("GET /healthz with reporter error returns 503 reporter_error", func(t *testing.T) {
		t.Parallel()
		h := newTestHandler(&fakeReporter{err: io.ErrUnexpectedEOF}, maxAge, fixedNow)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Len(t, body, 3, "unhealthy body must have exactly 3 fields")
		assert.Equal(t, "unhealthy", body["status"])
		assert.Equal(t, "reporter_error", body["reason"])
		assert.Equal(t, float64(-1), body["handshake_age_seconds"])
	})

	t.Run("POST /healthz returns 405 method not allowed", func(t *testing.T) {
		t.Parallel()
		h := newTestHandler(&fakeReporter{ts: fixedNow}, maxAge, fixedNow)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
	})
}
