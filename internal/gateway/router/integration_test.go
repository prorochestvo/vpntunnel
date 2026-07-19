package router_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/application/asyncjob"
	"vpntunnel/internal/application/lazy"
	"vpntunnel/internal/domain"
	"vpntunnel/internal/egress"
	"vpntunnel/internal/gateway/httpV1/handlers"
	"vpntunnel/internal/gateway/router"
	"vpntunnel/internal/gateway/router/apitls"
	"vpntunnel/internal/infrastructure/config"
	"vpntunnel/internal/infrastructure/observability"
)

// compile-time interface assertions for integration-test fakes.
var (
	_ asyncjob.Forwarder    = directForwarder{}
	_ asyncjob.Forwarder    = (*blockingForwarder)(nil)
	_ handlers.Forwarder    = directSyncForwarder{}
	_ handlers.Forwarder    = tlsTrustingForwarder{}
	_ handlers.RawForwarder = tlsTrustingRawForwarder{}

	_ handlers.Router = (*fakeWorkingRouter)(nil)
)

// fakeWorkingDialer satisfies egress.Dialer + egress.Resolver so that
// fakeWorkingRouter can return a live dialer without constructing a real WireGuard
// device. DialContext and LookupHost are never actually called in integration tests
// because the injected ProxyForwarder / asyncjob.Forwarder bypasses WireGuard.
type fakeWorkingDialer struct{}

func (fakeWorkingDialer) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return nil, fmt.Errorf("fakeWorkingDialer: DialContext not used in integration tests")
}

func (fakeWorkingDialer) LookupHost(_ context.Context, _ string) ([]netip.Addr, error) {
	// return a dummy address; never dialed in tests because the injected
	// forwarders bypass tunnel transport.
	return []netip.Addr{netip.MustParseAddr("198.51.100.1")}, nil
}

// fakeWorkingRouter always succeeds and returns a fakeWorkingDialer as both
// dialer and resolver. directSyncForwarder ignores both, so this is safe.
type fakeWorkingRouter struct{}

func (fakeWorkingRouter) Route(_ context.Context, _ string) (egress.Dialer, egress.Resolver, func(), error) {
	return fakeWorkingDialer{}, fakeWorkingDialer{}, func() {}, nil
}

// directForwarder is an asyncjob.Forwarder that executes the cloned request
// directly over http.DefaultTransport (no WireGuard, no IP deny-list). Used
// for async tests whose upstream is an httptest.Server on loopback.
type directForwarder struct{}

func (directForwarder) Forward(ctx context.Context, req *http.Request) (asyncjob.UpstreamResponse, error) {
	client := &http.Client{Transport: http.DefaultTransport}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return asyncjob.UpstreamResponse{}, fmt.Errorf("directForwarder: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return asyncjob.UpstreamResponse{}, fmt.Errorf("directForwarder: read body: %w", err)
	}
	return asyncjob.UpstreamResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
		Body:       body,
	}, nil
}

// blockingForwarder blocks until release is closed OR the pool's internal
// context is cancelled. In practice pool.Shutdown→p.cancel() unblocks workers
// via ctx.Done() before t.Cleanup fires close(release); the release channel
// is a secondary escape hatch.
type blockingForwarder struct {
	release chan struct{}
}

func (f *blockingForwarder) Forward(ctx context.Context, _ *http.Request) (asyncjob.UpstreamResponse, error) {
	select {
	case <-f.release:
		return asyncjob.UpstreamResponse{StatusCode: http.StatusOK, Body: []byte("unblocked")}, nil
	case <-ctx.Done():
		return asyncjob.UpstreamResponse{}, ctx.Err()
	}
}

// directSyncForwarder is a handlers.Forwarder that forwards to the upstream
// directly over http.DefaultTransport, bypassing the WireGuard dialer and the
// IP deny-list. Injected via router.Options.ProxyForwarder so the sync
// /v1/proxy/ path can reach loopback upstreams in integration tests.
type directSyncForwarder struct{}

func (directSyncForwarder) Forward(
	w http.ResponseWriter,
	r *http.Request,
	_ string,
	_ egress.Dialer,
	_ egress.Resolver,
) error {
	client := &http.Client{
		Transport:     http.DefaultTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return nil
	}
	defer resp.Body.Close()
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return nil
}

// tlsTrustingForwarder is a handlers.Forwarder that forwards over an injected
// http.RoundTripper. It is used for the sync proxy path when the upstream is an
// httptest.NewTLSServer (self-signed cert); InsecureSkipVerify is set on the
// injected transport only — forwarder.go is never touched.
type tlsTrustingForwarder struct{ rt http.RoundTripper }

