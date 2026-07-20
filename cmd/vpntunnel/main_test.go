// Package main tests the composition root (main.go). Smoke tests in this file
// exercise run end-to-end using fake tunnel dialers so no real WireGuard
// device is needed.
//
// Port allocation trade-off: the fixture config hard-codes proxy=127.0.0.1:17788
// and api=127.0.0.1:18888. These ports are unlikely to be in use on a developer
// machine or CI runner; if they are, the test will fail with "bind: address already
// in use". The alternative — using port 0 and recovering the actual port — requires
// exposing net.Listener from run, which would add non-trivial surface.
// The current approach keeps the production path clean at the cost of a small
// flakiness risk on heavily-loaded machines.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"vpntunnel/internal/application/asyncjob"
	"vpntunnel/internal/application/tunnelpool"
	"vpntunnel/internal/constants"
	"vpntunnel/internal/egress"
	"vpntunnel/internal/infrastructure/config"
)

// withSupervisorBuilder returns a runOpt that injects a fake DeviceBuilderFn
// into the streaming supervisor. Intended for tests only.
func withSupervisorBuilder(b tunnelpool.DeviceBuilderFn) runOpt {
	return func(o *runOptions) { o.supervisorBuilder = b }
}

// withSchedulerBuilder returns a runOpt that injects a fake DeviceBuilderFn
// into the on-demand scheduler. Intended for tests only.
func withSchedulerBuilder(b tunnelpool.DeviceBuilderFn) runOpt {
	return func(o *runOptions) { o.schedulerBuilder = b }
}

// withShutdownCtx returns a runOpt that replaces signal.NotifyContext with the
// caller-owned context as the shutdown trigger. Test-only seam.
func withShutdownCtx(ctx context.Context) runOpt {
	return func(o *runOptions) { o.shutdownCtx = ctx }
}

func TestResolveAuthToken(t *testing.T) {
	t.Parallel()

	t.Run("both empty returns empty string", func(t *testing.T) {
		t.Parallel()
		tok, err := resolveAuthToken(config.Auth{}, "/some/dir")
		require.NoError(t, err)
		assert.Empty(t, tok)
	})

	t.Run("inline token only is returned trimmed", func(t *testing.T) {
		t.Parallel()
		tok, err := resolveAuthToken(config.Auth{Token: "  mytoken  "}, "/some/dir")
		require.NoError(t, err)
		assert.Equal(t, "mytoken", tok)
	})

	t.Run("inline token without padding is returned as-is", func(t *testing.T) {
		t.Parallel()
		tok, err := resolveAuthToken(config.Auth{Token: "mytoken"}, "/some/dir")
		require.NoError(t, err)
		assert.Equal(t, "mytoken", tok)
	})

	t.Run("token_file with relative path is resolved against configDir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tokenPath := filepath.Join(dir, "token.txt")
		require.NoError(t, os.WriteFile(tokenPath, []byte("filetoken\n"), 0o600))

		tok, err := resolveAuthToken(config.Auth{TokenFile: "token.txt"}, dir)
		require.NoError(t, err)
		assert.Equal(t, "filetoken", tok)
	})

	t.Run("token_file with absolute path is used as-is", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tokenPath := filepath.Join(dir, "token.txt")
		require.NoError(t, os.WriteFile(tokenPath, []byte("abstoken\n"), 0o600))

		tok, err := resolveAuthToken(config.Auth{TokenFile: tokenPath}, "/different/dir")
		require.NoError(t, err)
		assert.Equal(t, "abstoken", tok)
	})

	t.Run("token_file with CRLF line ending is trimmed correctly", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tokenPath := filepath.Join(dir, "token.txt")
		require.NoError(t, os.WriteFile(tokenPath, []byte("crlftoken\r\n"), 0o600))

		tok, err := resolveAuthToken(config.Auth{TokenFile: "token.txt"}, dir)
		require.NoError(t, err)
		assert.Equal(t, "crlftoken", tok)
	})

	t.Run("unreadable token_file returns wrapped error", func(t *testing.T) {
		t.Parallel()
		_, err := resolveAuthToken(config.Auth{TokenFile: "nonexistent.txt"}, "/nonexistent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read auth.token_file")
		// error must not contain token contents (there are none, but check shape)
	})

	t.Run("token_file with whitespace-only contents returns error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tokenPath := filepath.Join(dir, "empty.txt")
		require.NoError(t, os.WriteFile(tokenPath, []byte("   \n  \t  \n"), 0o600))

		_, err := resolveAuthToken(config.Auth{TokenFile: "empty.txt"}, dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "token is empty after trim")
		// error must not contain the file contents
		assert.NotContains(t, err.Error(), "   ")
	})

	t.Run("token takes precedence when both set (defensive; config validation prevents this)", func(t *testing.T) {
		t.Parallel()
		// config.Load would have rejected this, but resolveAuthToken should handle it
		// gracefully by preferring the inline token.
		tok, err := resolveAuthToken(config.Auth{Token: "inlinetoken", TokenFile: "somefile.txt"}, "/dir")
		require.NoError(t, err)
		assert.Equal(t, "inlinetoken", tok)
	})
}

