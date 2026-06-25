package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/asyncjob"
	"vpntunnel/internal/transport/apiserver/handlers"
	"vpntunnel/internal/tunnel"
)

// compile-time interface assertions.
var (
	_ handlers.ZoneChecker = (*fakeChecker)(nil)
	_ handlers.Router      = (*fakeRouter)(nil)
	_ tunnel.Resolver      = (*fakeResolver)(nil)
)

// fakeChecker implements handlers.ZoneChecker. It accepts any zone in its
// knownIDs set and rejects all others.
type fakeChecker struct {
	knownIDs map[string]struct{}
}

func (c *fakeChecker) IsEligible(zoneID string) bool {
	_, ok := c.knownIDs[zoneID]
	return ok
}

// fakeRouter implements handlers.Router. On Route it returns the configured
// dialer and resolver, or an error if err is non-nil.
type fakeRouter struct {
	dialer   tunnel.Dialer
	resolver tunnel.Resolver
	err      error
}

func (r *fakeRouter) Route(_ context.Context, _ string) (tunnel.Dialer, tunnel.Resolver, func(), error) {
	if r.err != nil {
		return nil, nil, nil, r.err
	}
	return r.dialer, r.resolver, func() {}, nil
}

// fakeResolver satisfies tunnel.Resolver for tests.
type fakeResolver struct {
	addrs []netip.Addr
	err   error
}

func (f *fakeResolver) LookupHost(_ context.Context, _ string) ([]netip.Addr, error) {
	return f.addrs, f.err
}

// fakeForwarder captures the outbound *http.Request and tunnel ID for assertion.
// It drains the body so http.MaxBytesReader errors surface here and are
// returned to the caller for 413 mapping.
type fakeForwarder struct {
	captured   *http.Request
	capturedID string
	bodyErr    error
}

var _ handlers.Forwarder = (*fakeForwarder)(nil)

func (f *fakeForwarder) Forward(w http.ResponseWriter, r *http.Request, tunnelID string, _ tunnel.Dialer, _ tunnel.Resolver) error {
	f.captured = r
	f.capturedID = tunnelID
	if r.Body != nil {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			f.bodyErr = err
			return err
		}
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// newTestHandler builds a proxy handler with the given checker, router, and
// forwarder using sensible defaults for the timeout parameters.
func newTestHandler(t *testing.T, checker handlers.ZoneChecker, router handlers.Router, fwd *fakeForwarder, maxBodyBytes int64) http.Handler {
	t.Helper()
	return handlers.NewProxyHandler(
		checker,
		router,
		fwd,
		maxBodyBytes,
		30*time.Second, // upstreamTimeout
		5*time.Minute,  // maxUpstreamTimeout
		discardLogger(t),
		nil, // asyncPool: nil → sync-only mode for existing validation tests
	)
}

// discardLogger returns a *slog.Logger that drops all output.
func discardLogger(_ *testing.T) *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// defaultTestChecker returns a ZoneChecker that accepts "se-sto-wg-001".
func defaultTestChecker() *fakeChecker {
	return &fakeChecker{knownIDs: map[string]struct{}{"se-sto-wg-001": {}}}
}

// defaultTestRouter returns a Router that always succeeds for any zone,
// returning a minimal no-op dialer and resolver.
func defaultTestRouter() *fakeRouter {
	return &fakeRouter{
		dialer:   noopDialer{},
		resolver: &fakeResolver{},
	}
}

// noopDialer satisfies tunnel.Dialer but is never called in proxy unit tests
// because fakeForwarder ignores the dialer argument entirely.
type noopDialer struct{}

func (noopDialer) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return nil, fmt.Errorf("noopDialer: not connected")
}

// serveProxy issues req through the handler via httptest.ResponseRecorder.
func serveProxy(t *testing.T, handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// newRequest creates a test request for the given method+path with an optional
// body. The caller is responsible for setting path values (scheme, rest, id)
// via req.SetPathValue as appropriate for the test scenario.
func newRequest(t *testing.T, method, path string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, "https://api.example.test"+path, body)
	require.NoError(t, err)
	return req
}