func (f tlsTrustingForwarder) Forward(
	w http.ResponseWriter,
	r *http.Request,
	_ string,
	_ egress.Dialer,
	_ egress.Resolver,
) error {
	client := &http.Client{
		Transport:     f.rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return nil
}

// tlsTrustingRawForwarder is a handlers.RawForwarder that forwards over an
// injected http.RoundTripper. It is the base for Wave 6 async-path cells where
// the upstream is an httptest.NewTLSServer; it is wrapped in a
// ZoneRoutingForwarder before being passed to asyncjob.Pool so the matrix
// exercises the production async path (header-strip + zone-routing), not a
// bare forwarder that bypasses ZoneRoutingForwarder.
type tlsTrustingRawForwarder struct{ rt http.RoundTripper }

func (f tlsTrustingRawForwarder) ForwardRaw(
	ctx context.Context,
	req *http.Request,
	_ string,
	_ egress.Dialer,
	_ egress.Resolver,
) (asyncjob.UpstreamResponse, error) {
	client := &http.Client{Transport: f.rt}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return asyncjob.UpstreamResponse{}, fmt.Errorf("tlsTrustingRawForwarder: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return asyncjob.UpstreamResponse{}, fmt.Errorf("tlsTrustingRawForwarder: read body: %w", err)
	}
	return asyncjob.UpstreamResponse{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: body}, nil
}

// insecureRoundTripper returns a transport that skips TLS certificate
// verification. Used in tests to reach httptest.NewTLSServer upstreams that
// use a per-server self-signed cert.
func insecureRoundTripper() http.RoundTripper {
	return &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only self-signed cert
	}
}

