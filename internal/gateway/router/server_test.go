package router_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/application/lazy"
	"vpntunnel/internal/domain"
	"vpntunnel/internal/egress"
	"vpntunnel/internal/gateway/httpV1/handlers"
	"vpntunnel/internal/gateway/middleware"
	"vpntunnel/internal/gateway/router"
	"vpntunnel/internal/gateway/router/apitls"
	"vpntunnel/internal/infrastructure/config"
)

// compile-time assertions: test doubles must satisfy the interfaces they implement.
var (
	_ handlers.ZoneChecker   = (*fakeZoneChecker)(nil)
	_ handlers.Router        = (*fakeZoneRouter)(nil)
	_ handlers.TunnelCatalog = (*fakeCatalog)(nil)
)

// fakeCatalog implements handlers.TunnelCatalog for unit tests. It returns a
// fixed set of entries so tests can assert the catalog JSON body without a
// real EligibleSet.
type fakeCatalog struct{}

func (fakeCatalog) Entries() []lazy.CatalogEntry {
	return []lazy.CatalogEntry{
		{ID: "se-sto-wg-001-hmac", Basename: "se-sto-wg-001", Country: "se"},
		{ID: "de-fra-wg-001-hmac", Basename: "de-fra-wg-001", Country: "de"},
	}
}

// fakeLiveHealther returns a fixed TunnelHealth snapshot for the streaming supervisor fake.
type fakeLiveHealther struct {
	health domain.TunnelHealth
	ok     bool
}

func (f *fakeLiveHealther) LiveHealth() (domain.TunnelHealth, bool) {
	return f.health, f.ok
}

// fakeZoneChecker always reports all zones as eligible.
type fakeZoneChecker struct{}

func (fakeZoneChecker) IsEligible(_ string) bool { return true }

// fakeZoneRouter always returns an error so async/sync routing paths fail cleanly.
type fakeZoneRouter struct{}

func (fakeZoneRouter) Route(_ context.Context, _ string) (egress.Dialer, egress.Resolver, func(), error) {
	return nil, nil, nil, fmt.Errorf("fakeZoneRouter: not connected")
}

// serverFixture holds everything a single test needs to talk to the API server.
type serverFixture struct {
	client     *http.Client
	baseURL    string
	adminToken string
	userToken  string
	shutdown   func()
}