func TestProxyHandler_Validation(t *testing.T) {
	t.Parallel()

	t.Run("invalid_scheme_rejected", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		h := newTestHandler(t, defaultTestChecker(), defaultTestRouter(), fwd, 1<<20)

		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/ftp/example.com", nil)
		req.SetPathValue("scheme", "ftp")
		req.SetPathValue("rest", "example.com")
		req.SetPathValue("id", "se-sto-wg-001")

		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "invalid_scheme", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("missing_tunnel_id_rejected", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		h := newTestHandler(t, defaultTestChecker(), defaultTestRouter(), fwd, 1<<20)

		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com", nil)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com")
		// intentionally no {id} path value set — PathValue returns "" → 400 tunnel_required.

		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "tunnel_required", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("unknown_tunnel_id_rejected", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		h := newTestHandler(t, defaultTestChecker(), defaultTestRouter(), fwd, 1<<20)

		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com", nil)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com")
		req.SetPathValue("id", "nope")

		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "unknown_tunnel", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("bad_timeout_format_rejected", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		h := newTestHandler(t, defaultTestChecker(), defaultTestRouter(), fwd, 1<<20)

		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com", nil)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com")
		req.SetPathValue("id", "se-sto-wg-001")
		req.Header.Set("X-Proxy-Timeout", "not_a_duration")

		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "bad_request", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("timeout_exceeds_max_rejected", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		// maxUpstreamTimeout = 5m, client sends 10m.
		h := handlers.NewProxyHandler(defaultTestChecker(), defaultTestRouter(), fwd, 1<<20, 30*time.Second, 5*time.Minute, discardLogger(t), nil)

		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com", nil)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com")
		req.SetPathValue("id", "se-sto-wg-001")
		req.Header.Set("X-Proxy-Timeout", "10m")

		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "timeout_too_large", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("negative_timeout_rejected", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		h := newTestHandler(t, defaultTestChecker(), defaultTestRouter(), fwd, 1<<20)

		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com", nil)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com")
		req.SetPathValue("id", "se-sto-wg-001")
		req.Header.Set("X-Proxy-Timeout", "-1s")

		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "timeout_too_large", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("malformed_target_url_rejected", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		h := newTestHandler(t, defaultTestChecker(), defaultTestRouter(), fwd, 1<<20)

		// rest is empty → host is empty → url.Parse succeeds but Host == "".
		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/http/", nil)
		req.SetPathValue("scheme", "http")
		req.SetPathValue("rest", "")
		req.SetPathValue("id", "se-sto-wg-001")

		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "bad_request", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("service_headers_not_forwarded", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		h := newTestHandler(t, defaultTestChecker(), defaultTestRouter(), fwd, 1<<20)

		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com/path", nil)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com/path")
		req.SetPathValue("id", "se-sto-wg-001")
		// service headers that the client might attempt to set.
		req.Header.Set("X-Vpntunnel-Token", "should-not-forward")
		req.Header.Set("X-Proxy-Timeout", "30s")
		req.Header.Set("X-Proxy-Forward-Headers", "")
		// a normal always-forwarded header.
		req.Header.Set("Content-Type", "application/json")

		w := serveProxy(t, h, req)
		require.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, fwd.captured)

		assert.Equal(t, "application/json", fwd.captured.Header.Get("Content-Type"))
		assert.Empty(t, fwd.captured.Header.Get("X-Vpntunnel-Token"), "service header must be stripped")
		assert.Empty(t, fwd.captured.Header.Get("X-Vpntunnel-Tunnel-Id"), "internal tunnel-id header must be stripped before forwarding")
		assert.Empty(t, fwd.captured.Header.Get("X-Proxy-Timeout"), "service header must be stripped")
		assert.Empty(t, fwd.captured.Header.Get("X-Proxy-Forward-Headers"), "service header must be stripped")
	})

	t.Run("default_headers_forwarded", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		h := newTestHandler(t, defaultTestChecker(), defaultTestRouter(), fwd, 1<<20)

		req := newRequest(t, http.MethodPost, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com/api", nil)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com/api")
		req.SetPathValue("id", "se-sto-wg-001")
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("User-Agent", "test-agent/1.0")

		w := serveProxy(t, h, req)
		require.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, fwd.captured)

		assert.Equal(t, "text/plain", fwd.captured.Header.Get("Content-Type"))
		assert.Equal(t, "application/json", fwd.captured.Header.Get("Accept"))
		assert.Equal(t, "Bearer tok", fwd.captured.Header.Get("Authorization"))
		assert.Equal(t, "test-agent/1.0", fwd.captured.Header.Get("User-Agent"))
	})

	t.Run("extension_headers_forwarded_canonical_case", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		h := newTestHandler(t, defaultTestChecker(), defaultTestRouter(), fwd, 1<<20)

		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com/", nil)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com/")
		req.SetPathValue("id", "se-sto-wg-001")
		// lowercase extension header names — should be canonicalised on forward.
		req.Header.Set("X-Proxy-Forward-Headers", "x-custom-foo, x-custom-bar")
		req.Header.Set("x-custom-foo", "A")
		req.Header.Set("x-custom-bar", "B")

		w := serveProxy(t, h, req)
		require.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, fwd.captured)

		assert.Equal(t, "A", fwd.captured.Header.Get("X-Custom-Foo"),
			"extension header must be forwarded with canonical casing")
		assert.Equal(t, "B", fwd.captured.Header.Get("X-Custom-Bar"),
			"extension header must be forwarded with canonical casing")
	})

	t.Run("oversize_body_returns_413", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		// cap at 10 bytes.
		const maxBody = 10
		h := newTestHandler(t, defaultTestChecker(), defaultTestRouter(), fwd, maxBody)

		// body is 11 bytes — one over the limit.
		body := strings.NewReader(strings.Repeat("x", maxBody+1))

		req := newRequest(t, http.MethodPost, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com/upload", body)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com/upload")
		req.SetPathValue("id", "se-sto-wg-001")
		req.Header.Set("Content-Type", "application/octet-stream")
		// simulate chunked encoding by not setting Content-Length.
		req.ContentLength = -1

		w := serveProxy(t, h, req)
		// the forwarder drains the body and returns the MaxBytesError; the
		// handler maps it to 413 body_too_large.
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		assert.Equal(t, "body_too_large", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("zero_timeout_rejected", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		h := newTestHandler(t, defaultTestChecker(), defaultTestRouter(), fwd, 1<<20)

		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com", nil)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com")
		req.SetPathValue("id", "se-sto-wg-001")
		req.Header.Set("X-Proxy-Timeout", "0s")

		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "timeout_too_large", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("exact_max_body_allowed", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		const maxBody = 10
		h := newTestHandler(t, defaultTestChecker(), defaultTestRouter(), fwd, maxBody)

		// body is exactly at the limit — must reach the forwarder without 413.
		body := strings.NewReader(strings.Repeat("x", maxBody))

		req := newRequest(t, http.MethodPost, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com/upload", body)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com/upload")
		req.SetPathValue("id", "se-sto-wg-001")
		req.Header.Set("Content-Type", "application/octet-stream")
		req.ContentLength = -1

		w := serveProxy(t, h, req)
		// fakeForwarder returns 200 on success; reaching it without 413 proves
		// the body cap allows exactly maxBody bytes through.
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("absent_user_agent_not_invented", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		h := newTestHandler(t, defaultTestChecker(), defaultTestRouter(), fwd, 1<<20)

		// valid request with no User-Agent header set.
		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com/", nil)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com/")
		req.SetPathValue("id", "se-sto-wg-001")
		// intentionally no User-Agent header.

		w := serveProxy(t, h, req)
		require.Equal(t, http.StatusOK, w.Code)
		require.NotNil(t, fwd.captured)

		// copyProxyHeaders must suppress Go's default "Go-http-client/…" by
		// explicitly setting User-Agent to "". An absent client UA must not
		// become an invented one on the outbound request.
		assert.Equal(t, "", fwd.captured.Header.Get("User-Agent"),
			"absent client User-Agent must not be replaced by Go's default")
	})

	t.Run("tunnel_id_passed_to_forwarder", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		h := newTestHandler(t, defaultTestChecker(), defaultTestRouter(), fwd, 1<<20)

		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com/", nil)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com/")
		req.SetPathValue("id", "se-sto-wg-001")

		w := serveProxy(t, h, req)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "se-sto-wg-001", fwd.capturedID, "forwarder must receive the tunnel ID")
	})

}