// newUpstream starts a test HTTP or HTTPS server with the given handler.
// tlsEnabled == false → httptest.NewServer; true → httptest.NewTLSServer.
// The server is registered for cleanup; callers do not need to call Close.
func newUpstream(t *testing.T, tlsEnabled bool, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	if tlsEnabled {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv
}

// modeName returns "http" or "https" for use in subtest names.
func modeName(httpMode bool) string {
	if httpMode {
		return "http"
	}
	return "https"
}

// integrationDaemon holds a running API server with a real async pool and store.
type integrationDaemon struct {
	baseURL   string
	userToken string
	// hmacKey is the fixed 32-byte test HMAC key used to derive tunnel ids for
	// e2e path assertions. Tests compute the expected {id} via
	// domain.TunnelID(daemon.hmacKey, basename).
	hmacKey []byte
	// store is exposed so tests can inspect bbolt state or wait for workers.
	store asyncjob.Store
	// shutdown drains in-flight workers gracefully, then stops the server and
	// closes the store. Safe to call multiple times (idempotent via sync.Once).
	shutdown func()
}

// client returns an http.Client appropriate for d's mode: a TLS-trusting client
// (self-signed cert) when the base URL is https://, otherwise a plain http.Client.
func (d *integrationDaemon) client() *http.Client {
	if strings.HasPrefix(d.baseURL, "https") {
		return intClient()
	}
	return &http.Client{}
}

// integrationDaemonOpts customises the daemon for a specific test scenario.
type integrationDaemonOpts struct {
	// asyncFwd is the asyncjob.Forwarder used by the job pool. When nil,
	// directForwarder{} is used (connects directly via net/http).
	asyncFwd asyncjob.Forwarder
	// maxConcurrent is the job pool semaphore size. Default: 4.
	maxConcurrent int
	// storePath is the bbolt file path. When empty, a new temp file is used.
	// Pass an explicit path to share a store across daemon restarts.
	storePath string
	// access is the optional access logger passed to router.Options.Access.
	access *observability.AccessLogger
	// httpMode, when true, boots the API listener as plain HTTP (Cert: nil).
	// When false (the default), a self-signed TLS cert is generated and the
	// API serves HTTPS. The nil-ness of Options.Cert is the mode discriminator.
	httpMode bool
	// proxyFwd, when non-nil, is used as router.Options.ProxyForwarder
	// instead of directSyncForwarder{}. Allows tests to inject a transport
	// that trusts self-signed upstream certs (e.g. tlsTrustingForwarder).
	proxyFwd handlers.Forwarder
}

// startIntegrationDaemon boots a full API server with a real bbolt store, a real
// asyncjob.Pool, and directSyncForwarder injected as ProxyForwarder so loopback
// upstreams are reachable without WireGuard. The pool uses a fakeFullDialer with
// a fresh handshake so the health gate passes. The returned shutdown function
// must be called exactly once per daemon; tests using daemon restarts must NOT
// add it to t.Cleanup and MUST call it explicitly at the right point.
func startIntegrationDaemon(t *testing.T, opts integrationDaemonOpts) *integrationDaemon {
	t.Helper()

	dir := t.TempDir()

	storePath := opts.storePath
	if storePath == "" {
		storePath = filepath.Join(dir, "async.db")
	}

	store, err := asyncjob.NewStore(storePath)
	require.NoError(t, err, "open bbolt store at %s", storePath)

	// RunRecovery deletes pending records left from a previous daemon run,
	// mirroring the production startup sequence.
	require.NoError(t, asyncjob.RunRecovery(store, slog.New(slog.DiscardHandler)))

	asyncFwd := opts.asyncFwd
	if asyncFwd == nil {
		asyncFwd = directForwarder{}
	}
	maxConcurrent := opts.maxConcurrent
	if maxConcurrent == 0 {
		maxConcurrent = 4
	}
	jobPool := asyncjob.NewPool(store, asyncFwd, maxConcurrent, slog.New(slog.DiscardHandler))

	// write two 64-byte role tokens.
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
		return p
	}
	apiAuth := config.APIAuth{
		AdminTokenFile: writeToken("admin.token", adminPlain),
		ProxyTokenFile: writeToken("user.token", userPlain),
	}
	tokens, err := router.LoadTokens(apiAuth, dir)
	require.NoError(t, err)

	// load or generate a TLS cert only when running in HTTPS mode.
	var cert *tls.Certificate
	if !opts.httpMode {
		certDir := filepath.Join(dir, "tls")
		cert, _, err = apitls.LoadOrGenerate(certDir, "localhost", nil, slog.New(slog.DiscardHandler))
		require.NoError(t, err)
	}

	// build a real EligibleSet (HMAC-keyed) used as both ZoneChecker and TunnelCatalog.
	// A fixed 32-byte test key is used so e2e tests can compute the expected {id} via
	// domain.TunnelID(testHMACKey, basename) without hard-coding a hash.
	testHMACKey := bytes.Repeat([]byte{0x42}, 32)
	tunnelsDir := filepath.Join(dir, "tunnels")
	require.NoError(t, os.MkdirAll(tunnelsDir, 0o700))
	// write a minimal but parseable WireGuard conf so NewFullSet can discover it.
	confContent := "[Interface]\nPrivateKey = 6M3/R+JW0JbIxcpBhGJJdBkobJlp2y1TJVqJEWZfvkE=\nAddress = 10.0.0.1/32\n\n[Peer]\nPublicKey = hiRT7pDuZWF4K7HRuSr5o0wT/T1xyEJRv3z3I71WYAk=\nEndpoint = 185.213.155.1:51820\nAllowedIPs = 0.0.0.0/0\n"
	require.NoError(t, os.WriteFile(filepath.Join(tunnelsDir, "se-sto-wg-001.conf"), []byte(confContent), 0o600))
	fullSet, err := lazy.NewFullSet([]string{filepath.Join(tunnelsDir, "se-sto-wg-001.conf")}, tunnelsDir, testHMACKey)
	require.NoError(t, err)

	// fake live health: streaming reports se-sto-wg-001 healthy; on-demand idle.
	streaming := &fakeLiveHealther{
		health: domain.TunnelHealth{ID: "se-sto-wg-001", LastHandshake: time.Now().Add(-5 * time.Second)},
		ok:     true,
	}
	onDemand := &fakeLiveHealther{ok: false}
	liveHealth := handlers.NewLiveHealthModel(streaming, onDemand)

	// use the injected sync forwarder when provided; fall back to directSyncForwarder.
	proxyFwd := opts.proxyFwd
	if proxyFwd == nil {
		proxyFwd = directSyncForwarder{}
	}

	srvOpts := router.Options{
		Addr:                "127.0.0.1:0",
		ShutdownTimeout:     5 * time.Second,
		Cert:                cert, // nil when httpMode == true
		Tokens:              tokens,
		LiveHealth:          liveHealth,
		TunnelCatalog:       fullSet,
		ZoneChecker:         fullSet,
		ZoneRouter:          fakeWorkingRouter{},
		MaxRequestBodyBytes: 10 * 1024 * 1024,
		UpstreamTimeout:     10 * time.Second,
		MaxUpstreamTimeout:  30 * time.Second,
		HealthMaxAge:        180 * time.Second,
		JobPool:             jobPool,
		ProxyForwarder:      proxyFwd,
		Access:              opts.access,
	}
	srv := router.New(srvOpts, slog.New(slog.DiscardHandler))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.StartOn(ln) }()

	addr := ln.Addr().String()
	if opts.httpMode {
		// plain-HTTP readiness probe: accept a bare TCP connection.
		require.Eventually(t, func() bool {
			conn, dialErr := net.Dial("tcp", addr)
			if dialErr != nil {
				return false
			}
			_ = conn.Close()
			return true
		}, 3*time.Second, 10*time.Millisecond, "API server never accepted plain-TCP connections (HTTP mode)")
	} else {
		require.Eventually(t, func() bool {
			c, dialErr := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
			if dialErr != nil {
				return false
			}
			_ = c.Close()
			return true
		}, 3*time.Second, 10*time.Millisecond, "API server never accepted TLS connections")
	}

	// derive the base URL scheme from the mode so intProxyReq and d.client()
	// build correct request URLs.
	scheme := "https"
	if opts.httpMode {
		scheme = "http"
	}
	baseURL := scheme + "://" + addr

	// shutdownOnce ensures the shutdown function is idempotent even when tests
	// call it manually AND via t.Cleanup.
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			assert.NoError(t, srv.Shutdown(shutCtx))

			poolCtx, poolCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer poolCancel()
			assert.NoError(t, jobPool.Shutdown(poolCtx))

			// close the store; bbolt releases its file lock.
			assert.NoError(t, store.Close())

			select {
			case e := <-errCh:
				assert.NoError(t, e)
			case <-time.After(5 * time.Second):
				t.Error("API server goroutine did not exit within timeout")
			}
		})
	}

	return &integrationDaemon{
		baseURL:   baseURL,
		userToken: string(userPlain),
		hmacKey:   testHMACKey,
		store:     store,
		shutdown:  shutdown,
	}
}