// startServerMode builds a complete server fixture. When tlsEnabled is true the
// server runs HTTPS (TLS 1.3) with a self-signed cert; when false it runs plain
// HTTP with Cert: nil. startServer is a thin wrapper that always enables TLS so
// existing tests compile unchanged. mutate, if given, is applied to the
// constructed router.Options before New is called — e.g. to inject a
// Rotator — so one-off fields don't need a dedicated fixture function.
func startServerMode(t *testing.T, tlsEnabled bool, mutate ...func(*router.Options)) *serverFixture {
	t.Helper()

	dir := t.TempDir()

	// two distinct 64-byte tokens — long enough for LoadTokens.
	mk := func(b byte) []byte {
		s := make([]byte, 64)
		for i := range s {
			s[i] = b + byte(i%26)
		}
		return s
	}
	adminPlain := mk('a')
	userPlain := mk('0')

	writeToken := func(name string, content []byte) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, content, 0o600))
		require.NoError(t, os.Chmod(p, 0o600))
		return p
	}

	adminFile := writeToken("admin.token", adminPlain)
	userFile := writeToken("user.token", userPlain)

	apiAuth := config.APIAuth{
		AdminTokenFile: adminFile,
		ProxyTokenFile: userFile,
	}

	tokens, err := middleware.LoadTokens(apiAuth, dir)
	require.NoError(t, err)

	// load or generate a TLS cert only when TLS is enabled.
	var cert *tls.Certificate
	if tlsEnabled {
		certDir := filepath.Join(dir, "tls")
		cert, _, err = apitls.LoadOrGenerate(certDir, "localhost", nil, slog.New(slog.DiscardHandler))
		require.NoError(t, err)
	}

	// fake live health: streaming reports se-sto-wg-001 healthy; on-demand idle.
	now := time.Now()
	streaming := &fakeLiveHealther{
		health: domain.TunnelHealth{ID: "se-sto-wg-001", LastHandshake: now.Add(-5 * time.Second)},
		ok:     true,
	}
	onDemand := &fakeLiveHealther{ok: false}
	liveHealth := handlers.NewLiveHealthModel(streaming, onDemand)

	opts := router.Options{
		Addr:                "127.0.0.1:0",
		ShutdownTimeout:     5 * time.Second,
		Cert:                cert,
		Tokens:              tokens,
		LiveHealth:          liveHealth,
		TunnelCatalog:       fakeCatalog{},
		ZoneChecker:         fakeZoneChecker{},
		ZoneRouter:          fakeZoneRouter{},
		MaxRequestBodyBytes: 10 * 1024 * 1024,
		UpstreamTimeout:     30 * time.Second,
		MaxUpstreamTimeout:  5 * time.Minute,
		HealthMaxAge:        180 * time.Second,
	}
	for _, m := range mutate {
		m(&opts)
	}
	srv := router.New(opts, slog.New(slog.DiscardHandler))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.StartOn(ln) }()

	addr := ln.Addr().String()

	var client *http.Client
	var baseURL string
	if tlsEnabled {
		// insecure client: the cert is self-signed. DisableKeepAlives ensures
		// connections are not pooled so http.Server.Shutdown returns immediately
		// without waiting for idle keep-alive connections to expire.
		client = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test cert
				DisableKeepAlives: true,
			},
		}
		baseURL = "https://" + addr
		require.Eventually(t, func() bool {
			c, dialErr := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
			if dialErr != nil {
				return false
			}
			_ = c.Close()
			return true
		}, 3*time.Second, 10*time.Millisecond, "API server never started accepting TLS connections")
	} else {
		// plain-HTTP client — no TLS config needed.
		client = &http.Client{
			Transport: &http.Transport{DisableKeepAlives: true},
		}
		baseURL = "http://" + addr
		require.Eventually(t, func() bool {
			conn, dialErr := net.Dial("tcp", addr)
			if dialErr != nil {
				return false
			}
			_ = conn.Close()
			return true
		}, 3*time.Second, 10*time.Millisecond, "API server never started accepting plain-TCP connections")
	}

	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// use assert (not require) so a shutdown error does not panic inside cleanup.
		assert.NoError(t, srv.Shutdown(ctx))
		select {
		case e := <-errCh:
			assert.NoError(t, e)
		case <-time.After(3 * time.Second):
			t.Error("API server did not stop within timeout")
		}
	}

	return &serverFixture{
		client:     client,
		baseURL:    baseURL,
		adminToken: string(adminPlain),
		userToken:  string(userPlain),
		shutdown:   shutdown,
	}
}

// startServer builds a complete server fixture with TLS enabled. Thin wrapper
// around startServerMode so existing tests compile unchanged.
func startServer(t *testing.T) *serverFixture {
	t.Helper()
	return startServerMode(t, true)
}

// get is a helper that issues a GET request with an optional token header.
func (f *serverFixture) get(t *testing.T, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.baseURL+path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("X-Vpntunnel-Token", token)
	}
	resp, err := f.client.Do(req)
	require.NoError(t, err)
	return resp
}

// post is a helper that issues a POST request with an optional token header
// and no body.
func (f *serverFixture) post(t *testing.T, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.baseURL+path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("X-Vpntunnel-Token", token)
	}
	resp, err := f.client.Do(req)
	require.NoError(t, err)
	return resp
}

// drainAndClose reads and closes the response body.
func drainAndClose(t *testing.T, resp *http.Response) {
	t.Helper()
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
}

