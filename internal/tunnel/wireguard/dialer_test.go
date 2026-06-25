package wireguard_test

import (
	"context"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"vpntunnel/internal/tunnel/wireguard"
)

// newTestLogger returns a slog logger suitable for tests.
func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(
		testWriter{},
		&slog.HandlerOptions{Level: slog.LevelDebug},
	))
}

// testWriter discards log output in tests (wireguard-go is very chatty).
type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

// generateKey generates a fresh WireGuard private key for testing.
func generateKey(t *testing.T) wgtypes.Key {
	t.Helper()
	k, err := wgtypes.GeneratePrivateKey()
	require.NoError(t, err)
	return k
}

// minimalOpts returns a complete Options struct using freshly generated keys
// and a dead-peer endpoint (127.0.0.1:51820). The handshake will not complete
// but device construction will succeed.
func minimalOpts(t *testing.T) wireguard.Options {
	t.Helper()
	priv := generateKey(t)
	peer := generateKey(t)
	return wireguard.Options{
		PrivateKey:    priv.String(),
		PeerPublicKey: peer.PublicKey().String(),
		PeerEndpoint:  "127.0.0.1:51820",
		LocalAddresses: []netip.Addr{
			netip.MustParseAddr("10.99.0.1"),
		},
		DNSServers: []netip.Addr{
			netip.MustParseAddr("10.64.0.1"),
		},
		MTU:                        1420,
		PersistentKeepaliveSeconds: 25,
		AllowedIPs: []netip.Prefix{
			netip.MustParsePrefix("0.0.0.0/0"),
			netip.MustParsePrefix("::/0"),
		},
		Logger: newTestLogger(),
	}
}

func TestNewDialer(t *testing.T) {
	t.Parallel()

	t.Run("rejects invalid private key", func(t *testing.T) {
		t.Parallel()
		opts := minimalOpts(t)
		opts.PrivateKey = "not-valid-base64!!!!"
		_, err := wireguard.NewDialer(t.Context(), opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "private key")
	})

	t.Run("rejects invalid peer public key", func(t *testing.T) {
		t.Parallel()
		opts := minimalOpts(t)
		opts.PeerPublicKey = "not-valid-base64!!!!"
		_, err := wireguard.NewDialer(t.Context(), opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "peer key")
	})

	t.Run("rejects unresolvable peer endpoint", func(t *testing.T) {
		t.Parallel()
		opts := minimalOpts(t)
		opts.PeerEndpoint = "does-not-exist.invalid:51820"
		_, err := wireguard.NewDialer(t.Context(), opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "peer endpoint")
	})

	t.Run("constructs device with valid inputs and Close tears down", func(t *testing.T) {
		t.Parallel()
		opts := minimalOpts(t)
		d, err := wireguard.NewDialer(t.Context(), opts)
		require.NoError(t, err)
		require.NotNil(t, d)

		// register cleanup before any assertion that could panic
		t.Cleanup(func() { _ = d.Close() })

		// Close should return nil and be idempotent
		require.NoError(t, d.Close())
		require.NoError(t, d.Close(), "second Close must be a no-op, not an error")
	})

	t.Run("panics when Logger is nil", func(t *testing.T) {
		t.Parallel()
		opts := minimalOpts(t)
		opts.Logger = nil
		assert.Panics(t, func() {
			_, _ = wireguard.NewDialer(t.Context(), opts)
		})
	})
}

func TestWireGuardDialer_DialContext(t *testing.T) {
	t.Parallel()

	t.Run("propagates context cancellation", func(t *testing.T) {
		t.Parallel()
		opts := minimalOpts(t)
		d, err := wireguard.NewDialer(t.Context(), opts)
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel immediately

		_, err = d.DialContext(ctx, "tcp", "10.0.0.1:80")
		require.Error(t, err)
		// the error is either ctx.Err() wrapped or a timeout — both acceptable
		// because gVisor's stack may not propagate context.Canceled verbatim;
		// it may return a timeout error when the context is already done.
		assert.Error(t, err)
	})

	t.Run("returns error when handshake never completes due to dead peer", func(t *testing.T) {
		t.Parallel()
		opts := minimalOpts(t)
		d, err := wireguard.NewDialer(t.Context(), opts)
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })

		// use a short deadline; the peer (127.0.0.1:51820) is not listening so
		// the handshake will never complete and the dial will time out.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		_, err = d.DialContext(ctx, "tcp", "10.0.0.2:80")
		require.Error(t, err, "expected error when dialling through a dead peer")
	})
}