// intProxyReq builds a request to /v1/tunnels/{id}/proxy/{scheme}/{upstream host+path}
// against daemon, pre-populated with the user token. The {id} path segment is the
// HMAC tunnel id derived from the daemon's test key and the "se-sto-wg-001" basename.
// tag is the Proxy-Retry-Tag value; pass "" for the sync (no-tag) path.
func intProxyReq(t *testing.T, daemon *integrationDaemon, method, upstreamURL, tag string) *http.Request {
	t.Helper()
	parsed, err := http.NewRequest(method, upstreamURL, nil)
	require.NoError(t, err)

	scheme := parsed.URL.Scheme
	hostPath := parsed.URL.Host + parsed.URL.Path
	if parsed.URL.RawQuery != "" {
		hostPath += "?" + parsed.URL.RawQuery
	}

	tunnelID := domain.TunnelID(daemon.hmacKey, "se-sto-wg-001")
	target := daemon.baseURL + "/v1/tunnels/" + tunnelID + "/proxy/" + scheme + "/" + hostPath
	req, err := http.NewRequestWithContext(context.Background(), method, target, nil)
	require.NoError(t, err)
	req.Header.Set("X-Vpntunnel-Token", daemon.userToken)
	if tag != "" {
		req.Header.Set("Proxy-Retry-Tag", tag)
	}
	return req
}

// intClient returns an http.Client that accepts self-signed TLS certs.
func intClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test cert
		},
	}
}

// waitForJobCompletion polls the store until tag is no longer in StatusPending.
// Times out after 5 s with a test failure.
func waitForJobCompletion(t *testing.T, store asyncjob.Store, tag string) {
	t.Helper()
	require.Eventually(t, func() bool {
		rec, found, err := store.Get(tag)
		if err != nil || !found {
			return false
		}
		return rec.Status != asyncjob.StatusPending
	}, 5*time.Second, 10*time.Millisecond,
		"async job for tag %q never transitioned out of pending", tag)
}

// TestHarnessHTTPMode is a self-test that anchors the new harness pieces added
// in Wave 5. It proves that the httpMode flag produces a plain-HTTP daemon and
// that the existing HTTPS mode is unaffected, validating the server-mode axis
// before the orthogonality matrix (Wave 6) consumes it.
func TestHarnessHTTPMode(t *testing.T) {
	t.Parallel()

	t.Run("http-mode daemon serves /v1/admin/health over plain HTTP", func(t *testing.T) {
		t.Parallel()
		daemon := startIntegrationDaemon(t, integrationDaemonOpts{httpMode: true})
		t.Cleanup(daemon.shutdown)

		// send a no-token GET to /v1/admin/health — 401 proves the HTTP connection
		// worked without needing to expose admin/deploy tokens from the daemon fixture.
		noTokenReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, daemon.baseURL+"/v1/admin/health", nil)
		require.NoError(t, err)

		resp, err := daemon.client().Do(noTokenReq)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		// 401 proves the HTTP connection succeeded and the API responded.
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		// the scheme in the base URL must be http://, not https://.
		assert.True(t, strings.HasPrefix(daemon.baseURL, "http://"),
			"HTTP-mode daemon baseURL must start with http://, got %s", daemon.baseURL)
	})

	t.Run("https-mode daemon still serves over TLS", func(t *testing.T) {
		t.Parallel()
		daemon := startIntegrationDaemon(t, integrationDaemonOpts{httpMode: false})
		t.Cleanup(daemon.shutdown)

		noTokenReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, daemon.baseURL+"/v1/admin/health", nil)
		require.NoError(t, err)

		resp, err := daemon.client().Do(noTokenReq)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		// 401 proves the HTTPS connection succeeded.
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.True(t, strings.HasPrefix(daemon.baseURL, "https://"),
			"HTTPS-mode daemon baseURL must start with https://, got %s", daemon.baseURL)
	})
}

// TestProxySync asserts that requests without Proxy-Retry-Tag are forwarded
// synchronously and the response is byte-identical to the upstream reply.
func TestProxySync(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	daemon := startIntegrationDaemon(t, integrationDaemonOpts{})
	t.Cleanup(daemon.shutdown)

	t.Run("GET passthrough: no tag returns 200 body identical no Proxy-Async-Status", func(t *testing.T) {
		t.Parallel()
		req := intProxyReq(t, daemon, http.MethodGet, upstream.URL+"/path", "")
		resp, err := intClient().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "ok", string(body))
		assert.Empty(t, resp.Header.Get("Proxy-Async-Status"),
			"sync response must not carry Proxy-Async-Status")
	})
}

// TestProxyAsyncFirstSubmit asserts that the first request carrying a
// Proxy-Retry-Tag returns 202 + Proxy-Async-Status: pending with the correct
// JSON envelope.
func TestProxyAsyncFirstSubmit(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "upstream-body")
	}))
	t.Cleanup(upstream.Close)

	daemon := startIntegrationDaemon(t, integrationDaemonOpts{})
	t.Cleanup(daemon.shutdown)

	t.Run("first submit returns 202 pending", func(t *testing.T) {
		t.Parallel()
		const tag = "validtag-001first"
		req := intProxyReq(t, daemon, http.MethodGet, upstream.URL+"/check", tag)
		resp, err := intClient().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusAccepted, resp.StatusCode)
		assert.Equal(t, "pending", resp.Header.Get("Proxy-Async-Status"))

		var envelope map[string]string
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&envelope))
		assert.Equal(t, "pending", envelope["status"])
		assert.Equal(t, tag, envelope["retry_tag"])
	})
}

