package wireguard

import (
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// knownEndpoint is a deterministic test endpoint that is already resolved.
var knownEndpoint = netip.MustParseAddrPort("192.0.2.1:51820")

// testOpts returns deterministic Options using all-zeros private key and
// all-ones peer key. These are valid wgtypes keys for the purposes of hex
// encoding; wireguard-go does not validate key material in unit tests.
func testOpts() Options {
	return Options{
		PrivateKey:                 "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		PeerPublicKey:              "//////////////////////////////////////////8=",
		PeerEndpoint:               "192.0.2.1:51820",
		LocalAddresses:             []netip.Addr{netip.MustParseAddr("10.66.0.5")},
		DNSServers:                 []netip.Addr{netip.MustParseAddr("10.64.0.1")},
		MTU:                        1420,
		PersistentKeepaliveSeconds: 25,
		AllowedIPs: []netip.Prefix{
			netip.MustParsePrefix("0.0.0.0/0"),
			netip.MustParsePrefix("::/0"),
		},
	}
}

func TestResolveEndpoint(t *testing.T) {
	t.Parallel()

	t.Run("already-numeric IP bypasses DNS lookup", func(t *testing.T) {
		t.Parallel()
		ap, err := resolveEndpoint(t.Context(), "203.0.113.1:51820")
		require.NoError(t, err)
		assert.Equal(t, netip.MustParseAddrPort("203.0.113.1:51820"), ap)
	})

	t.Run("returns first address from IPv6-only result", func(t *testing.T) {
		t.Parallel()
		// localhost resolves to 127.0.0.1 (IPv4) on any sane system;
		// verify the function returns a valid AddrPort without error.
		ap, err := resolveEndpoint(t.Context(), "localhost:51820")
		require.NoError(t, err)
		assert.Equal(t, uint16(51820), ap.Port())
		assert.True(t, ap.Addr().IsValid())
	})

	t.Run("returns error on no addresses", func(t *testing.T) {
		t.Parallel()
		_, err := resolveEndpoint(t.Context(), "does-not-exist.invalid:51820")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "resolve")
	})
}

func TestBuildIpcSet(t *testing.T) {
	t.Parallel()

	t.Run("emits canonical key=value lines", func(t *testing.T) {
		t.Parallel()
		opts := testOpts()
		got, err := buildIpcSet(opts, knownEndpoint)
		require.NoError(t, err)

		want, err := os.ReadFile("testdata/uapi_canonical.txt")
		require.NoError(t, err)
		assert.Equal(t, string(want), got)
	})

	t.Run("emits one allowed_ip line per prefix in order", func(t *testing.T) {
		t.Parallel()
		opts := testOpts()
		opts.AllowedIPs = []netip.Prefix{
			netip.MustParsePrefix("10.0.0.0/8"),
			netip.MustParsePrefix("172.16.0.0/12"),
			netip.MustParsePrefix("192.168.0.0/16"),
		}
		got, err := buildIpcSet(opts, knownEndpoint)
		require.NoError(t, err)

		lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
		var ipLines []string
		for _, l := range lines {
			if strings.HasPrefix(l, "allowed_ip=") {
				ipLines = append(ipLines, l)
			}
		}
		require.Len(t, ipLines, 3)
		assert.Equal(t, "allowed_ip=10.0.0.0/8", ipLines[0])
		assert.Equal(t, "allowed_ip=172.16.0.0/12", ipLines[1])
		assert.Equal(t, "allowed_ip=192.168.0.0/16", ipLines[2])
	})

	t.Run("emits persistent_keepalive_interval=0 when keepalive is zero", func(t *testing.T) {
		t.Parallel()
		opts := testOpts()
		opts.PersistentKeepaliveSeconds = 0
		got, err := buildIpcSet(opts, knownEndpoint)
		require.NoError(t, err)
		assert.Contains(t, got, "persistent_keepalive_interval=0\n")
	})

	t.Run("encodes keys as 64-char hex not base64", func(t *testing.T) {
		t.Parallel()
		opts := testOpts()
		got, err := buildIpcSet(opts, knownEndpoint)
		require.NoError(t, err)

		lines := strings.Split(got, "\n")
		for _, l := range lines {
			if strings.HasPrefix(l, "private_key=") || strings.HasPrefix(l, "public_key=") {
				parts := strings.SplitN(l, "=", 2)
				require.Len(t, parts, 2)
				val := parts[1]
				assert.Len(t, val, 64, "expected 64-char hex for %s, got %q", parts[0], val)
				// base64 keys end with = padding; hex never does
				assert.NotContains(t, val, "=", "key should be hex, not base64: %s=%s", parts[0], val)
			}
		}
	})

	t.Run("returns error on invalid private key", func(t *testing.T) {
		t.Parallel()
		opts := testOpts()
		opts.PrivateKey = "not-valid-base64!!!!"
		_, err := buildIpcSet(opts, knownEndpoint)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse private key")
	})

	t.Run("returns error on invalid peer key", func(t *testing.T) {
		t.Parallel()
		opts := testOpts()
		opts.PeerPublicKey = "not-valid-base64!!!!"
		_, err := buildIpcSet(opts, knownEndpoint)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse peer key")
	})

	t.Run("endpoint uses bracketed form for IPv6", func(t *testing.T) {
		t.Parallel()
		opts := testOpts()
		ep := netip.MustParseAddrPort("[2001:db8::1]:51820")
		got, err := buildIpcSet(opts, ep)
		require.NoError(t, err)
		assert.Contains(t, got, "endpoint=[2001:db8::1]:51820\n")
	})

	t.Run("output ends with newline", func(t *testing.T) {
		t.Parallel()
		opts := testOpts()
		got, err := buildIpcSet(opts, knownEndpoint)
		require.NoError(t, err)
		assert.True(t, strings.HasSuffix(got, "\n"), "UAPI string must end with newline")
	})

	t.Run("emits preshared_key as 64-char hex when PresharedKey is set", func(t *testing.T) {
		t.Parallel()
		opts := testOpts()
		// all-zeros PSK in base64 — same encoding as a wgtypes key
		opts.PresharedKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
		got, err := buildIpcSet(opts, knownEndpoint)
		require.NoError(t, err)

		var pskLine string
		for _, l := range strings.Split(got, "\n") {
			if strings.HasPrefix(l, "preshared_key=") {
				pskLine = l
				break
			}
		}
		require.NotEmpty(t, pskLine, "expected preshared_key line in UAPI output")
		parts := strings.SplitN(pskLine, "=", 2)
		require.Len(t, parts, 2)
		val := parts[1]
		assert.Len(t, val, 64, "preshared_key must be 64-char hex")
		assert.NotContains(t, val, "=", "preshared_key must be hex, not base64")
		// all-zeros key → all-zeros hex
		assert.Equal(t, strings.Repeat("0", 64), val)
	})

	t.Run("no preshared_key line when PresharedKey is empty", func(t *testing.T) {
		t.Parallel()
		opts := testOpts()
		opts.PresharedKey = ""
		got, err := buildIpcSet(opts, knownEndpoint)
		require.NoError(t, err)
		assert.NotContains(t, got, "preshared_key=")
	})

	t.Run("returns error on invalid preshared key", func(t *testing.T) {
		t.Parallel()
		opts := testOpts()
		opts.PresharedKey = "not-valid-base64!!!!"
		_, err := buildIpcSet(opts, knownEndpoint)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse preshared key")
	})
}
