package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/application/asyncjob"
	"vpntunnel/internal/tunnel"
)

// fakePool is a test double for tunnelPool.
type fakePool struct {
	reports []tunnel.TunnelHealth
}

var _ tunnelPool = (*fakePool)(nil)

func (f *fakePool) Reports() []tunnel.TunnelHealth { return f.reports }

// stubCounter is a test double for AsyncJobCounter.
type stubCounter struct {
	counts asyncjob.JobCounts
	err    error
}

var _ AsyncJobCounter = (*stubCounter)(nil)

func (s *stubCounter) Counts() (asyncjob.JobCounts, error) { return s.counts, s.err }

func TestHealthHandler_ServeHTTP(t *testing.T) {
	t.Parallel()

	const maxAge = 180 * time.Second
	now := time.Now()
	recent := now.Add(-10 * time.Second)
	old := now.Add(-200 * time.Second)

	t.Run("all_healthy_returns_ok_200", func(t *testing.T) {
		t.Parallel()
		reports := []tunnel.TunnelHealth{
			{ID: "se-sto-wg-001", LastHandshake: recent},
			{ID: "de-fra-wg-001", LastHandshake: recent},
			{ID: "nl-ams-wg-001", LastHandshake: recent},
		}
		h := NewHealthHandler(&fakePool{reports: reports}, maxAge, &stubCounter{}, testLogger(t))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		var body healthResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		assert.Equal(t, "ok", body.Status)
		require.Len(t, body.Tunnels, 3)
		for _, e := range body.Tunnels {
			assert.True(t, e.Healthy, "expected tunnel %q to be healthy", e.ID)
		}
	})

	t.Run("none_healthy_returns_down_503", func(t *testing.T) {
		t.Parallel()
		reports := []tunnel.TunnelHealth{
			{ID: "se-sto-wg-001", LastHandshake: time.Time{}},
			{ID: "de-fra-wg-001", LastHandshake: time.Time{}},
			{ID: "nl-ams-wg-001", LastHandshake: time.Time{}},
		}
		h := NewHealthHandler(&fakePool{reports: reports}, maxAge, &stubCounter{}, testLogger(t))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		var body healthResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		assert.Equal(t, "down", body.Status)
		for _, e := range body.Tunnels {
			assert.False(t, e.Healthy)
			assert.Equal(t, int64(-1), e.HandshakeAgeSeconds)
		}
	})

	t.Run("mixed_returns_degraded_200", func(t *testing.T) {
		t.Parallel()
		reports := []tunnel.TunnelHealth{
			{ID: "se-sto-wg-001", LastHandshake: recent},
			{ID: "de-fra-wg-001", LastHandshake: recent},
			{ID: "nl-ams-wg-001", LastHandshake: old},
		}
		h := NewHealthHandler(&fakePool{reports: reports}, maxAge, &stubCounter{}, testLogger(t))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		var body healthResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		assert.Equal(t, "degraded", body.Status)
		require.Len(t, body.Tunnels, 3)
		assert.True(t, body.Tunnels[0].Healthy)
		assert.True(t, body.Tunnels[1].Healthy)
		assert.False(t, body.Tunnels[2].Healthy)
	})

	t.Run("single_reporter_error_marks_unhealthy", func(t *testing.T) {
		t.Parallel()
		errored := errors.New("wg device closed")
		reports := []tunnel.TunnelHealth{
			{ID: "se-sto-wg-001", LastHandshake: recent},
			{ID: "de-fra-wg-001", Err: errored},
			{ID: "nl-ams-wg-001", LastHandshake: recent},
		}
		h := NewHealthHandler(&fakePool{reports: reports}, maxAge, &stubCounter{}, testLogger(t))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		var body healthResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		assert.Equal(t, "degraded", body.Status)
		require.Len(t, body.Tunnels, 3)
		assert.True(t, body.Tunnels[0].Healthy)
		assert.False(t, body.Tunnels[1].Healthy)
		assert.Equal(t, int64(-1), body.Tunnels[1].HandshakeAgeSeconds)
		assert.True(t, body.Tunnels[2].Healthy)
	})

	t.Run("zero_handshake_marks_unhealthy_with_age_minus_one", func(t *testing.T) {
		t.Parallel()
		reports := []tunnel.TunnelHealth{
			{ID: "se-sto-wg-001", LastHandshake: time.Time{}},
		}
		h := NewHealthHandler(&fakePool{reports: reports}, maxAge, &stubCounter{}, testLogger(t))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		var body healthResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		assert.Equal(t, "down", body.Status)
		require.Len(t, body.Tunnels, 1)
		assert.False(t, body.Tunnels[0].Healthy)
		assert.Equal(t, int64(-1), body.Tunnels[0].HandshakeAgeSeconds)
	})

	t.Run("empty_pool_returns_down_503", func(t *testing.T) {
		t.Parallel()
		h := NewHealthHandler(&fakePool{reports: nil}, maxAge, &stubCounter{}, testLogger(t))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		var body healthResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		assert.Equal(t, "down", body.Status)
		assert.Empty(t, body.Tunnels)
	})

	t.Run("non_GET_returns_405", func(t *testing.T) {
		t.Parallel()
		h := NewHealthHandler(&fakePool{}, maxAge, &stubCounter{}, testLogger(t))

		methods := []string{
			http.MethodPost,
			http.MethodPut,
			http.MethodDelete,
			http.MethodPatch,
			http.MethodHead,
			http.MethodOptions,
		}
		for _, method := range methods {
			method := method
			t.Run(method, func(t *testing.T) {
				t.Parallel()
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(method, "/v1/health", nil)
				h.ServeHTTP(rec, req)

				assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
				assert.Equal(t, "GET", rec.Header().Get("Allow"))
			})
		}
	})

	t.Run("response_omits_internal_error_text", func(t *testing.T) {
		t.Parallel()
		sensitiveErr := errors.New("upstream peer unreachable: AS31013")
		reports := []tunnel.TunnelHealth{
			{ID: "se-sto-wg-001", LastHandshake: recent},
			{ID: "de-fra-wg-001", Err: sensitiveErr},
		}
		h := NewHealthHandler(&fakePool{reports: reports}, maxAge, &stubCounter{}, testLogger(t))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		h.ServeHTTP(rec, req)

		raw := rec.Body.String()
		assert.NotContains(t, raw, "upstream peer unreachable", "error message must not appear in response body")
		assert.NotContains(t, raw, "AS31013", "ASN must not appear in response body")

		var body healthResponse
		require.NoError(t, json.Unmarshal([]byte(raw), &body))
		assert.False(t, body.Tunnels[1].Healthy)
		assert.Equal(t, int64(-1), body.Tunnels[1].HandshakeAgeSeconds)
	})
}