// TestProxyAsyncRetryAfterCompletion asserts that a second request for a
// completed tag returns the upstream response verbatim with no
// Proxy-Async-Status header.
func TestProxyAsyncRetryAfterCompletion(t *testing.T) {
	t.Parallel()

	const responseBody = "upstream-data"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Custom", "value")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, responseBody)
	}))
	t.Cleanup(upstream.Close)

	daemon := startIntegrationDaemon(t, integrationDaemonOpts{})
	t.Cleanup(daemon.shutdown)

	t.Run("completed retry returns upstream verbatim", func(t *testing.T) {
		t.Parallel()
		const tag = "validtag-retrycomp"
		client := intClient()

		// first submit → 202 pending.
		req1 := intProxyReq(t, daemon, http.MethodGet, upstream.URL+"/data", tag)
		resp1, err := client.Do(req1)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp1.Body)
		require.NoError(t, resp1.Body.Close())
		assert.Equal(t, http.StatusAccepted, resp1.StatusCode)

		// wait for the worker goroutine to transition the record out of pending.
		waitForJobCompletion(t, daemon.store, tag)

		// retry → upstream response verbatim.
		req2 := intProxyReq(t, daemon, http.MethodGet, upstream.URL+"/data", tag)
		resp2, err := client.Do(req2)
		require.NoError(t, err)
		defer func() { _ = resp2.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp2.StatusCode)
		body, err := io.ReadAll(resp2.Body)
		require.NoError(t, err)
		assert.Equal(t, responseBody, string(body))
		assert.Empty(t, resp2.Header.Get("Proxy-Async-Status"),
			"completed response must not carry Proxy-Async-Status")
	})
}

// TestProxyAsyncTombstone seeds a tombstone record before starting the daemon,
// then asserts that a request for the tag returns 410 + tag_evicted.
func TestProxyAsyncTombstone(t *testing.T) {
	t.Parallel()

	t.Run("pre-seeded tombstone returns 410 tag_evicted", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		storePath := filepath.Join(dir, "async.db")

		// write the tombstone record before the daemon opens the file.
		preSeed, err := asyncjob.NewStore(storePath)
		require.NoError(t, err)
		const tag = "validtag-tombstone1"
		require.NoError(t, preSeed.Put(tag, asyncjob.Record{
			Tag:       tag,
			Status:    asyncjob.StatusTombstone,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
			EvictedAt: time.Now().UTC(),
		}))
		require.NoError(t, preSeed.Close())

		daemon := startIntegrationDaemon(t, integrationDaemonOpts{storePath: storePath})
		t.Cleanup(daemon.shutdown)

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)

		req := intProxyReq(t, daemon, http.MethodGet, upstream.URL+"/", tag)
		resp, err := intClient().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusGone, resp.StatusCode)
		assert.Equal(t, "tag_evicted", resp.Header.Get("X-Proxy-Error"))
	})
}

// TestProxyAsyncQueueFull saturates the pool semaphore with blocking workers,
// then verifies that the next fresh submit returns 503 + queue_full.
func TestProxyAsyncQueueFull(t *testing.T) {
	t.Parallel()

	t.Run("saturated semaphore returns 503 queue_full", func(t *testing.T) {
		t.Parallel()

		const maxConcurrent = 2
		release := make(chan struct{})
		// close release in Cleanup so blocking workers exit and pool.Shutdown drains.
		t.Cleanup(func() { close(release) })

		daemon := startIntegrationDaemon(t, integrationDaemonOpts{
			asyncFwd:      &blockingForwarder{release: release},
			maxConcurrent: maxConcurrent,
		})
		t.Cleanup(daemon.shutdown)

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)

		client := intClient()

		// fill every semaphore slot with a blocking worker.
		for i := range maxConcurrent {
			tag := fmt.Sprintf("validtag-queuefull%02d", i)
			req := intProxyReq(t, daemon, http.MethodGet, upstream.URL+"/block", tag)
			resp, err := client.Do(req)
			require.NoError(t, err, "slot %d submit must succeed", i)
			_, _ = io.Copy(io.Discard, resp.Body)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, http.StatusAccepted, resp.StatusCode, "slot %d", i)
		}

		// all slots occupied: next fresh tag must return queue_full.
		req := intProxyReq(t, daemon, http.MethodGet, upstream.URL+"/block", "validtag-queuefull99")
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Equal(t, "queue_full", resp.Header.Get("X-Proxy-Error"))
	})
}