// bodyJSON reads resp.Body into a map[string]string. Closes the body.
func bodyJSON(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var m map[string]string
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

var uuidv7Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestServer_Routing(t *testing.T) {
	t.Parallel()

	f := startServer(t)
	// t.Cleanup runs AFTER all parallel subtests complete, preventing the server
	// from shutting down while subtests are still making requests.
	t.Cleanup(f.shutdown)

	t.Run("tunnels_admin_token_returns_200_with_list", func(t *testing.T) {
		t.Parallel()
		resp := f.get(t, "/v1/tunnels", f.adminToken)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json"),
			"Content-Type must be application/json, got %q", resp.Header.Get("Content-Type"))
		var body map[string][]string
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		// fakeCatalog has se + de entries.
		assert.Contains(t, body, "se")
		assert.Contains(t, body, "de")
	})

	t.Run("tunnels_proxy_token_returns_200_with_list", func(t *testing.T) {
		t.Parallel()
		resp := f.get(t, "/v1/tunnels", f.userToken)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json"),
			"Content-Type must be application/json, got %q", resp.Header.Get("Content-Type"))
		var body map[string][]string
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		// user token is now allowed on /v1/tunnels (user or admin).
		assert.Contains(t, body, "se")
		assert.Contains(t, body, "de")
	})

	t.Run("tunnels_no_token_returns_401_unauthorized", func(t *testing.T) {
		t.Parallel()
		resp := f.get(t, "/v1/tunnels", "")
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Equal(t, "unauthorized", resp.Header.Get("X-Proxy-Error"))
	})

	t.Run("tunnels_invalid_token_returns_401_unauthorized", func(t *testing.T) {
		t.Parallel()
		resp := f.get(t, "/v1/tunnels", strings.Repeat("x", 64))
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Equal(t, "unauthorized", resp.Header.Get("X-Proxy-Error"))
	})

	t.Run("health_admin_token_returns_200_ok", func(t *testing.T) {
		t.Parallel()
		resp := f.get(t, "/v1/admin/health", f.adminToken)
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("health_proxy_token_returns_403_forbidden", func(t *testing.T) {
		t.Parallel()
		// /v1/admin/health is admin-only; user token must be rejected.
		resp := f.get(t, "/v1/admin/health", f.userToken)
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		assert.Equal(t, "forbidden", resp.Header.Get("X-Proxy-Error"))
	})

	t.Run("health_no_token_returns_401_unauthorized", func(t *testing.T) {
		t.Parallel()
		resp := f.get(t, "/v1/admin/health", "")
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Equal(t, "unauthorized", resp.Header.Get("X-Proxy-Error"))
	})

	t.Run("health_invalid_token_returns_401_unauthorized", func(t *testing.T) {
		t.Parallel()
		resp := f.get(t, "/v1/admin/health", strings.Repeat("x", 64))
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Equal(t, "unauthorized", resp.Header.Get("X-Proxy-Error"))
	})

	t.Run("proxy_admin_token_returns_allowed", func(t *testing.T) {
		t.Parallel()
		// admin is in the {user, admin} allow-list. fakeZoneChecker accepts any id;
		// fakeZoneRouter always returns an error, so the handler returns 503
		// tunnel_unavailable — proving auth passed and zone routing ran.
		resp := f.get(t, "/v1/tunnels/se-sto-wg-001-hmac/proxy/https/example.com", f.adminToken)
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
			"admin/user token must pass auth and reach zone routing")
	})

	t.Run("proxy_proxy_token_returns_allowed", func(t *testing.T) {
		t.Parallel()
		// user token is in the {user, admin} allow-list. fakeZoneChecker accepts any id;
		// fakeZoneRouter always returns an error, so the handler returns 503
		// tunnel_unavailable — proving auth passed and zone routing ran.
		resp := f.get(t, "/v1/tunnels/se-sto-wg-001-hmac/proxy/https/example.com", f.userToken)
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
			"admin/user token must pass auth and reach zone routing")
	})

	t.Run("proxy_empty_id_segment_returns_404", func(t *testing.T) {
		t.Parallel()
		// /v1/tunnels//proxy/... has an empty segment — the mux pattern does not match
		// (empty wildcard fails), so the catch-all returns 404 bad_request.
		resp := f.get(t, "/v1/tunnels//proxy/https/example.com", f.userToken)
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		assert.Equal(t, "bad_request", resp.Header.Get("X-Proxy-Error"))
	})

	t.Run("old_proxy_path_returns_404", func(t *testing.T) {
		t.Parallel()
		// the retired /v1/proxy/{scheme}/{rest...} path must no longer be registered.
		resp := f.get(t, "/v1/proxy/https/example.com", f.userToken)
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		assert.Equal(t, "bad_request", resp.Header.Get("X-Proxy-Error"))
	})

	t.Run("unknown_route_returns_404_bad_request_envelope", func(t *testing.T) {
		t.Parallel()
		resp := f.get(t, "/v1/no-such-route", f.adminToken)
		defer drainAndClose(t, resp)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		assert.Equal(t, "bad_request", resp.Header.Get("X-Proxy-Error"))
	})

	t.Run("request_id_header_present_on_every_response", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			path  string
			token string
		}{
			{"/v1/tunnels", f.adminToken},
			{"/v1/tunnels", f.userToken},
			{"/v1/tunnels", ""},
			{"/v1/admin/health", f.adminToken},
			{"/v1/tunnels/se-sto-wg-001-hmac/proxy/https/example.com", f.userToken},
			{"/v1/tunnels/se-sto-wg-001-hmac/proxy/https/example.com", f.adminToken},
			{"/not-a-route", f.adminToken},
		}
		for _, tc := range cases {
			resp := f.get(t, tc.path, tc.token)
			drainAndClose(t, resp)
			id := resp.Header.Get("X-Request-Id")
			assert.Regexp(t, uuidv7Pattern, id,
				"X-Request-Id must be UUIDv7 for path=%s token=%q; got %q", tc.path, tc.token, id)
		}
	})

	t.Run("error_envelope_includes_request_id_and_x_proxy_error", func(t *testing.T) {
		t.Parallel()
		resp := f.get(t, "/v1/tunnels", "") // no token → 401
		m := bodyJSON(t, resp)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Equal(t, "unauthorized", resp.Header.Get("X-Proxy-Error"))
		assert.Equal(t, "unauthorized", m["error"])
		reqID := m["request_id"]
		assert.Regexp(t, uuidv7Pattern, reqID, "request_id in JSON body must be UUIDv7")
		assert.Equal(t, resp.Header.Get("X-Request-Id"), reqID, "JSON request_id must match X-Request-Id header")
	})
}