func TestParseTLSOptions(t *testing.T) {
	t.Parallel()

	t.Run("empty hostname rejected", func(t *testing.T) {
		t.Parallel()
		_, err := parseTLSOptions("/tmp/tls", "", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "-tls-hostname")
	})

	t.Run("relative cert dir resolves against cwd", func(t *testing.T) {
		t.Parallel()
		cwd, err := os.Getwd()
		require.NoError(t, err)
		opts, err := parseTLSOptions("reltls", "localhost", "")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(cwd, "reltls"), opts.CertDir)
	})

	t.Run("absolute cert dir passed through unchanged", func(t *testing.T) {
		t.Parallel()
		opts, err := parseTLSOptions("/opt/vpntunnel/tls/", "localhost", "")
		require.NoError(t, err)
		assert.Equal(t, "/opt/vpntunnel/tls/", opts.CertDir)
	})

	t.Run("comma-separated ip sans parsed", func(t *testing.T) {
		t.Parallel()
		opts, err := parseTLSOptions("/tmp/tls", "localhost", "127.0.0.1,::1")
		require.NoError(t, err)
		assert.Len(t, opts.IPSANs, 2)
	})

	t.Run("invalid ip rejected", func(t *testing.T) {
		t.Parallel()
		_, err := parseTLSOptions("/tmp/tls", "localhost", "127.0.0.1,not-an-ip")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not-an-ip")
	})

	t.Run("empty ip-sans yields none", func(t *testing.T) {
		t.Parallel()
		opts, err := parseTLSOptions("/tmp/tls", "localhost", "")
		require.NoError(t, err)
		assert.Len(t, opts.IPSANs, 0)
	})

	t.Run("trailing comma tolerated", func(t *testing.T) {
		t.Parallel()
		opts, err := parseTLSOptions("/tmp/tls", "localhost", "127.0.0.1,")
		require.NoError(t, err)
		assert.Len(t, opts.IPSANs, 1)
	})

	t.Run("empty cert dir yields HTTP-mode tlsOptions", func(t *testing.T) {
		t.Parallel()
		// regression guard: filepath.Abs("") returns the cwd, which would
		// silently re-enable HTTPS. The empty branch must preserve CertDir as "".
		got, err := parseTLSOptions("", "localhost", "127.0.0.1")
		require.NoError(t, err)
		assert.Equal(t, "", got.CertDir, "CertDir must stay empty (not resolved to cwd)")
		assert.Equal(t, "localhost", got.Hostname)
		assert.Len(t, got.IPSANs, 1)
	})
}

// compile-time assertions: smokeDialer must satisfy all interfaces the supervisor and scheduler cast to.
var (
	_ egress.DialerCloser   = (*smokeDialer)(nil)
	_ egress.HealthReporter = (*smokeDialer)(nil)
	_ egress.Resolver       = (*smokeDialer)(nil)
)

// smokeDialer is a no-op test double for the full tunnel interface set.
// DialContext always fails (smoke tests do not actually proxy traffic).
// LastHandshake returns a recent timestamp so the health handler reports healthy.
type smokeDialer struct{}

func (smokeDialer) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return nil, fmt.Errorf("smokeDialer: not connected")
}

func (smokeDialer) Close() error { return nil }

func (smokeDialer) LastHandshake() (time.Time, error) {
	return time.Now().Add(-5 * time.Second), nil
}

func (smokeDialer) LookupHost(_ context.Context, _ string) ([]netip.Addr, error) {
	return nil, fmt.Errorf("smokeDialer: DNS not implemented")
}

// smokeBuilder is a tunnelpool.DeviceBuilderFn that returns a smokeDialer for any config path.
func smokeBuilder(_ context.Context, _, _ string, _ *slog.Logger) (egress.DialerCloser, error) {
	return &smokeDialer{}, nil
}

// genWGKey returns a fresh WireGuard private key in base64 string form.
func genWGKey(tb testing.TB) string {
	tb.Helper()
	k, err := wgtypes.GeneratePrivateKey()
	require.NoError(tb, err)
	return k.String()
}