// TestProxyAsyncRestartDropsInflight simulates a daemon crash by writing a
// StatusPending record directly to the store (as if the crash occurred after
// accepting the job but before the worker completed), then starts the daemon
// against that store and verifies that RunRecovery wipes the pending record so
// a retry returns 202 + pending (fresh-first-submit semantics).
//
// No t.Parallel() — daemon lifecycle is sequential and uses a fixed store path.
func TestProxyAsyncRestartDropsInflight(t *testing.T) {
	t.Run("crash drops pending: retry is fresh submit returning 202", func(t *testing.T) {
		dir := t.TempDir()
		storePath := filepath.Join(dir, "async.db")

		// write a pending record as if a previous process accepted the job but
		// crashed before the worker completed.
		crashed, err := asyncjob.NewStore(storePath)
		require.NoError(t, err)
		const tag = "validtag-restart001"
		require.NoError(t, crashed.Put(tag, asyncjob.Record{
			Tag:       tag,
			Status:    asyncjob.StatusPending,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}))
		require.NoError(t, crashed.Close())

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)

		// start the daemon: RunRecovery (inside startIntegrationDaemon) deletes
		// the pending record left by the "crashed" previous process.
		daemon := startIntegrationDaemon(t, integrationDaemonOpts{
			asyncFwd:  directForwarder{},
			storePath: storePath,
		})
		t.Cleanup(daemon.shutdown)

		// retry the same tag: the pending record was deleted by RunRecovery, so
		// the pool treats this as a brand-new first submit and returns 202 pending.
		req := intProxyReq(t, daemon, http.MethodGet, upstream.URL+"/", tag)
		resp, err := intClient().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusAccepted, resp.StatusCode)
		assert.Equal(t, "pending", resp.Header.Get("Proxy-Async-Status"))
	})
}

// TestProxyAsyncTagIdempotencyAcrossRestart submits a job, waits for completion,
// shuts down, restarts against the same bbolt file, and asserts that a retry
// returns the upstream response verbatim. RunRecovery only removes pending
// records — completed records survive.
//
// No t.Parallel() — daemon lifecycle is sequential.
func TestProxyAsyncTagIdempotencyAcrossRestart(t *testing.T) {
	t.Run("completed record survives restart: retry returns upstream verbatim", func(t *testing.T) {
		dir := t.TempDir()
		storePath := filepath.Join(dir, "async.db")

		const responseBody = "persistent-body"
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, responseBody)
		}))
		t.Cleanup(upstream.Close)

		daemon1 := startIntegrationDaemon(t, integrationDaemonOpts{
			asyncFwd:  directForwarder{},
			storePath: storePath,
		})

		const tag = "validtag-persist001"
		client := intClient()

		// submit and wait until the worker stores the result.
		req1 := intProxyReq(t, daemon1, http.MethodGet, upstream.URL+"/persistent", tag)
		resp1, err := client.Do(req1)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp1.Body)
		require.NoError(t, resp1.Body.Close())
		assert.Equal(t, http.StatusAccepted, resp1.StatusCode)

		waitForJobCompletion(t, daemon1.store, tag)
		daemon1.shutdown()

		// restart: RunRecovery must not touch the completed record.
		daemon2 := startIntegrationDaemon(t, integrationDaemonOpts{
			asyncFwd:  directForwarder{},
			storePath: storePath,
		})
		t.Cleanup(daemon2.shutdown)

		// retry → upstream verbatim, not a fresh 202.
		req2 := intProxyReq(t, daemon2, http.MethodGet, upstream.URL+"/persistent", tag)
		resp2, err := client.Do(req2)
		require.NoError(t, err)
		defer func() { _ = resp2.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp2.StatusCode)
		body, err := io.ReadAll(resp2.Body)
		require.NoError(t, err)
		assert.Equal(t, responseBody, string(body))
		assert.Empty(t, resp2.Header.Get("Proxy-Async-Status"),
			"completed response must not carry Proxy-Async-Status")
	})
}

// TestAccessLogSanitiserStripsToken configures a Telegram-style sanitise
// pattern, sends a request with a bot token in the URL path, and asserts that
// the written access-log JSONL line contains <REDACTED> instead of the raw
// token. Relies on the router's withAccessLog middleware (wired when
// opts.Access is non-nil).
func TestAccessLogSanitiserStripsToken(t *testing.T) {
	t.Parallel()

	t.Run("Telegram bot token in path is redacted in access log", func(t *testing.T) {
		t.Parallel()

		logDir := t.TempDir()
		logPath := filepath.Join(logDir, "access.log")

		sanitizers := []observability.PathSanitizePattern{
			{
				// /bot<numeric-id>:<token>/ → /bot<REDACTED>/
				Regexp:      regexp.MustCompile(`(/bot)[0-9]+:[A-Za-z0-9_-]+(/)`),
				Replacement: `${1}<REDACTED>${2}`,
			},
		}
		accessLog, err := observability.NewAccessLogger(config.AccessLog{
			Path:       logPath,
			MaxSizeMB:  10,
			MaxAgeDays: 1,
			MaxBackups: 1,
			Compress:   false,
		}, nil, sanitizers)
		require.NoError(t, err)
		t.Cleanup(func() { _ = accessLog.Close() })

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "sent")
		}))
		t.Cleanup(upstream.Close)

		daemon := startIntegrationDaemon(t, integrationDaemonOpts{access: accessLog})
		t.Cleanup(daemon.shutdown)

		// request with a raw Telegram bot token in the path.
		req := intProxyReq(t, daemon, http.MethodGet,
			upstream.URL+"/bot12345:abcdefSecret/sendMessage", "")
		resp, err := intClient().Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		// lumberjack writes synchronously; the line is on disk by the time the
		// HTTP response has returned. We close and re-open the logger to flush
		// any internal buffering in the slog JSON handler.
		require.NoError(t, accessLog.Close())

		f, err := os.Open(logPath)
		require.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })

		var targets []string
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			var rec map[string]any
			if jsonErr := json.Unmarshal(scanner.Bytes(), &rec); jsonErr != nil {
				continue
			}
			if v, ok := rec["target"].(string); ok {
				targets = append(targets, v)
			}
		}
		require.NoError(t, scanner.Err())
		require.NotEmpty(t, targets, "access log must contain at least one target line")

		for _, tgt := range targets {
			assert.NotContains(t, tgt, "12345:abcdefSecret",
				"raw bot token must not appear in access log")
			assert.Contains(t, tgt, "<REDACTED>",
				"access log target must contain the <REDACTED> placeholder")
			assert.True(t, strings.HasPrefix(tgt, "/v1/tunnels/"),
				"access log target must be the tunnel proxy path, not the upstream URL: got %q", tgt)
		}
	})
}

