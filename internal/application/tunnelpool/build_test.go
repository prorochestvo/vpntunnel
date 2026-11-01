package tunnelpool

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"

	"github.com/prorochestvo/loginjector"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/infrastructure/wireguard/wgconf"
)

// compile-time assertion: fakeDialer must satisfy DialerCloser.
var _ DialerCloser = (*fakeDialer)(nil)

// fakeDialer is a minimal in-memory DialerCloser for build tests.
type fakeDialer struct {
	closeCount atomic.Int32
}

func (f *fakeDialer) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return nil, errors.New("fakeDialer: not implemented")
}

func (f *fakeDialer) Close() error {
	f.closeCount.Add(1)
	return nil
}

// minimalConf is a syntactically-valid wg-quick conf accepted by wgconf.Parse.
const minimalConf = `[Interface]
PrivateKey = 6K+9LAzCccccccccccccccccccccccccccccccccccY=
Address = 10.64.0.2/32
DNS = 10.64.0.1

[Peer]
PublicKey = 6K+9LAzCccccccccccccccccccccccccccccccccccY=
Endpoint = 1.2.3.4:51820
AllowedIPs = 0.0.0.0/0
`

// writeFakeConfBuild writes minimalConf to "<dir>/<name>.conf" and returns the path.
func writeFakeConfBuild(t *testing.T, dir, name string) string {
	t.Helper()
	path := dir + "/" + name + ".conf"
	require.NoError(t, os.WriteFile(path, []byte(minimalConf), 0o600))
	return path
}

func TestBuildDialer(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)

	t.Run("success path with fake builder", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		confPath := writeFakeConfBuild(t, dir, "mullvad-se-sto-wg-001")

		fake := &fakeDialer{}
		fn := BuilderFn(func(_ context.Context, parsed *wgconf.ParsedConfig, _ *slog.Logger) (DialerCloser, error) {
			// verify parsed content was passed correctly
			assert.NotEmpty(t, parsed.Interface.PrivateKey)
			assert.NotEmpty(t, parsed.Peer.Endpoint)
			return fake, nil
		})

		d, err := BuildDialer(t.Context(), confPath, dir, log, fn)
		require.NoError(t, err)
		assert.Equal(t, fake, d)
		require.NoError(t, d.Close())
	})

	t.Run("nil builder is dispatched only after a successful parse without panicking", func(t *testing.T) {
		t.Parallel()
		// a nil fn means production DefaultBuilder would be used, which opens a real
		// userspace wireguard-go + gVisor netstack device — forbidden in unit tests
		// (it leaks UDP sockets and blocking goroutines into the package test binary).
		// Verify the nil-fn dispatch path does not panic by feeding a config that
		// fails to parse, so the builder is never reached. DefaultBuilder itself is
		// exercised by the Task 10 integration test, not here.
		dir := t.TempDir()
		badPath := dir + "/bad.conf"
		require.NoError(t, os.WriteFile(badPath, []byte("not a conf\n"), 0o600))

		assert.NotPanics(t, func() {
			_, err := BuildDialer(t.Context(), badPath, dir, log, nil)
			require.Error(t, err)
			isPublic := errors.As(err, new(loginjector.PublicDetailsError))
			assert.False(t, isPublic, "parse failure must be a plain error")
		})
	})

	t.Run("parse error returns wrapped plain error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		badPath := dir + "/bad.conf"
		require.NoError(t, os.WriteFile(badPath, []byte("not a conf\n"), 0o600))

		called := false
		fn := BuilderFn(func(_ context.Context, _ *wgconf.ParsedConfig, _ *slog.Logger) (DialerCloser, error) {
			called = true
			return nil, nil
		})

		_, err := BuildDialer(t.Context(), badPath, dir, log, fn)
		require.Error(t, err)
		assert.False(t, called, "builder must not be called when parse fails")
		assert.Contains(t, err.Error(), "tunnelpool: parse")
	})

	t.Run("build error returns wrapped plain error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		confPath := writeFakeConfBuild(t, dir, "mullvad-de-fra-wg-001")

		injectedErr := errors.New("injected build failure")
		fn := BuilderFn(func(_ context.Context, _ *wgconf.ParsedConfig, _ *slog.Logger) (DialerCloser, error) {
			return nil, injectedErr
		})

		_, err := BuildDialer(t.Context(), confPath, dir, log, fn)
		require.Error(t, err)
		assert.ErrorIs(t, err, injectedErr)
		assert.Contains(t, err.Error(), "tunnelpool: build")
	})

	t.Run("relative configPath resolved against configDir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFakeConfBuild(t, dir, "mullvad-gb-lon-wg-001")

		fake := &fakeDialer{}
		fn := BuilderFn(func(_ context.Context, _ *wgconf.ParsedConfig, _ *slog.Logger) (DialerCloser, error) {
			return fake, nil
		})

		// pass relative name; BuildDialer must resolve it against dir
		d, err := BuildDialer(t.Context(), "mullvad-gb-lon-wg-001.conf", dir, log, fn)
		require.NoError(t, err)
		assert.Equal(t, fake, d)
		require.NoError(t, d.Close())
	})

	t.Run("builder receives correct parsed options", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		confPath := writeFakeConfBuild(t, dir, "mullvad-ua-kiv-wg-001")

		var capturedParsed *wgconf.ParsedConfig
		fn := BuilderFn(func(_ context.Context, p *wgconf.ParsedConfig, _ *slog.Logger) (DialerCloser, error) {
			capturedParsed = p
			return &fakeDialer{}, nil
		})

		_, err := BuildDialer(t.Context(), confPath, dir, log, fn)
		require.NoError(t, err)
		require.NotNil(t, capturedParsed)

		// verify the wg-quick defaults were applied by the parser
		assert.Equal(t, "1.2.3.4:51820", capturedParsed.Peer.Endpoint)
		assert.Equal(t, 1420, capturedParsed.Interface.MTU)
		assert.Equal(t, 25, capturedParsed.Peer.PersistentKeepaliveSeconds)
		require.Len(t, capturedParsed.Interface.Addresses, 1)
		assert.Equal(t, netip.MustParsePrefix("10.64.0.2/32"), capturedParsed.Interface.Addresses[0])
	})
}

// localAddresses verifies firstLocalAddress handles empty addresses.
func TestFirstLocalAddress(t *testing.T) {
	t.Parallel()

	t.Run("empty addresses returns empty string", func(t *testing.T) {
		t.Parallel()
		parsed := &wgconf.ParsedConfig{}
		assert.Equal(t, "", firstLocalAddress(parsed))
	})

	t.Run("first address returned as string", func(t *testing.T) {
		t.Parallel()
		parsed := &wgconf.ParsedConfig{
			Interface: wgconf.InterfaceSection{
				Addresses: []netip.Prefix{
					netip.MustParsePrefix("10.64.0.2/32"),
					netip.MustParsePrefix("fd00::2/128"),
				},
			},
		}
		assert.Equal(t, "10.64.0.2/32", firstLocalAddress(parsed))
	})
}