// genWGPubKey returns a fresh WireGuard public key (derived from a fresh
// private key) in base64 string form.
func genWGPubKey(tb testing.TB) string {
	tb.Helper()
	k, err := wgtypes.GeneratePrivateKey()
	require.NoError(tb, err)
	return k.PublicKey().String()
}

// writeToken writes a ≥64-byte deterministic token file at path with mode 0600.
// offset shifts the starting position in the alphabet so callers can produce
// three files with distinct contents (and therefore distinct SHA-512 hashes).
func writeToken(tb testing.TB, path string, offset int) {
	tb.Helper()
	// 80 printable ASCII characters — safely above the 64-byte minimum.
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	buf := make([]byte, 80)
	for i := range buf {
		buf[i] = alphabet[(i+offset)%len(alphabet)]
	}
	require.NoError(tb, os.WriteFile(path, buf, 0o600))
}

// TestRun is the composition-root smoke test. It boots the full
// run pipeline with a fake tunnel pool (no real WireGuard) and asserts:
//  1. Both the proxy and API servers become reachable.
//  2. Context cancellation triggers graceful shutdown (avoids SIGTERM to the
//     whole test process, which would be unsafe if other tests run in parallel).
//  3. run returns nil within a reasonable timeout.
func TestRun(t *testing.T) {
	// not t.Parallel() — binds to fixed ports 17788 / 18888.

	t.Run("boots and shuts down on context cancel", func(t *testing.T) {
		dir := t.TempDir()

		// write a minimal wg-quick .conf the parser will accept into the
		// tunnels/ subdir so DiscoverConfigs picks it up automatically.
		tunnelsDir := filepath.Join(dir, "tunnels")
		require.NoError(t, os.MkdirAll(tunnelsDir, 0o700))
		confContent := fmt.Sprintf("[Interface]\nPrivateKey = %s\nAddress = 10.99.0.1/32\n\n[Peer]\nPublicKey = %s\nEndpoint = 203.0.113.1:51820\n",
			genWGKey(t), genWGPubKey(t))
		require.NoError(t, os.WriteFile(filepath.Join(tunnelsDir, "smoke.conf"), []byte(confContent), 0o600))

		// write three distinct token files.
		authDir := filepath.Join(dir, "auth")
		require.NoError(t, os.MkdirAll(authDir, 0o700))
		userTok := filepath.Join(authDir, "proxy_token")
		adminTok := filepath.Join(authDir, "admin_token")
		// each token uses a different alphabet offset so the two SHA-512 hashes
		// are distinct, satisfying LoadTokens' uniqueness check.
		writeToken(t, userTok, 0)
		writeToken(t, adminTok, 1)

		// create the cert dir (apitls.LoadOrGenerate will generate a cert here).
		certDir := filepath.Join(dir, "tls")
		require.NoError(t, os.MkdirAll(certDir, 0o700))

		// access log goes into the temp dir.
		logDir := filepath.Join(dir, "logs")
		require.NoError(t, os.MkdirAll(logDir, 0o755))

		// write a v6 fixture proxy.json. Tunnels are auto-discovered from
		// <configDir>/tunnels/ so no "configs" field is needed. TLS settings
		// travel via tlsOptions (CLI flags), not the JSON config.
		// vpnstream.auth is omitted (proxy auth disabled); api.auth holds the
		// two API role tokens.
		asyncDBPath := filepath.Join(dir, "async.db")
		cfgJSON := fmt.Sprintf(`{
  "vpnstream": {
    "listen": "127.0.0.1:17788",
    "dial_timeout": "5s",
    "shutdown_timeout": "3s"
  },
  "api": {
    "listen": "127.0.0.1:18888",
    "shutdown_timeout": "2s",
    "auth": {
      "proxy_token_file": %q,
      "admin_token_file":  %q
    },
    "max_request_body_bytes": 1048576,
    "vpn": {
      "timeout":      "5s",
      "max_timeout":  "30s",
      "async": {
        "storage_path": %q
      }
    }
  },
  "access_log": { "path": %q }
}`, userTok, adminTok, asyncDBPath,
			filepath.Join(logDir, "access.log"))

		cfgPath := filepath.Join(dir, "proxy.json")
		require.NoError(t, os.WriteFile(cfgPath, []byte(cfgJSON), 0o644))

		// shutdownCtx is the test-owned cancellation source. Cancelling it drives
		// the same ctx.Done() branch in run as SIGTERM would in production,
		// without risking signal delivery to the whole test process.
		shutdownCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// run in a goroutine; collect the return value.
		done := make(chan error, 1)
		go func() {
			done <- run(cfgPath, tlsOptions{CertDir: certDir, Hostname: "localhost", IPSANs: []net.IP{net.ParseIP("127.0.0.1")}}, withSupervisorBuilder(smokeBuilder), withSchedulerBuilder(smokeBuilder), withShutdownCtx(shutdownCtx))
		}()

		// wait for both servers to become reachable (up to 5 s).
		proxyReady := waitTCP(t, "127.0.0.1:17788", 5*time.Second)
		apiReady := waitHTTPS(t, "https://127.0.0.1:18888/v1/admin/health", 5*time.Second)
		assert.True(t, proxyReady, "proxy listener (127.0.0.1:17788) did not become reachable")
		assert.True(t, apiReady, "api listener (127.0.0.1:18888) did not become reachable")

		// trigger shutdown by cancelling the injected context.
		cancel()

		// assert clean shutdown within 10 s.
		select {
		case err := <-done:
			assert.NoError(t, err, "run returned error on context-cancel shutdown")
		case <-time.After(10 * time.Second):
			t.Fatal("run did not return within 10s after context cancel")
		}
	})

	t.Run("async startup: recovery deletes pending records before listeners start", func(t *testing.T) {
		// not t.Parallel() — binds to fixed ports 17790 / 18890.

		dir := t.TempDir()
		dbPath := filepath.Join(dir, "async.db")

		// seed the database with one pending record before boot.
		store, err := asyncjob.NewStore(dbPath)
		require.NoError(t, err, "create seed store")
		seedRec := asyncjob.NewPendingRecord("seed-tag-001", time.Now())
		require.NoError(t, store.Put("seed-tag-001", seedRec), "write seed record")
		require.NoError(t, store.Close(), "close seed store")

		cfgPath := filepath.Join(dir, "proxy.json")
		certDir := filepath.Join(dir, "tls")
		writeFixtureConfig(t, cfgPath, fixtureConfig{
			proxyAddr:   "127.0.0.1:17790",
			apiAddr:     "127.0.0.1:18890",
			asyncDBPath: dbPath,
			certDir:     certDir,
		})

		shutdownCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() {
			done <- run(cfgPath, tlsOptions{CertDir: certDir, Hostname: "localhost", IPSANs: []net.IP{net.ParseIP("127.0.0.1")}}, withSupervisorBuilder(smokeBuilder), withSchedulerBuilder(smokeBuilder), withShutdownCtx(shutdownCtx))
		}()

		// wait for the API to become reachable (proves boot + recovery completed).
		apiReady := waitHTTPS(t, "https://127.0.0.1:18890/v1/admin/health", 10*time.Second)
		assert.True(t, apiReady, "api listener (127.0.0.1:18890) did not become reachable after async startup")

		// trigger shutdown.
		cancel()

		select {
		case runErr := <-done:
			require.NoError(t, runErr, "run returned an error")
		case <-time.After(15 * time.Second):
			t.Fatal("run did not return within 15s after async startup shutdown")
		}

		// reopen the store and confirm recovery deleted the pending record.
		postStore, err := asyncjob.NewStore(dbPath)
		require.NoError(t, err, "reopen store after shutdown")
		t.Cleanup(func() { _ = postStore.Close() })

		counts, err := postStore.Counts()
		require.NoError(t, err, "counts after shutdown")
		assert.Equal(t, 0, counts.Pending, "recovery should have deleted the seeded pending record")
	})

	t.Run("gc goroutine stops cleanly on context cancel", func(t *testing.T) {
		// not t.Parallel() — binds to fixed ports 17792 / 18892.

		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "proxy.json")
		certDir := filepath.Join(dir, "tls")
		writeFixtureConfig(t, cfgPath, fixtureConfig{
			proxyAddr: "127.0.0.1:17792",
			apiAddr:   "127.0.0.1:18892",
			certDir:   certDir,
		})

		shutdownCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() {
			done <- run(cfgPath, tlsOptions{CertDir: certDir, Hostname: "localhost", IPSANs: []net.IP{net.ParseIP("127.0.0.1")}}, withSupervisorBuilder(smokeBuilder), withSchedulerBuilder(smokeBuilder), withShutdownCtx(shutdownCtx))
		}()

		// wait until the API is up — guarantees GC goroutine has been launched.
		apiReady := waitHTTPS(t, "https://127.0.0.1:18892/v1/admin/health", 10*time.Second)
		assert.True(t, apiReady, "api listener (127.0.0.1:18892) did not become reachable")

		// cancel → GC context fires → GC.Run returns → goroutine exits.
		cancel()

		select {
		case runErr := <-done:
			// clean exit proves the GC goroutine did not block the shutdown path.
			assert.NoError(t, runErr, "run returned an error on GC shutdown")
		case <-time.After(15 * time.Second):
			t.Fatal("run did not return within 15s; GC goroutine may be leaking")
		}
	})

	t.Run("missing async store dir is created on startup", func(t *testing.T) {
		// not t.Parallel() — binds to fixed ports 17794 / 18894.
		// Regression: on a fresh host the async store's parent dir does not exist;
		// bbolt.Open does not create it, so the daemon must MkdirAll it or it
		// crash-loops before any listener binds.

		dir := t.TempDir()
		// nested path whose parent dirs do NOT exist yet.
		asyncDir := filepath.Join(dir, "state", "nested")
		dbPath := filepath.Join(asyncDir, "async.db")
		require.NoDirExists(t, asyncDir, "precondition: async dir must not exist before startup")

		cfgPath := filepath.Join(dir, "proxy.json")
		certDir := filepath.Join(dir, "tls")
		writeFixtureConfig(t, cfgPath, fixtureConfig{
			proxyAddr:   "127.0.0.1:17794",
			apiAddr:     "127.0.0.1:18894",
			asyncDBPath: dbPath,
			certDir:     certDir,
		})

		shutdownCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		go func() {
			done <- run(cfgPath, tlsOptions{CertDir: certDir, Hostname: "localhost", IPSANs: []net.IP{net.ParseIP("127.0.0.1")}}, withSupervisorBuilder(smokeBuilder), withSchedulerBuilder(smokeBuilder), withShutdownCtx(shutdownCtx))
		}()

		// API reachability proves the store opened, which proves the dir was created.
		apiReady := waitHTTPS(t, "https://127.0.0.1:18894/v1/admin/health", 10*time.Second)
		assert.True(t, apiReady, "api listener (127.0.0.1:18894) did not become reachable; async store dir likely not created")
		assert.DirExists(t, asyncDir, "daemon should have created the async store parent dir")

		cancel()

		select {
		case runErr := <-done:
			assert.NoError(t, runErr, "run returned an error")
		case <-time.After(15 * time.Second):
			t.Fatal("run did not return within 15s after shutdown")
		}
	})

	t.Run("http mode (no cert dir): API answers over plain HTTP", func(t *testing.T) {
		// not t.Parallel() — binds to fixed ports 17796 / 18896.
		// Port choice: next available pair after the three TLS-mode subtests
		// above (17788/18888, 17790/18890, 17792/18892).

		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "proxy.json")
		writeFixtureConfig(t, cfgPath, fixtureConfig{
			proxyAddr: "127.0.0.1:17796",
			apiAddr:   "127.0.0.1:18896",
		})

		shutdownCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		// pass an empty CertDir — this is the HTTP-mode trigger. run
		// skips apitls.LoadOrGenerate and emits the no-TLS WARN instead.
		// waitHTTPS to the same port is the negative control (TLS dial to a
		// plain-HTTP listener must fail fast).
		go func() {
			done <- run(cfgPath,
				tlsOptions{CertDir: "", Hostname: "localhost", IPSANs: []net.IP{net.ParseIP("127.0.0.1")}},
				withSupervisorBuilder(smokeBuilder),
				withSchedulerBuilder(smokeBuilder),
				withShutdownCtx(shutdownCtx),
			)
		}()

		// plain-HTTP reachability proves the API is running in HTTP mode.
		httpReady := waitHTTP(t, "http://127.0.0.1:18896/v1/admin/health", 5*time.Second)
		assert.True(t, httpReady, "api listener (127.0.0.1:18896) did not become reachable over plain HTTP")

		// TLS dial to a plain-HTTP listener must fail (negative control).
		// Use a short timeout — the failure is immediate at the record layer.
		// This is the server-mode smoke only; the full 2x2 target-scheme matrix
		// is in Wave 6 (TestProxyListenerTargetOrthogonality).
		tlsReady := waitHTTPS(t, "https://127.0.0.1:18896/v1/admin/health", 1*time.Second)
		assert.False(t, tlsReady, "TLS dial to plain-HTTP listener must fail (server-mode negative control)")

		cancel()

		select {
		case err := <-done:
			assert.NoError(t, err, "run returned error on context-cancel shutdown (HTTP mode)")
		case <-time.After(10 * time.Second):
			t.Fatal("run did not return within 10s after context cancel (HTTP mode)")
		}
	})

	t.Run("https mode with unloadable cert dir fails startup", func(t *testing.T) {
		// not t.Parallel() — binds to fixed ports 17798 / 18898 but returns
		// before any listener starts (startup fails at the cert-load step).
		// Port choice: next available pair after the HTTP-mode smoke above.

		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "proxy.json")
		// the fixture config must be otherwise valid so the pre-cert startup
		// steps succeed (access log, async store, recovery) and the cert-load
		// branch is reached. Poison only tlsOptions.CertDir via wrong perms.
		writeFixtureConfig(t, cfgPath, fixtureConfig{
			proxyAddr: "127.0.0.1:17798",
			apiAddr:   "127.0.0.1:18898",
		})

		// create a dir with wrong permissions (0777 instead of 0700).
		// apitls.ensureCertDir rejects dirs whose permissions are not 0700,
		// giving a deterministic FAIL via a *publicerror.Error (apitls.go:121).
		// os.Chmod must follow os.MkdirAll because MkdirAll honours the umask.
		// NOTE: do NOT call parseTLSOptions — we inject the tlsOptions directly
		// to exercise the Wave 3 cert-load branch inside run in isolation.
		// parseTLSOptions is only invoked from parseFlags (via main) to parse CLI flags.
		poisonDir := filepath.Join(t.TempDir(), "badtls")
		require.NoError(t, os.MkdirAll(poisonDir, 0o777))
		require.NoError(t, os.Chmod(poisonDir, 0o777))

		shutdownCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		err := run(cfgPath,
			tlsOptions{CertDir: poisonDir, Hostname: "localhost", IPSANs: nil},
			withSupervisorBuilder(smokeBuilder),
			withSchedulerBuilder(smokeBuilder),
			withShutdownCtx(shutdownCtx),
		)
		// exercises the FAIL-not-fallback contract via the wrong-perms path
		// (apitls.ensureCertDir returns a *publicerror.Error for non-0700 dirs).
		// the existing-but-unparseable-cert FAIL variant is gated on a sibling
		// apitls change and is not asserted here.
		require.Error(t, err)
		assert.Contains(t, err.Error(), "load tls cert")
	})

	t.Run("startup fails when allowed_countries matches no discovered config", func(t *testing.T) {
		// no port binding — run returns before any listener starts.

		dir := t.TempDir()
		tunnelsDir := filepath.Join(dir, "tunnels")
		require.NoError(t, os.MkdirAll(tunnelsDir, 0o700))
		// write a single conf with a "se-sto" zone that will not match "xx".
		confContent := fmt.Sprintf("[Interface]\nPrivateKey = %s\nAddress = 10.99.0.2/32\n\n[Peer]\nPublicKey = %s\nEndpoint = 203.0.113.2:51820\n",
			genWGKey(t), genWGPubKey(t))
		require.NoError(t, os.WriteFile(filepath.Join(tunnelsDir, "mullvad-se-sto-wg-001.conf"), []byte(confContent), 0o600))

		authDir := filepath.Join(dir, "auth")
		require.NoError(t, os.MkdirAll(authDir, 0o700))
		userTok := filepath.Join(authDir, "proxy_token")
		adminTok := filepath.Join(authDir, "admin_token")
		writeToken(t, userTok, 6)
		writeToken(t, adminTok, 7)

		certDir := filepath.Join(dir, "tls")
		require.NoError(t, os.MkdirAll(certDir, 0o700))
		logDir := filepath.Join(dir, "logs")
		require.NoError(t, os.MkdirAll(logDir, 0o755))

		cfgJSON := fmt.Sprintf(`{
  "vpnstream": {
    "listen": "127.0.0.1:17794",
    "allowed_countries": ["xx"],
    "dial_timeout": "5s",
    "shutdown_timeout": "3s"
  },
  "api": {
    "listen": "127.0.0.1:18894",
    "shutdown_timeout": "2s",
    "auth": {
      "proxy_token_file": %q,
      "admin_token_file":  %q
    },
    "max_request_body_bytes": 1048576,
    "vpn": {
      "timeout": "5s",
      "max_timeout": "30s",
      "async": { "storage_path": %q }
    }
  },
  "access_log": { "path": %q }
}`,
			userTok, adminTok,
			filepath.Join(dir, "async.db"),
			filepath.Join(logDir, "access.log"),
		)
		cfgPath := filepath.Join(dir, "proxy.json")
		require.NoError(t, os.WriteFile(cfgPath, []byte(cfgJSON), 0o644))

		shutdownCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		err := run(cfgPath, tlsOptions{CertDir: certDir, Hostname: "localhost", IPSANs: []net.IP{net.ParseIP("127.0.0.1")}},
			withSupervisorBuilder(smokeBuilder), withSchedulerBuilder(smokeBuilder), withShutdownCtx(shutdownCtx))
		require.Error(t, err, "expected startup error when allowed_countries matches no config")
		assert.Contains(t, err.Error(), "streaming tunnel set",
			"error must identify the streaming tunnel set as the failing component")
	})

	t.Run("telegram notifier disabled when VPNTUNNEL_TELEGRAMBOT_DSN is unset", func(t *testing.T) {
		// not t.Parallel() — binds to fixed ports 17800 / 18900 and, via
		// captureStdout, temporarily swaps the process-wide os.Stdout; both
		// require this subtest to run in isolation from parallel siblings.
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "proxy.json")
		certDir := filepath.Join(dir, "tls")
		writeFixtureConfig(t, cfgPath, fixtureConfig{
			proxyAddr: "127.0.0.1:17800",
			apiAddr:   "127.0.0.1:18900",
			certDir:   certDir,
		})

		shutdownCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		stdout := captureStdout(t, func() {
			go func() {
				done <- run(cfgPath, tlsOptions{CertDir: certDir, Hostname: "localhost", IPSANs: []net.IP{net.ParseIP("127.0.0.1")}}, withSupervisorBuilder(smokeBuilder), withSchedulerBuilder(smokeBuilder), withShutdownCtx(shutdownCtx))
			}()

			apiReady := waitHTTPS(t, "https://127.0.0.1:18900/v1/admin/health", 10*time.Second)
			assert.True(t, apiReady, "api listener (127.0.0.1:18900) did not become reachable")

			cancel()

			select {
			case runErr := <-done:
				assert.NoError(t, runErr)
			case <-time.After(15 * time.Second):
				t.Fatal("run did not return within 15s")
			}
		})

		assert.Contains(t, stdout, "telegram notifier disabled: VPNTUNNEL_TELEGRAMBOT_DSN not set")
	})

	t.Run("malformed telegram dsn warns and disables the notifier without aborting startup", func(t *testing.T) {
		// not t.Parallel() — see the previous subtest's comment; also uses
		// t.Setenv, which forbids parallel siblings.
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "proxy.json")
		certDir := filepath.Join(dir, "tls")
		writeFixtureConfig(t, cfgPath, fixtureConfig{
			proxyAddr: "127.0.0.1:17802",
			apiAddr:   "127.0.0.1:18902",
			certDir:   certDir,
		})

		const malformedDSN = "tbot://not-a-valid-dsn"
		t.Setenv(constants.EnvTelegramBotDSN, malformedDSN)

		shutdownCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)
		stdout := captureStdout(t, func() {
			go func() {
				done <- run(cfgPath, tlsOptions{CertDir: certDir, Hostname: "localhost", IPSANs: []net.IP{net.ParseIP("127.0.0.1")}}, withSupervisorBuilder(smokeBuilder), withSchedulerBuilder(smokeBuilder), withShutdownCtx(shutdownCtx))
			}()

			apiReady := waitHTTPS(t, "https://127.0.0.1:18902/v1/admin/health", 10*time.Second)
			assert.True(t, apiReady, "api listener (127.0.0.1:18902) did not become reachable")

			cancel()

			select {
			case runErr := <-done:
				// a malformed DSN must never abort the proxy — it is auxiliary
				// telemetry, not a startup precondition.
				assert.NoError(t, runErr, "run must succeed even with a malformed telegram DSN")
			case <-time.After(15 * time.Second):
				t.Fatal("run did not return within 15s")
			}
		})

		assert.Contains(t, stdout, "telegram notifier disabled: invalid VPNTUNNEL_TELEGRAMBOT_DSN")
		assert.Contains(t, strings.ToUpper(stdout), "WARN")
		assert.NotContains(t, stdout, malformedDSN, "the raw DSN must never be logged")
	})
}