// cellResult captures the observed (status, body) for one matrix cell so the
// post-loop equality check can compare all four cells with require.Equal.
type cellResult struct {
	status int
	body   string
}

// TestProxyListenerTargetOrthogonality asserts that the proxy forward logic is
// independent of the API listener protocol. It drives a 2x2 matrix:
//
//	{API server in HTTP mode, API server in HTTPS mode} ×
//	{upstream target is http, upstream target is https}
//
// Both the sync (no Proxy-Retry-Tag) and async (with Proxy-Retry-Tag) paths are
// tested. All four cells must produce byte-identical responses, proving the
// target scheme is derived exclusively from the {scheme} path segment (not from
// r.TLS or the inbound connection protocol). This is the runtime proof of the
// invariant pinned as a comment in proxy.go ServeHTTP / serveAsync.
//
// Async cells wire asyncFwd through a real ZoneRoutingForwarder so the matrix
// exercises the production async path (header-strip + zone-routing), not the
// zone-routing-bypass used by the legacy TestProxyAsync* tests.
func TestProxyListenerTargetOrthogonality(t *testing.T) {
	t.Parallel()

	// upstream handler: returns a fixed body + echo headers so equality is unambiguous.
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Echo-Method", r.Method)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "orthogonal-body")
	})

	t.Run("sync", func(t *testing.T) {
		t.Parallel()

		var results []cellResult
		var resultsMu sync.Mutex

		// drive 4 cells: httpMode ∈ {false, true} × targetTLS ∈ {false, true}.
		for _, httpMode := range []bool{false, true} {
			for _, targetTLS := range []bool{false, true} {
				httpMode, targetTLS := httpMode, targetTLS // capture loop vars
				tgtName := map[bool]string{false: "http", true: "https"}[targetTLS]
				name := fmt.Sprintf("server=%s/target=%s", modeName(httpMode), tgtName)
				t.Run(name, func(t *testing.T) {
					t.Parallel()

					up := newUpstream(t, targetTLS, upstreamHandler)

					// pick the sync forwarder based on the upstream TLS mode.
					var syncFwd handlers.Forwarder
					if targetTLS {
						syncFwd = tlsTrustingForwarder{rt: insecureRoundTripper()}
					} else {
						syncFwd = directSyncForwarder{}
					}

					daemon := startIntegrationDaemon(t, integrationDaemonOpts{
						httpMode: httpMode,
						proxyFwd: syncFwd,
					})
					t.Cleanup(daemon.shutdown)

					req := intProxyReq(t, daemon, http.MethodGet, up.URL+"/p", "")
					resp, err := daemon.client().Do(req)
					require.NoError(t, err)
					defer func() { _ = resp.Body.Close() }()

					body, err := io.ReadAll(resp.Body)
					require.NoError(t, err)

					assert.Equal(t, http.StatusOK, resp.StatusCode)
					assert.Equal(t, "orthogonal-body", string(body))
					assert.Equal(t, "GET", resp.Header.Get("X-Echo-Method"))
					assert.Empty(t, resp.Header.Get("Proxy-Async-Status"),
						"sync response must not carry Proxy-Async-Status")

					resultsMu.Lock()
					results = append(results, cellResult{status: resp.StatusCode, body: string(body)})
					resultsMu.Unlock()
				})
			}
		}

		// after all subtests finish, all four cells must be identical.
		t.Cleanup(func() {
			resultsMu.Lock()
			defer resultsMu.Unlock()
			if len(results) != 4 {
				t.Errorf("expected 4 sync cell results, got %d", len(results))
				return
			}
			for i, r := range results[1:] {
				assert.Equal(t, results[0], r, "sync cell %d differs from cell 0", i+1)
			}
		})
	})

	t.Run("async", func(t *testing.T) {
		t.Parallel()

		var results []cellResult
		var resultsMu sync.Mutex

		for _, httpMode := range []bool{false, true} {
			for _, targetTLS := range []bool{false, true} {
				httpMode, targetTLS := httpMode, targetTLS
				tgtName := map[bool]string{false: "http", true: "https"}[targetTLS]
				name := fmt.Sprintf("server=%s/target=%s", modeName(httpMode), tgtName)
				t.Run(name, func(t *testing.T) {
					t.Parallel()

					up := newUpstream(t, targetTLS, upstreamHandler)

					// pick the base raw forwarder by upstream TLS mode; wrap it in a
					// ZoneRoutingForwarder so the async path exercises the production
					// wiring (header-strip + zone-routing), not the legacy direct inject.
					var baseFwd handlers.RawForwarder
					if targetTLS {
						baseFwd = tlsTrustingRawForwarder{rt: insecureRoundTripper()}
					} else {
						baseFwd = tlsTrustingRawForwarder{rt: http.DefaultTransport}
					}
					asyncFwd := handlers.NewZoneRoutingForwarder(fakeWorkingRouter{}, baseFwd)

					// the sync ProxyForwarder for this daemon must also trust the upstream.
					var syncFwd handlers.Forwarder
					if targetTLS {
						syncFwd = tlsTrustingForwarder{rt: insecureRoundTripper()}
					} else {
						syncFwd = directSyncForwarder{}
					}

					daemon := startIntegrationDaemon(t, integrationDaemonOpts{
						httpMode: httpMode,
						asyncFwd: asyncFwd,
						proxyFwd: syncFwd,
					})
					t.Cleanup(daemon.shutdown)

					// tags must be unique per cell; embed the cell identity.
					// Proxy-Retry-Tag format: 8-128 chars, [a-zA-Z0-9_-].
					tag := fmt.Sprintf("orth-%s-%s-sync01", modeName(httpMode), tgtName)

					// first submit → 202 pending.
					req1 := intProxyReq(t, daemon, http.MethodGet, up.URL+"/p", tag)
					resp1, err := daemon.client().Do(req1)
					require.NoError(t, err)
					_, _ = io.Copy(io.Discard, resp1.Body)
					require.NoError(t, resp1.Body.Close())
					require.Equal(t, http.StatusAccepted, resp1.StatusCode)

					// wait until the worker stores the result.
					waitForJobCompletion(t, daemon.store, tag)

					// retry → upstream response replayed.
					req2 := intProxyReq(t, daemon, http.MethodGet, up.URL+"/p", tag)
					resp2, err := daemon.client().Do(req2)
					require.NoError(t, err)
					defer func() { _ = resp2.Body.Close() }()

					body, err := io.ReadAll(resp2.Body)
					require.NoError(t, err)

					assert.Equal(t, http.StatusOK, resp2.StatusCode)
					assert.Equal(t, "orthogonal-body", string(body))
					assert.Equal(t, "GET", resp2.Header.Get("X-Echo-Method"))
					// absence of Proxy-Async-Status marks the completed replay.
					assert.Empty(t, resp2.Header.Get("Proxy-Async-Status"))

					resultsMu.Lock()
					results = append(results, cellResult{status: resp2.StatusCode, body: string(body)})
					resultsMu.Unlock()
				})
			}
		}

		// post-loop: all four async cells must be identical.
		t.Cleanup(func() {
			resultsMu.Lock()
			defer resultsMu.Unlock()
			if len(results) != 4 {
				t.Errorf("expected 4 async cell results, got %d", len(results))
				return
			}
			for i, r := range results[1:] {
				assert.Equal(t, results[0], r, "async cell %d differs from cell 0", i+1)
			}
		})
	})
}

