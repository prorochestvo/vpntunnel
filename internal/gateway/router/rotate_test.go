package router_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/gateway/router"
	"vpntunnel/internal/tools/rotation"
)

var _ rotation.Rotator = (*stubRotator)(nil)

// stubRotator is a recording test double for rotation.Rotator: it always
// returns the configured result/err and records every force value it was
// called with, so tests can assert both the response body and that ?force
// reached the rotator unchanged.
type stubRotator struct {
	mu     sync.Mutex
	result rotation.RotationResult
	err    error
	forces []bool
}

func (s *stubRotator) Rotate(_ context.Context, force bool) (rotation.RotationResult, error) {
	s.mu.Lock()
	s.forces = append(s.forces, force)
	s.mu.Unlock()
	return s.result, s.err
}

func (s *stubRotator) forceCalls() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]bool, len(s.forces))
	copy(out, s.forces)
	return out
}

// bodyJSONAny reads resp.Body into a map[string]any and closes the body. Used
// instead of bodyJSON (map[string]string) because active_sessions is a JSON
// number, not a string.
func bodyJSONAny(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var m map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&m))
	return m
}

func TestServer_handleRotate(t *testing.T) {
	t.Parallel()

	t.Run("non-POST with admin token returns 405 with Allow header", func(t *testing.T) {
		t.Parallel()
		stub := &stubRotator{}
		f := startServerMode(t, false, func(o *router.Options) { o.Rotator = stub })
		t.Cleanup(f.shutdown)

		resp := f.get(t, "/v1/admin/rotate", f.adminToken)
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
		assert.Equal(t, http.MethodPost, resp.Header.Get("Allow"))
	})

	t.Run("proxy token returns 403", func(t *testing.T) {
		t.Parallel()
		stub := &stubRotator{}
		f := startServerMode(t, false, func(o *router.Options) { o.Rotator = stub })
		t.Cleanup(f.shutdown)

		resp := f.post(t, "/v1/admin/rotate", f.userToken)
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("no token returns 401", func(t *testing.T) {
		t.Parallel()
		stub := &stubRotator{}
		f := startServerMode(t, false, func(o *router.Options) { o.Rotator = stub })
		t.Cleanup(f.shutdown)

		resp := f.post(t, "/v1/admin/rotate", "")
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("invalid force value returns 400", func(t *testing.T) {
		t.Parallel()
		stub := &stubRotator{}
		f := startServerMode(t, false, func(o *router.Options) { o.Rotator = stub })
		t.Cleanup(f.shutdown)

		resp := f.post(t, "/v1/admin/rotate?force=notabool", f.adminToken)
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("rotated outcome returns 200 with status and country", func(t *testing.T) {
		t.Parallel()
		stub := &stubRotator{result: rotation.RotationResult{Outcome: rotation.RotationRotated, Country: "se"}}
		f := startServerMode(t, false, func(o *router.Options) { o.Rotator = stub })
		t.Cleanup(f.shutdown)

		resp := f.post(t, "/v1/admin/rotate", f.adminToken)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body := bodyJSONAny(t, resp)
		assert.Equal(t, "rotated", body["status"])
		assert.Equal(t, "se", body["country"])
		assert.NotContains(t, body, "active_sessions")
	})

	t.Run("skipped_active outcome returns 200 with status and active_sessions", func(t *testing.T) {
		t.Parallel()
		stub := &stubRotator{result: rotation.RotationResult{Outcome: rotation.RotationSkippedActive, ActiveSessions: 3}}
		f := startServerMode(t, false, func(o *router.Options) { o.Rotator = stub })
		t.Cleanup(f.shutdown)

		resp := f.post(t, "/v1/admin/rotate", f.adminToken)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body := bodyJSONAny(t, resp)
		assert.Equal(t, "skipped_active", body["status"])
		assert.Equal(t, float64(3), body["active_sessions"])
		assert.NotContains(t, body, "country")
	})

	t.Run("unavailable outcome returns 503", func(t *testing.T) {
		t.Parallel()
		stub := &stubRotator{result: rotation.RotationResult{Outcome: rotation.RotationUnavailable}}
		f := startServerMode(t, false, func(o *router.Options) { o.Rotator = stub })
		t.Cleanup(f.shutdown)

		resp := f.post(t, "/v1/admin/rotate", f.adminToken)
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		body := bodyJSONAny(t, resp)
		assert.Equal(t, "unavailable", body["status"])
	})

	t.Run("no rotator configured defaults to NoopRotator and returns 503", func(t *testing.T) {
		t.Parallel()
		// no mutate func — Options.Rotator stays nil.
		f := startServerMode(t, false)
		t.Cleanup(f.shutdown)

		resp := f.post(t, "/v1/admin/rotate", f.adminToken)
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		body := bodyJSONAny(t, resp)
		assert.Equal(t, "unavailable", body["status"])
	})

	t.Run("force=true reaches the rotator", func(t *testing.T) {
		t.Parallel()
		stub := &stubRotator{result: rotation.RotationResult{Outcome: rotation.RotationRotated, Country: "se"}}
		f := startServerMode(t, false, func(o *router.Options) { o.Rotator = stub })
		t.Cleanup(f.shutdown)

		resp := f.post(t, "/v1/admin/rotate?force=true", f.adminToken)
		drainAndClose(t, resp)
		assert.Equal(t, []bool{true}, stub.forceCalls())
	})

	t.Run("no force param defaults to false", func(t *testing.T) {
		t.Parallel()
		stub := &stubRotator{result: rotation.RotationResult{Outcome: rotation.RotationRotated, Country: "se"}}
		f := startServerMode(t, false, func(o *router.Options) { o.Rotator = stub })
		t.Cleanup(f.shutdown)

		resp := f.post(t, "/v1/admin/rotate", f.adminToken)
		drainAndClose(t, resp)
		assert.Equal(t, []bool{false}, stub.forceCalls())
	})

	t.Run("responses carry X-Request-Id", func(t *testing.T) {
		t.Parallel()
		stub := &stubRotator{result: rotation.RotationResult{Outcome: rotation.RotationRotated, Country: "se"}}
		f := startServerMode(t, false, func(o *router.Options) { o.Rotator = stub })
		t.Cleanup(f.shutdown)

		resp := f.post(t, "/v1/admin/rotate", f.adminToken)
		defer drainAndClose(t, resp)
		assert.Regexp(t, uuidv7Pattern, resp.Header.Get("X-Request-Id"))
	})
}