// compile-time assertion: *fakeAsyncPool must satisfy the asyncPool interface
// that proxy.go defines (unexported — verified here via handlers package usage).
var _ interface {
	SubmitOrFetch(ctx context.Context, tag string, req *http.Request) (asyncjob.Outcome, error)
} = (*fakeAsyncPool)(nil)

// fakeAsyncPool is a minimal fake for the asyncPool interface. Each call
// returns the configured outcome or error; the fake captures the tag and
// submitted request so tests can assert on them.
type fakeAsyncPool struct {
	outcome     asyncjob.Outcome
	err         error
	lastTag     string
	lastRequest *http.Request
}

func (f *fakeAsyncPool) SubmitOrFetch(_ context.Context, tag string, req *http.Request) (asyncjob.Outcome, error) {
	f.lastTag = tag
	f.lastRequest = req
	if f.err != nil {
		return nil, f.err
	}
	return f.outcome, nil
}

// newAsyncTestHandler builds a proxy handler wired with the given asyncPool.
// fwd may be nil when the test only exercises the async path.
func newAsyncTestHandler(t *testing.T, ap *fakeAsyncPool) http.Handler {
	t.Helper()
	fwd := &fakeForwarder{}
	return handlers.NewProxyHandler(
		defaultTestChecker(),
		defaultTestRouter(),
		fwd,
		1<<20,
		30*time.Second,
		5*time.Minute,
		discardLogger(t),
		ap,
	)
}