func TestServer_TLS(t *testing.T) {
	t.Parallel()

	f := startServer(t)
	t.Cleanup(f.shutdown)

	t.Run("tls12_handshake_refused", func(t *testing.T) {
		t.Parallel()
		addr := strings.TrimPrefix(f.baseURL, "https://")
		_, err := tls.Dial("tcp", addr, &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // test-only, checking TLS version rejection
			MaxVersion:         tls.VersionTLS12,
		})
		assert.Error(t, err, "expected TLS 1.2 handshake to be refused")
	})

	t.Run("tls13_handshake_succeeds", func(t *testing.T) {
		t.Parallel()
		addr := strings.TrimPrefix(f.baseURL, "https://")
		conn, err := tls.Dial("tcp", addr, &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // test-only self-signed cert
			MinVersion:         tls.VersionTLS13,
		})
		require.NoError(t, err, "TLS 1.3 handshake must succeed")
		_ = conn.Close()
	})
}

// TestServer_HTTPMode asserts that the server operates correctly in plain-HTTP
// mode (Options.Cert == nil): requests succeed over http://, TLS dials fail,
// and the startup log carries tls=false.
func TestServer_HTTPMode(t *testing.T) {
	t.Parallel()

	t.Run("plain http request succeeds when cert nil", func(t *testing.T) {
		t.Parallel()
		f := startServerMode(t, false)
		t.Cleanup(f.shutdown)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.baseURL+"/v1/admin/health", nil)
		require.NoError(t, err)
		req.Header.Set("X-Vpntunnel-Token", f.adminToken)

		resp, err := f.client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode)

		// confirm the body contains the expected top-level keys.
		var body map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		assert.Contains(t, body, "status")
		assert.Contains(t, body, "tunnels")
	})

	t.Run("tls dial fails when cert nil", func(t *testing.T) {
		t.Parallel()
		f := startServerMode(t, false)
		t.Cleanup(f.shutdown)

		addr := strings.TrimPrefix(f.baseURL, "http://")
		// a TLS dial against a plain-HTTP listener must fail at the record layer;
		// the exact error message is not contractual, so we assert only err != nil.
		conn, err := tls.DialWithDialer(
			&net.Dialer{Timeout: 1 * time.Second},
			"tcp",
			addr,
			&tls.Config{InsecureSkipVerify: true}, //nolint:gosec // testing failure, not security
		)
		if conn != nil {
			_ = conn.Close()
		}
		assert.Error(t, err, "tls dial against a plain-HTTP listener must fail")
	})

	t.Run("listening log reports tls false", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		mk := func(b byte) []byte {
			s := make([]byte, 64)
			for i := range s {
				s[i] = b + byte(i%26)
			}
			return s
		}
		adminPlain := mk('a')
		userPlain := mk('0')
		writeTokenFn := func(name string, content []byte) string {
			p := filepath.Join(dir, name)
			require.NoError(t, os.WriteFile(p, content, 0o600))
			return p
		}
		apiAuth := config.APIAuth{
			AdminTokenFile: writeTokenFn("admin.token", adminPlain),
			ProxyTokenFile: writeTokenFn("user.token", userPlain),
		}
		tokens, err := middleware.LoadTokens(apiAuth, dir)
		require.NoError(t, err)

		streaming := &fakeLiveHealther{
			health: domain.TunnelHealth{ID: "se-sto-wg-001", LastHandshake: time.Now().Add(-5 * time.Second)},
			ok:     true,
		}
		onDemand := &fakeLiveHealther{ok: false}
		liveHealth := handlers.NewLiveHealthModel(streaming, onDemand)

		var mu sync.Mutex
		var logBuf bytes.Buffer
		sw := &syncWriter{mu: &mu, buf: &logBuf}
		captureLog := slog.New(slog.NewTextHandler(sw, &slog.HandlerOptions{Level: slog.LevelInfo}))

		opts := router.Options{
			Addr:                "127.0.0.1:0",
			ShutdownTimeout:     5 * time.Second,
			Cert:                nil, // HTTP mode
			Tokens:              tokens,
			LiveHealth:          liveHealth,
			TunnelCatalog:       fakeCatalog{},
			ZoneChecker:         fakeZoneChecker{},
			ZoneRouter:          fakeZoneRouter{},
			MaxRequestBodyBytes: 10 * 1024 * 1024,
			UpstreamTimeout:     30 * time.Second,
			MaxUpstreamTimeout:  5 * time.Minute,
			HealthMaxAge:        180 * time.Second,
		}
		srv := router.New(opts, captureLog)

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)

		errCh := make(chan error, 1)
		go func() { errCh <- srv.StartOn(ln) }()

		// synchronize on the log line itself, not on dial-ability: ln is already
		// open before StartOn runs, so a successful dial does not prove the
		// "listening" log line has been emitted yet (flaky on loaded runners — the
		// dial races the StartOn goroutine writing to the buffer).
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return strings.Contains(logBuf.String(), "tls=false")
		}, 3*time.Second, 10*time.Millisecond,
			"startup log must report tls=false in HTTP mode")

		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(shutCtx))
		select {
		case e := <-errCh:
			assert.NoError(t, e)
		case <-time.After(3 * time.Second):
			t.Error("API server did not stop within timeout")
		}
	})
}