func TestTunnelHealthEntry(t *testing.T) {
	t.Parallel()

	const maxAge = 180 * time.Second
	now := time.Now()

	t.Run("recent_handshake_no_err_returns_healthy", func(t *testing.T) {
		t.Parallel()
		ts := now.Add(-10 * time.Second)
		h := tunnel.TunnelHealth{ID: "se-sto-wg-001", LastHandshake: ts}
		e := tunnelHealthEntry(h, now, maxAge)
		assert.True(t, e.Healthy)
		assert.Equal(t, int64(10), e.HandshakeAgeSeconds)
		assert.Equal(t, "se-sto-wg-001", e.ID)
	})

	t.Run("old_handshake_returns_unhealthy_with_correct_age", func(t *testing.T) {
		t.Parallel()
		ts := now.Add(-200 * time.Second)
		h := tunnel.TunnelHealth{ID: "de-fra-wg-001", LastHandshake: ts}
		e := tunnelHealthEntry(h, now, maxAge)
		assert.False(t, e.Healthy)
		assert.Equal(t, int64(200), e.HandshakeAgeSeconds)
	})

	t.Run("zero_handshake_returns_unhealthy_with_age_minus_one", func(t *testing.T) {
		t.Parallel()
		h := tunnel.TunnelHealth{ID: "se-sto-wg-001", LastHandshake: time.Time{}}
		e := tunnelHealthEntry(h, now, maxAge)
		assert.False(t, e.Healthy)
		assert.Equal(t, int64(-1), e.HandshakeAgeSeconds)
	})

	t.Run("non_nil_err_returns_unhealthy_regardless_of_handshake_freshness", func(t *testing.T) {
		t.Parallel()
		ts := now.Add(-5 * time.Second) // very fresh, but Err is set
		h := tunnel.TunnelHealth{ID: "nl-ams-wg-001", LastHandshake: ts, Err: errors.New("device closed")}
		e := tunnelHealthEntry(h, now, maxAge)
		assert.False(t, e.Healthy)
		assert.Equal(t, int64(-1), e.HandshakeAgeSeconds)
	})

	t.Run("handshake_exactly_at_max_age_is_healthy", func(t *testing.T) {
		t.Parallel()
		ts := now.Add(-maxAge) // age == maxAge exactly; <= maxAge is true
		h := tunnel.TunnelHealth{ID: "se-sto-wg-001", LastHandshake: ts}
		e := tunnelHealthEntry(h, now, maxAge)
		assert.True(t, e.Healthy)
		assert.Equal(t, int64(maxAge/time.Second), e.HandshakeAgeSeconds)
	})

	t.Run("future_handshake_clamped_to_zero_age_healthy", func(t *testing.T) {
		t.Parallel()
		ts := now.Add(5 * time.Second)
		h := tunnel.TunnelHealth{ID: "se-sto-wg-001", LastHandshake: ts}
		e := tunnelHealthEntry(h, now, maxAge)
		assert.True(t, e.Healthy)
		assert.Equal(t, int64(0), e.HandshakeAgeSeconds)
	})
}