// fixtureConfig holds the paths needed to write a test proxy.json.
type fixtureConfig struct {
	proxyAddr   string
	apiAddr     string
	asyncDBPath string
	// certDir is the directory where apitls.LoadOrGenerate will store/generate
	// the TLS cert. When empty, writeFixtureConfig derives it as
	// filepath.Join(dir, "tls"). The caller must pass the same path as
	// tlsOptions.CertDir to run so the on-disk dir and the flag agree.
	certDir string
}

// writeFixtureConfig writes a proxy.json at cfgPath suitable for running
// run in tests. All secrets (tokens, keys) are generated fresh.
// The fixture includes an async block pointing to cfg.asyncDBPath (when
// non-empty) or a temp path derived from cfgPath's directory.
//
// The cert dir is derived from cfg.certDir; when empty it defaults to
// filepath.Join(dir, "tls"). The caller must pass the same cert-dir path as
// tlsOptions.CertDir to run — writeFixtureConfig no longer embeds TLS
// settings in the JSON; they travel as CLI-flag-equivalent tlsOptions.
func writeFixtureConfig(tb testing.TB, cfgPath string, cfg fixtureConfig) {
	tb.Helper()
	dir := filepath.Dir(cfgPath)

	// write the .conf into the tunnels/ subdir so DiscoverConfigs picks it up.
	tunnelsDir := filepath.Join(dir, "tunnels")
	require.NoError(tb, os.MkdirAll(tunnelsDir, 0o700))
	confContent := fmt.Sprintf("[Interface]\nPrivateKey = %s\nAddress = 10.99.0.1/32\n\n[Peer]\nPublicKey = %s\nEndpoint = 203.0.113.1:51820\n",
		genWGKey(tb), genWGPubKey(tb))
	require.NoError(tb, os.WriteFile(filepath.Join(tunnelsDir, "tunnel.conf"), []byte(confContent), 0o600))

	authDir := filepath.Join(dir, "auth")
	require.NoError(tb, os.MkdirAll(authDir, 0o700))
	userTok := filepath.Join(authDir, "proxy_token")
	adminTok := filepath.Join(authDir, "admin_token")
	writeToken(tb, userTok, 3)
	writeToken(tb, adminTok, 4)

	certDir := cfg.certDir
	if certDir == "" {
		certDir = filepath.Join(dir, "tls")
	}
	require.NoError(tb, os.MkdirAll(certDir, 0o700))

	logDir := filepath.Join(dir, "logs")
	require.NoError(tb, os.MkdirAll(logDir, 0o755))

	asyncDBPath := cfg.asyncDBPath
	if asyncDBPath == "" {
		asyncDBPath = filepath.Join(dir, "async.db")
	}

	proxyAddr := cfg.proxyAddr
	if proxyAddr == "" {
		tb.Helper()
		tb.Fatal("writeFixtureConfig: proxyAddr must not be empty")
	}
	apiAddr := cfg.apiAddr
	if apiAddr == "" {
		tb.Helper()
		tb.Fatal("writeFixtureConfig: apiAddr must not be empty")
	}

	// v6 config shape: vpnstream block for proxy listener settings; api.vpn
	// for on-demand VPN knobs; api.auth for the two API role tokens.
	// Tunnels are auto-discovered from <configDir>/tunnels/; TLS settings
	// travel via tlsOptions (CLI flags), not the JSON config.
	cfgJSON := fmt.Sprintf(`{
  "vpnstream": {
    "listen": %q,
    "dial_timeout": "5s",
    "shutdown_timeout": "3s"
  },
  "api": {
    "listen": %q,
    "shutdown_timeout": "2s",
    "auth": {
      "proxy_token_file": %q,
      "admin_token_file":  %q
    },
    "max_request_body_bytes": 1048576,
    "vpn": {
      "timeout":      "5s",
      "max_timeout":  "30s",
      "async": {
        "storage_path": %q
      }
    }
  },
  "access_log": { "path": %q }
}`,
		proxyAddr,
		apiAddr,
		userTok, adminTok,
		asyncDBPath,
		filepath.Join(logDir, "access.log"),
	)
	require.NoError(tb, os.WriteFile(cfgPath, []byte(cfgJSON), 0o644))
}