// authServerFixture is a variant of serverFixture that captures log output
// for asserting that token values are never logged.
type authServerFixture struct {
	client     *http.Client
	baseURL    string
	adminPlain string
	logBuf     *bytes.Buffer
	logBufMu   *sync.Mutex
	shutdown   func()
}

// startAuthServer creates an isolated server with a capturing log handler.
// Each call produces its own token files and log buffer so tests don't share mutable state.
func startAuthServer(t *testing.T) *authServerFixture {
	t.Helper()

	dir := t.TempDir()
	mk := func(b byte) []byte {
		s := make([]byte, 64)
		for i := range s {
			s[i] = b + byte(i%26)
		}
		return s
	}
	adminPlain := mk('a')
	userPlain := mk('0')

	writeToken := func(name string, content []byte) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, content, 0o600))
		require.NoError(t, os.Chmod(p, 0o600))
		return p
	}

	adminFile := writeToken("admin.token", adminPlain)
	userFile := writeToken("user.token", userPlain)

	apiAuth := config.APIAuth{
		AdminTokenFile: adminFile,
		ProxyTokenFile: userFile,
	}

	tokens, err := middleware.LoadTokens(apiAuth, dir)
	require.NoError(t, err)

	certDir := filepath.Join(dir, "tls")
	cert, _, err := apitls.LoadOrGenerate(certDir, "localhost", nil, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	// fake live health: streaming reports one healthy tunnel; on-demand idle.
	streaming := &fakeLiveHealther{
		health: domain.TunnelHealth{ID: "se-sto-wg-001", LastHandshake: time.Now().Add(-5 * time.Second)},
		ok:     true,
	}
	onDemand := &fakeLiveHealther{ok: false}
	liveHealth := handlers.NewLiveHealthModel(streaming, onDemand)

	// syncBuf wraps bytes.Buffer with a mutex so the slog handler and the
	// test's read call don't race.
	var mu sync.Mutex
	var logBuf bytes.Buffer
	syncWriter := &syncWriter{mu: &mu, buf: &logBuf}
	captureLog := slog.New(slog.NewTextHandler(syncWriter, &slog.HandlerOptions{Level: slog.LevelDebug}))

	opts := router.Options{
		Addr:                "127.0.0.1:0",
		ShutdownTimeout:     5 * time.Second,
		Cert:                cert,
		Tokens:              tokens,
		LiveHealth:          liveHealth,
		TunnelCatalog:       fakeCatalog{},
		ZoneChecker:         fakeZoneChecker{},
		ZoneRouter:          fakeZoneRouter{},
		MaxRequestBodyBytes: 10 * 1024 * 1024,
		UpstreamTimeout:     30 * time.Second,
		MaxUpstreamTimeout:  5 * time.Minute,
		HealthMaxAge:        180 * time.Second,
	}
	srv := router.New(opts, captureLog)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.StartOn(ln) }()

	tlsClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			DisableKeepAlives: true,
		},
	}

	addr := ln.Addr().String()
	require.Eventually(t, func() bool {
		c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}, 3*time.Second, 10*time.Millisecond)

	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		assert.NoError(t, srv.Shutdown(ctx))
		select {
		case e := <-errCh:
			assert.NoError(t, e)
		case <-time.After(3 * time.Second):
			t.Error("API server did not stop within timeout")
		}
	}

	return &authServerFixture{
		client:     tlsClient,
		baseURL:    "https://" + addr,
		adminPlain: string(adminPlain),
		logBuf:     &logBuf,
		logBufMu:   &mu,
		shutdown:   shutdown,
	}
}

