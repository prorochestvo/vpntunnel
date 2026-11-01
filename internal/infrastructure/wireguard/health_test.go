package wireguard

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

var _ ipcGetter = (*failingIpcGetter)(nil)

var (
	_ healthReporter = (*WireGuardDialer)(nil)
	_ dialer         = (*WireGuardDialer)(nil)
	_ dialerCloser   = (*WireGuardDialer)(nil)
	_ resolver       = (*WireGuardDialer)(nil)
)

// newHealthTestLogger returns a logger that discards output.
func newHealthTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(healthTestWriter{}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type healthTestWriter struct{}

func (healthTestWriter) Write(p []byte) (int, error) { return len(p), nil }

// healthMinimalOpts returns a minimal valid Options using dead-peer endpoint.
func healthMinimalOpts(t *testing.T) Options {
	t.Helper()
	priv, err := wgtypes.GeneratePrivateKey()
	require.NoError(t, err)
	peer, err := wgtypes.GeneratePrivateKey()
	require.NoError(t, err)
	return Options{
		PrivateKey:    priv.String(),
		PeerPublicKey: peer.PublicKey().String(),
		PeerEndpoint:  "127.0.0.1:51820",
		LocalAddresses: []netip.Addr{
			netip.MustParseAddr("10.99.0.2"),
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
		Logger: newHealthTestLogger(),
	}
}

// failingIpcGetter is a test stub for ipcGetter that always returns an error.
type failingIpcGetter struct {
	err error
}

func (f *failingIpcGetter) IpcGet() (string, error) { return "", f.err }

func TestWireGuardDialer_LastHandshake(t *testing.T) {
	t.Parallel()

	t.Run("returns zero time on freshly-constructed device", func(t *testing.T) {
		t.Parallel()
		d, err := NewDialer(t.Context(), healthMinimalOpts(t))
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })

		ts, err := d.LastHandshake()
		require.NoError(t, err)
		assert.True(t, ts.IsZero(), "expected zero time before any handshake, got %v", ts)
	})

	t.Run("propagates IpcGet failure as error containing ipcget", func(t *testing.T) {
		t.Parallel()
		d := &WireGuardDialer{ipc: &failingIpcGetter{err: errors.New("boom")}}
		_, err := d.LastHandshake()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ipcget")
	})
}

func TestParseLastHandshake(t *testing.T) {
	t.Parallel()

	t.Run("returns zero time on empty input", func(t *testing.T) {
		t.Parallel()
		ts, err := parseLastHandshake("")
		require.NoError(t, err)
		assert.True(t, ts.IsZero())
	})

	t.Run("returns zero time when last_handshake_time_sec=0", func(t *testing.T) {
		t.Parallel()
		uapi := "public_key=abc\nlast_handshake_time_sec=0\nlast_handshake_time_nsec=0\n"
		ts, err := parseLastHandshake(uapi)
		require.NoError(t, err)
		assert.True(t, ts.IsZero())
	})

	t.Run("returns time.Unix(N,0) when last_handshake_time_sec=N", func(t *testing.T) {
		t.Parallel()
		uapi := "public_key=abc\nlast_handshake_time_sec=1700000000\n"
		ts, err := parseLastHandshake(uapi)
		require.NoError(t, err)
		assert.Equal(t, time.Unix(1700000000, 0), ts)
	})

	t.Run("returns max across multiple peers", func(t *testing.T) {
		t.Parallel()
		uapi := "public_key=aaa\nlast_handshake_time_sec=1000\npublic_key=bbb\nlast_handshake_time_sec=9999\npublic_key=ccc\nlast_handshake_time_sec=500\n"
		ts, err := parseLastHandshake(uapi)
		require.NoError(t, err)
		assert.Equal(t, time.Unix(9999, 0), ts)
	})

	t.Run("ignores unknown keys", func(t *testing.T) {
		t.Parallel()
		uapi := "private_key=deadbeef\npublic_key=cafebabe\nendpoint=1.2.3.4:51820\nallowed_ip=0.0.0.0/0\nlast_handshake_time_sec=42\n"
		ts, err := parseLastHandshake(uapi)
		require.NoError(t, err)
		assert.Equal(t, time.Unix(42, 0), ts)
	})

	t.Run("returns error on non-integer value", func(t *testing.T) {
		t.Parallel()
		uapi := "last_handshake_time_sec=foo\n"
		_, err := parseLastHandshake(uapi)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "last_handshake_time_sec=foo")
	})

	t.Run("ignores last_handshake_time_nsec does not influence result", func(t *testing.T) {
		t.Parallel()
		// nsec line present but should be skipped entirely.
		uapi := "last_handshake_time_sec=100\nlast_handshake_time_nsec=999999999\n"
		ts, err := parseLastHandshake(uapi)
		require.NoError(t, err)
		// result must be time.Unix(100, 0) — nsec is ignored.
		assert.Equal(t, time.Unix(100, 0), ts)
	})
}

// dialer, dialerCloser, resolver, and healthReporter are test-local
// copies of the four egress ports tunnelpool declares. They are copies rather
// than imports because wireguard sits below tunnelpool: importing it from here
// would invert the layering even in test scope.
//
// Honest limitation: a copy no longer breaks if tunnelpool changes a port. The
// real compile-time guard is tunnelpool.DefaultBuilder, which returns
// *WireGuardDialer as a tunnelpool.DialerCloser — that one still breaks.
type dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type dialerCloser interface {
	dialer
	io.Closer
}

type resolver interface {
	LookupHost(ctx context.Context, host string) ([]netip.Addr, error)
}

type healthReporter interface {
	LastHandshake() (time.Time, error)
}