// waitHTTP polls url with a plain-HTTP client until it gets any response or
// timeout elapses. Returns true if the endpoint responded within the timeout.
func waitHTTP(tb testing.TB, url string, timeout time.Duration) bool {
	tb.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url) //nolint:noctx // polling loop; deadline-bounded
		if err == nil {
			_ = resp.Body.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// waitTCP polls addr until a TCP connection is accepted or timeout elapses.
// Returns true if the listener became reachable within the timeout.
func waitTCP(tb testing.TB, addr string, timeout time.Duration) bool {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// waitHTTPS polls url with a TLS client that skips verification (the cert is
// self-signed by apitls) until it gets any HTTP response or timeout elapses.
// Returns true if the endpoint responded within the timeout.
func waitHTTPS(tb testing.TB, url string, timeout time.Duration) bool {
	tb.Helper()
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only
		},
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url) //nolint:noctx // polling loop; context cancel handled by deadline
		if err == nil {
			_ = resp.Body.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// captureStdout temporarily redirects the process-wide os.Stdout to a pipe,
// runs fn, restores the original os.Stdout, and returns everything written
// during fn. A background goroutine drains the pipe continuously so a
// long-running fn producing more output than the pipe's kernel buffer cannot
// deadlock. Callers must not run this concurrently with anything else that
// writes to or depends on os.Stdout (subtests using it must not be
// t.Parallel()).
func captureStdout(tb testing.TB, fn func()) string {
	tb.Helper()
	r, w, err := os.Pipe()
	require.NoError(tb, err)

	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	captured := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		captured <- buf.String()
	}()

	fn()

	require.NoError(tb, w.Close())
	out := <-captured
	require.NoError(tb, r.Close())
	return out
}