// syncWriter is an io.Writer that guards a bytes.Buffer with a mutex so that
// concurrent log writes from the server goroutine and reads from the test don't race.
type syncWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// loggedSoFar returns a snapshot of all log output captured so far.
func (f *authServerFixture) loggedSoFar() string {
	f.logBufMu.Lock()
	defer f.logBufMu.Unlock()
	return f.logBuf.String()
}

func TestServer_AuthMiddleware(t *testing.T) {
	t.Parallel()

	// subtests are sequential (no t.Parallel()) because each creates its own
	// server; there is no shared mutable state between subtests.
	t.Run("valid_token_not_logged", func(t *testing.T) {
		f := startAuthServer(t)
		t.Cleanup(f.shutdown)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.baseURL+"/v1/tunnels", nil)
		require.NoError(t, err)
		req.Header.Set("X-Vpntunnel-Token", f.adminPlain)

		resp, err := f.client.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		require.NoError(t, resp.Body.Close())

		assert.NotContains(t, f.loggedSoFar(), f.adminPlain,
			"the admin token plaintext must never appear in log output")
	})

	t.Run("invalid_token_not_logged", func(t *testing.T) {
		f := startAuthServer(t)
		t.Cleanup(f.shutdown)

		// a chosen bad token value that we can search for in logs.
		badToken := strings.Repeat("z", 64)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.baseURL+"/v1/tunnels", nil)
		require.NoError(t, err)
		req.Header.Set("X-Vpntunnel-Token", badToken)

		resp, err := f.client.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		require.NoError(t, resp.Body.Close())

		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.NotContains(t, f.loggedSoFar(), badToken,
			"the rejected token plaintext must never appear in log output")
	})
}