// TestProxyHMACRouting asserts that the new /v1/tunnels/{id}/proxy/... path works
// end-to-end: the HMAC-derived tunnel id is accepted by the full-set ZoneChecker,
// the proxy handler forwards to the loopback upstream via the injected sync forwarder,
// and a syntactically valid hex id NOT in the set returns 400 unknown_tunnel.
func TestProxyHMACRouting(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "hmac-ok")
	}))
	t.Cleanup(upstream.Close)

	daemon := startIntegrationDaemon(t, integrationDaemonOpts{})
	t.Cleanup(daemon.shutdown)

	t.Run("hmac_id_in_full_set_routes_to_upstream", func(t *testing.T) {
		t.Parallel()
		// compute the id the same way the daemon did — must not hard-code the hash.
		id := domain.TunnelID(daemon.hmacKey, "se-sto-wg-001")
		parsed, err := http.NewRequest(http.MethodGet, upstream.URL+"/check", nil)
		require.NoError(t, err)

		scheme := parsed.URL.Scheme
		hostPath := parsed.URL.Host + parsed.URL.Path
		target := daemon.baseURL + "/v1/tunnels/" + id + "/proxy/" + scheme + "/" + hostPath
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
		require.NoError(t, err)
		req.Header.Set("X-Vpntunnel-Token", daemon.userToken)

		resp, err := daemon.client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "hmac-ok", string(body))
	})

	t.Run("valid_hex_id_not_in_set_returns_400_unknown_tunnel", func(t *testing.T) {
		t.Parallel()
		// a syntactically valid 64-hex-char id that was never seeded into the full set.
		unknownID := strings.Repeat("a", 64)
		target := daemon.baseURL + "/v1/tunnels/" + unknownID + "/proxy/http/" + upstream.Listener.Addr().String() + "/"
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
		require.NoError(t, err)
		req.Header.Set("X-Vpntunnel-Token", daemon.userToken)

		resp, err := daemon.client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "unknown_tunnel", resp.Header.Get("X-Proxy-Error"))
	})
}
