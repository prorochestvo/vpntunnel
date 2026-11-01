package handlers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/gateway/httpV1/handlers"
	"vpntunnel/internal/tools/rotation"
)

// rotateStub is a fixed-outcome rotation.Rotator for unit-testing the handler in
// isolation (the router package covers the auth/method-role path end-to-end).
type rotateStub struct {
	result rotation.RotationResult
	err    error
}

func (s rotateStub) Rotate(context.Context, bool) (rotation.RotationResult, error) {
	return s.result, s.err
}

func TestRotateHandler(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)

	do := func(h http.Handler, method, target string) (*httptest.ResponseRecorder, map[string]any) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(method, target, nil))
		var body map[string]any
		if b, _ := io.ReadAll(rr.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &body)
		}
		return rr, body
	}

	t.Run("non-POST returns 405 with Allow header", func(t *testing.T) {
		t.Parallel()
		rr, _ := do(handlers.NewRotateHandler(rotateStub{}, log), http.MethodGet, "/v1/admin/rotate")
		assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
		assert.Equal(t, http.MethodPost, rr.Header().Get("Allow"))
	})

	t.Run("invalid force returns 400", func(t *testing.T) {
		t.Parallel()
		rr, _ := do(handlers.NewRotateHandler(rotateStub{}, log), http.MethodPost, "/v1/admin/rotate?force=notabool")
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("rotated returns 200 with country and no active_sessions", func(t *testing.T) {
		t.Parallel()
		h := handlers.NewRotateHandler(rotateStub{result: rotation.RotationResult{Outcome: rotation.RotationRotated, Country: "se"}}, log)
		rr, body := do(h, http.MethodPost, "/v1/admin/rotate")
		require.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "rotated", body["status"])
		assert.Equal(t, "se", body["country"])
		assert.NotContains(t, body, "active_sessions")
	})

	t.Run("skipped_active returns 200 with active_sessions and no country", func(t *testing.T) {
		t.Parallel()
		h := handlers.NewRotateHandler(rotateStub{result: rotation.RotationResult{Outcome: rotation.RotationSkippedActive, ActiveSessions: 3}}, log)
		rr, body := do(h, http.MethodPost, "/v1/admin/rotate")
		require.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "skipped_active", body["status"])
		assert.Equal(t, float64(3), body["active_sessions"])
		assert.NotContains(t, body, "country")
	})

	t.Run("unavailable returns 503", func(t *testing.T) {
		t.Parallel()
		h := handlers.NewRotateHandler(rotateStub{result: rotation.RotationResult{Outcome: rotation.RotationUnavailable}}, log)
		rr, body := do(h, http.MethodPost, "/v1/admin/rotate")
		require.Equal(t, http.StatusServiceUnavailable, rr.Code)
		assert.Equal(t, "unavailable", body["status"])
	})

	t.Run("nil rotator defaults to NoopRotator and returns 503", func(t *testing.T) {
		t.Parallel()
		rr, body := do(handlers.NewRotateHandler(nil, log), http.MethodPost, "/v1/admin/rotate")
		require.Equal(t, http.StatusServiceUnavailable, rr.Code)
		assert.Equal(t, "unavailable", body["status"])
	})
}