func TestHealthHandler_AsyncCounts(t *testing.T) {
	t.Parallel()

	const maxAge = 180 * time.Second
	now := time.Now()
	recent := now.Add(-10 * time.Second)

	// healthyPool returns one healthy tunnel so the response is 200/ok.
	healthyPool := &fakePool{reports: []tunnel.TunnelHealth{
		{ID: "se-sto-wg-001", LastHandshake: recent},
	}}
	// downPool returns a zero-time tunnel so the response is 503/down.
	downPool := &fakePool{reports: []tunnel.TunnelHealth{
		{ID: "se-sto-wg-001", LastHandshake: time.Time{}},
	}}

	t.Run("zero state — all three counts are 0 in the JSON response", func(t *testing.T) {
		t.Parallel()
		h := NewHealthHandler(healthyPool, maxAge, &stubCounter{}, testLogger(t))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/health", nil)
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		var body healthResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		assert.Equal(t, 0, body.PendingJobsCount)
		assert.Equal(t, 0, body.CompletedJobsCount)
		assert.Equal(t, 0, body.TombstoneJobsCount)
	})

	t.Run("seeded counts — pending=5, completed=3, tombstone=2 propagate to the JSON exactly", func(t *testing.T) {
		t.Parallel()
		counter := &stubCounter{counts: asyncjob.JobCounts{Pending: 5, Completed: 3, Tombstone: 2}}
		h := NewHealthHandler(healthyPool, maxAge, counter, testLogger(t))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/health", nil)
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		var body healthResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		assert.Equal(t, 5, body.PendingJobsCount)
		assert.Equal(t, 3, body.CompletedJobsCount)
		assert.Equal(t, 2, body.TombstoneJobsCount)
	})

	t.Run("Counts error — all three counts degrade to -1, HTTP status stays 200", func(t *testing.T) {
		t.Parallel()
		counter := &stubCounter{err: errors.New("bbolt: bucket scan failed")}
		h := NewHealthHandler(healthyPool, maxAge, counter, testLogger(t))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/health", nil)
		h.ServeHTTP(rec, req)

		// tunnel is healthy → HTTP 200 must not change because of the Counts error.
		assert.Equal(t, http.StatusOK, rec.Code)
		var body healthResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		assert.Equal(t, -1, body.PendingJobsCount)
		assert.Equal(t, -1, body.CompletedJobsCount)
		assert.Equal(t, -1, body.TombstoneJobsCount)
	})

	t.Run("503 from tunnel does not skip the counts call — counts are present in the 503 body too", func(t *testing.T) {
		t.Parallel()
		counter := &stubCounter{counts: asyncjob.JobCounts{Pending: 7, Completed: 1, Tombstone: 0}}
		h := NewHealthHandler(downPool, maxAge, counter, testLogger(t))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/health", nil)
		h.ServeHTTP(rec, req)

		// tunnel is down → 503, but counts must still be in the body.
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		var body healthResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		assert.Equal(t, "down", body.Status)
		assert.Equal(t, 7, body.PendingJobsCount)
		assert.Equal(t, 1, body.CompletedJobsCount)
		assert.Equal(t, 0, body.TombstoneJobsCount)
	})

	t.Run("noopCounter — zero counts and nil error", func(t *testing.T) {
		t.Parallel()
		// stubCounter with no fields set returns asyncjob.JobCounts{}, nil —
		// the same contract noopCounter (in server.go) fulfils. This test
		// confirms the handler serialises all three zero counts correctly and
		// that the zero-value struct round-trips through JSON without mutation.
		counter := &stubCounter{}
		h := NewHealthHandler(healthyPool, maxAge, counter, testLogger(t))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/health", nil)
		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		var body healthResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
		assert.Equal(t, 0, body.PendingJobsCount, "pending must be 0 for zero-value counter")
		assert.Equal(t, 0, body.CompletedJobsCount, "completed must be 0 for zero-value counter")
		assert.Equal(t, 0, body.TombstoneJobsCount, "tombstone must be 0 for zero-value counter")
	})
}

// testLogger returns a discard logger for tests. It is a helper; callers
// should not log test-meaningful output through it.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