// newAsyncRequest builds a request pre-configured with a valid tunnel id (via
// path value) and the given Proxy-Retry-Tag.
func newAsyncRequest(t *testing.T, tag string) *http.Request {
	t.Helper()
	req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com/", nil)
	req.SetPathValue("scheme", "https")
	req.SetPathValue("rest", "example.com/")
	req.SetPathValue("id", "se-sto-wg-001")
	if tag != "" {
		req.Header.Set("Proxy-Retry-Tag", tag)
	}
	return req
}

func TestProxyHandler_ServeHTTP(t *testing.T) {
	t.Parallel()

	t.Run("sync path: route error returns 503 tunnel_unavailable", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		// inject a router that always fails so Route returns an error.
		errRouter := &fakeRouter{err: errors.New("tunnel device is down")}
		h := handlers.NewProxyHandler(defaultTestChecker(), errRouter, fwd, 1<<20, 30*time.Second, 5*time.Minute, discardLogger(t), nil)

		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com/", nil)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com/")
		req.SetPathValue("id", "se-sto-wg-001")

		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, "tunnel_unavailable", w.Header().Get("X-Proxy-Error"))
		// the forwarder must not have been called when Route already failed.
		assert.Nil(t, fwd.captured, "forwarder must not be called when Route fails")
	})

	t.Run("sync path: header absent → byte-identical v1 behavior", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		// nil asyncPool: ensure the absence of Proxy-Retry-Tag never touches async logic.
		h := handlers.NewProxyHandler(defaultTestChecker(), defaultTestRouter(), fwd, 1<<20, 30*time.Second, 5*time.Minute, discardLogger(t), nil)

		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/https/example.com/", nil)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com/")
		req.SetPathValue("id", "se-sto-wg-001")
		// no Proxy-Retry-Tag header set.

		w := serveProxy(t, h, req)
		// fakeForwarder returns 200 on success — confirms the sync path reached the forwarder.
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, w.Header().Get("Proxy-Async-Status"), "sync path must not set Proxy-Async-Status")
		assert.Empty(t, w.Header().Get("X-Proxy-Error"), "sync path success must not set X-Proxy-Error")
	})

	t.Run("tag_invalid: too short", func(t *testing.T) {
		t.Parallel()
		h := newAsyncTestHandler(t, &fakeAsyncPool{outcome: asyncjob.OutcomePending{}})

		req := newAsyncRequest(t, "abc") // 3 chars — below the 8-char minimum.
		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "tag_invalid", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("tag_invalid: too long", func(t *testing.T) {
		t.Parallel()
		h := newAsyncTestHandler(t, &fakeAsyncPool{outcome: asyncjob.OutcomePending{}})

		tag := strings.Repeat("a", 129) // 129 chars — above the 128-char maximum.
		req := newAsyncRequest(t, tag)
		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "tag_invalid", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("tag_invalid: bad characters", func(t *testing.T) {
		t.Parallel()
		h := newAsyncTestHandler(t, &fakeAsyncPool{outcome: asyncjob.OutcomePending{}})

		// space and slash are both illegal.
		for _, bad := range []string{"has space!!", "has/slash!!", "has@symbol!!"} {
			bad := bad
			t.Run(bad, func(t *testing.T) {
				t.Parallel()
				req := newAsyncRequest(t, bad)
				w := serveProxy(t, h, req)
				assert.Equal(t, http.StatusBadRequest, w.Code, "tag %q must be rejected", bad)
				assert.Equal(t, "tag_invalid", w.Header().Get("X-Proxy-Error"))
			})
		}
	})

	t.Run("async_disabled: nil pool", func(t *testing.T) {
		t.Parallel()
		fwd := &fakeForwarder{}
		// explicitly nil asyncPool.
		h := handlers.NewProxyHandler(defaultTestChecker(), defaultTestRouter(), fwd, 1<<20, 30*time.Second, 5*time.Minute, discardLogger(t), nil)

		req := newAsyncRequest(t, "validtag-1234567")
		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, "async_disabled", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("OutcomePending → 202 + Proxy-Async-Status: pending + JSON envelope", func(t *testing.T) {
		t.Parallel()
		const tag = "validtag-pending1"
		ap := &fakeAsyncPool{outcome: asyncjob.OutcomePending{}}
		h := newAsyncTestHandler(t, ap)

		req := newAsyncRequest(t, tag)
		w := serveProxy(t, h, req)

		assert.Equal(t, http.StatusAccepted, w.Code)
		assert.Equal(t, "pending", w.Header().Get("Proxy-Async-Status"))

		var body map[string]string
		require.NoError(t, json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&body))
		assert.Equal(t, "pending", body["status"])
		assert.Equal(t, tag, body["retry_tag"])
	})

	t.Run("OutcomeCompleted → upstream verbatim, no Proxy-Async-Status header", func(t *testing.T) {
		t.Parallel()
		ap := &fakeAsyncPool{
			outcome: asyncjob.OutcomeCompleted{
				Response: asyncjob.UpstreamResponse{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type": []string{"application/json"},
						// Transfer-Encoding is hop-by-hop and must be stripped.
						"Transfer-Encoding": []string{"chunked"},
					},
					Body: []byte(`{"ok":true}`),
				},
			},
		}
		h := newAsyncTestHandler(t, ap)

		req := newAsyncRequest(t, "validtag-complete1")
		w := serveProxy(t, h, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, w.Header().Get("Proxy-Async-Status"), "completed response must not carry Proxy-Async-Status")
		assert.Empty(t, w.Header().Get("X-Proxy-Error"), "completed response must not carry X-Proxy-Error")
		assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
		assert.Empty(t, w.Header().Get("Transfer-Encoding"), "hop-by-hop header must be stripped from completed response")
		assert.Equal(t, `{"ok":true}`, w.Body.String())
	})

	t.Run("OutcomeTombstoned → 410 + tag_evicted", func(t *testing.T) {
		t.Parallel()
		ap := &fakeAsyncPool{outcome: asyncjob.OutcomeTombstoned{}}
		h := newAsyncTestHandler(t, ap)

		req := newAsyncRequest(t, "validtag-tombstn1")
		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusGone, w.Code)
		assert.Equal(t, "tag_evicted", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("OutcomeQueueFull → 503 + queue_full", func(t *testing.T) {
		t.Parallel()
		ap := &fakeAsyncPool{outcome: asyncjob.OutcomeQueueFull{}}
		h := newAsyncTestHandler(t, ap)

		req := newAsyncRequest(t, "validtag-queueful")
		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, "queue_full", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("ErrBodyTooLarge → 413 + body_too_large", func(t *testing.T) {
		t.Parallel()
		ap := &fakeAsyncPool{err: asyncjob.ErrBodyTooLarge}
		h := newAsyncTestHandler(t, ap)

		req := newAsyncRequest(t, "validtag-bigbody1")
		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		assert.Equal(t, "body_too_large", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("ErrPoolClosed → 503 + shutting_down", func(t *testing.T) {
		t.Parallel()
		ap := &fakeAsyncPool{err: asyncjob.ErrPoolClosed}
		h := newAsyncTestHandler(t, ap)

		req := newAsyncRequest(t, "validtag-poolclos")
		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Equal(t, "shutting_down", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("generic pool error → 500 + internal", func(t *testing.T) {
		t.Parallel()
		ap := &fakeAsyncPool{err: errors.New("store is on fire")}
		h := newAsyncTestHandler(t, ap)

		req := newAsyncRequest(t, "validtag-generror")
		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, "internal", w.Header().Get("X-Proxy-Error"))
	})

	// P1.2: CONNECT with tag must be rejected before any async work.
	t.Run("CONNECT method with tag → 405 method_not_allowed", func(t *testing.T) {
		t.Parallel()
		ap := &fakeAsyncPool{outcome: asyncjob.OutcomePending{}}
		h := newAsyncTestHandler(t, ap)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodConnect, "https://api.example.test/v1/tunnels/se-sto-wg-001/proxy/https/example.com/", nil)
		require.NoError(t, err)
		req.SetPathValue("scheme", "https")
		req.SetPathValue("rest", "example.com/")
		req.SetPathValue("id", "se-sto-wg-001")
		req.Header.Set("Proxy-Retry-Tag", "validtag-connect1")

		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
		assert.Equal(t, "method_not_allowed", w.Header().Get("X-Proxy-Error"))
	})

	// P1.3: tag length boundaries — exactly 8 and 128 chars must be accepted.
	t.Run("tag_valid: exactly 8 chars accepted", func(t *testing.T) {
		t.Parallel()
		const tag = "abcd1234" // exactly 8 characters
		ap := &fakeAsyncPool{outcome: asyncjob.OutcomePending{}}
		h := newAsyncTestHandler(t, ap)

		req := newAsyncRequest(t, tag)
		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusAccepted, w.Code)
		assert.Equal(t, "pending", w.Header().Get("Proxy-Async-Status"))
	})

	t.Run("tag_valid: exactly 128 chars accepted", func(t *testing.T) {
		t.Parallel()
		tag := strings.Repeat("a", 128) // exactly 128 characters
		ap := &fakeAsyncPool{outcome: asyncjob.OutcomePending{}}
		h := newAsyncTestHandler(t, ap)

		req := newAsyncRequest(t, tag)
		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusAccepted, w.Code)
		assert.Equal(t, "pending", w.Header().Get("Proxy-Async-Status"))
	})

	// P1.4: non-200 OutcomeCompleted must propagate the upstream status code verbatim.
	t.Run("OutcomeCompleted → upstream 422 propagated verbatim", func(t *testing.T) {
		t.Parallel()
		ap := &fakeAsyncPool{
			outcome: asyncjob.OutcomeCompleted{
				Response: asyncjob.UpstreamResponse{
					StatusCode: http.StatusUnprocessableEntity,
					Header: http.Header{
						"Content-Type": []string{"application/json"},
					},
					Body: []byte(`{"err":"unprocessable"}`),
				},
			},
		}
		h := newAsyncTestHandler(t, ap)

		req := newAsyncRequest(t, "validtag-422resp1")
		w := serveProxy(t, h, req)

		assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
		assert.Empty(t, w.Header().Get("Proxy-Async-Status"), "completed response must not carry Proxy-Async-Status")
		assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
		assert.Equal(t, `{"err":"unprocessable"}`, w.Body.String())
	})

	// P2.3: assert tag reaches the pool unchanged in the OutcomePending path.
	t.Run("OutcomePending → tag delivered to pool unchanged", func(t *testing.T) {
		t.Parallel()
		const tag = "validtag-pending1"
		ap := &fakeAsyncPool{outcome: asyncjob.OutcomePending{}}
		h := newAsyncTestHandler(t, ap)

		req := newAsyncRequest(t, tag)
		w := serveProxy(t, h, req)

		require.Equal(t, http.StatusAccepted, w.Code)
		assert.Equal(t, tag, ap.lastTag, "pool must receive the validated tag unchanged")
	})

	// scheme validation must fire before the async branch: a tagged request with
	// an unsupported scheme must return 400 invalid_scheme without ever reaching
	// the async pool.
	t.Run("tag with invalid scheme → scheme rejected before async branch", func(t *testing.T) {
		t.Parallel()
		ap := &fakeAsyncPool{outcome: asyncjob.OutcomePending{}}
		h := newAsyncTestHandler(t, ap)

		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/ftp/example.com", nil)
		req.SetPathValue("scheme", "ftp")
		req.SetPathValue("rest", "example.com")
		req.SetPathValue("id", "se-sto-wg-001")
		req.Header.Set("Proxy-Retry-Tag", "validtag-ftprej1")

		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "invalid_scheme", w.Header().Get("X-Proxy-Error"))
		assert.Empty(t, ap.lastTag, "async pool must not be reached when scheme is invalid")
	})

	t.Run("async malformed_target_url_rejected", func(t *testing.T) {
		t.Parallel()
		ap := &fakeAsyncPool{outcome: asyncjob.OutcomePending{}}
		h := newAsyncTestHandler(t, ap)
		req := newRequest(t, http.MethodGet, "/v1/tunnels/se-sto-wg-001/proxy/http/", nil)
		req.SetPathValue("scheme", "http")
		req.SetPathValue("rest", "")
		req.SetPathValue("id", "se-sto-wg-001")
		req.Header.Set("Proxy-Retry-Tag", "validtag-malform1")
		w := serveProxy(t, h, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Equal(t, "bad_request", w.Header().Get("X-Proxy-Error"))
	})

	// the async clone submitted to SubmitOrFetch must carry X-Vpntunnel-Tunnel-Id
	// so that ZoneRoutingForwarder can recover the validated tunnel id from the
	// stored request after the URL has been rewritten to the upstream absolute URL.
	t.Run("async clone carries internal tunnel-id header before storage", func(t *testing.T) {
		t.Parallel()
		ap := &fakeAsyncPool{outcome: asyncjob.OutcomePending{}}
		h := newAsyncTestHandler(t, ap)

		req := newAsyncRequest(t, "validtag-clonehdr")
		w := serveProxy(t, h, req)

		require.Equal(t, http.StatusAccepted, w.Code)
		require.NotNil(t, ap.lastRequest, "SubmitOrFetch must have been called")
		assert.Equal(t, "se-sto-wg-001", ap.lastRequest.Header.Get("X-Vpntunnel-Tunnel-Id"),
			"internal tunnel-id header must be stamped on the async clone")
	})
}
