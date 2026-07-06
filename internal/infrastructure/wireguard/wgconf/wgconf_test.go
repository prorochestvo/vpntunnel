package wgconf_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"vpntunnel/internal/infrastructure/wireguard/wgconf"
)

// genKey generates a fresh WireGuard private key and returns its base64 string.
func genKey(tb testing.TB) string {
	tb.Helper()
	k, err := wgtypes.GeneratePrivateKey()
	require.NoError(tb, err)
	return k.String()
}

// genPubKey generates a fresh WireGuard public key and returns its base64 string.
func genPubKey(tb testing.TB) string {
	tb.Helper()
	k, err := wgtypes.GeneratePrivateKey()
	require.NoError(tb, err)
	return k.PublicKey().String()
}

// writeConf writes the given wg-quick config body to a temp file and returns the path.
func writeConf(tb testing.TB, body string) string {
	tb.Helper()
	dir := tb.TempDir()
	path := filepath.Join(dir, "test.conf")
	require.NoError(tb, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// captureLogger returns a logger that writes to a buffer, and the buffer itself.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, &buf
}

// minimalConf returns a minimal valid wg-quick conf body using generated keys.
func minimalConf(tb testing.TB) string {
	tb.Helper()
	return fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
`, genKey(tb), genPubKey(tb))
}

func TestParse(t *testing.T) {
	t.Parallel()

	t.Run("loads valid mullvad-shaped config", func(t *testing.T) {
		t.Parallel()
		privKey := genKey(t)
		pubKey := genPubKey(t)
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32, fc00::5/128
DNS = 10.64.0.1

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
AllowedIPs = 0.0.0.0/0, ::/0
PersistentKeepalive = 25
`, privKey, pubKey)

		path := writeConf(t, body)
		logger, _ := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)

		assert.Equal(t, privKey, cfg.Interface.PrivateKey)
		require.Len(t, cfg.Interface.Addresses, 2)
		assert.Equal(t, netip.MustParsePrefix("10.66.0.5/32"), cfg.Interface.Addresses[0])
		assert.Equal(t, netip.MustParsePrefix("fc00::5/128"), cfg.Interface.Addresses[1])
		assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.64.0.1")}, cfg.Interface.DNS)
		assert.Equal(t, 1420, cfg.Interface.MTU)

		assert.Equal(t, pubKey, cfg.Peer.PublicKey)
		assert.Equal(t, "185.1.2.3:51820", cfg.Peer.Endpoint)
		require.Len(t, cfg.Peer.AllowedIPs, 2)
		assert.Equal(t, netip.MustParsePrefix("0.0.0.0/0"), cfg.Peer.AllowedIPs[0])
		assert.Equal(t, netip.MustParsePrefix("::/0"), cfg.Peer.AllowedIPs[1])
		assert.Equal(t, 25, cfg.Peer.PersistentKeepaliveSeconds)
		assert.Empty(t, cfg.Peer.PresharedKey)
	})

	t.Run("missing [Interface] returns error mentioning [Interface]", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
`, genPubKey(t))
		path := writeConf(t, body)
		logger, _ := captureLogger()
		_, err := wgconf.Parse(path, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "[Interface]")
	})

	t.Run("missing [Peer] returns error mentioning [Peer]", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32
`, genKey(t))
		path := writeConf(t, body)
		logger, _ := captureLogger()
		_, err := wgconf.Parse(path, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "[Peer]")
	})

	t.Run("two [Peer] blocks returns error mentioning multi-peer not supported", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820

[Peer]
PublicKey = %s
Endpoint = 185.1.2.4:51820
`, genKey(t), genPubKey(t), genPubKey(t))
		path := writeConf(t, body)
		logger, _ := captureLogger()
		_, err := wgconf.Parse(path, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "multi-peer not supported")
	})

	t.Run("missing PrivateKey returns error naming the missing key", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
Address = 10.66.0.5/32

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
`, genPubKey(t))
		path := writeConf(t, body)
		logger, _ := captureLogger()
		_, err := wgconf.Parse(path, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "PrivateKey")
	})

	t.Run("missing Address returns error naming the missing key", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
`, genKey(t), genPubKey(t))
		path := writeConf(t, body)
		logger, _ := captureLogger()
		_, err := wgconf.Parse(path, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Address")
	})

	t.Run("missing PublicKey returns error naming the missing key", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32

[Peer]
Endpoint = 185.1.2.3:51820
`, genKey(t))
		path := writeConf(t, body)
		logger, _ := captureLogger()
		_, err := wgconf.Parse(path, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "PublicKey")
	})

	t.Run("missing Endpoint returns error naming the missing key", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32

[Peer]
PublicKey = %s
`, genKey(t), genPubKey(t))
		path := writeConf(t, body)
		logger, _ := captureLogger()
		_, err := wgconf.Parse(path, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Endpoint")
	})

	t.Run("lowercase keys are parsed correctly", func(t *testing.T) {
		t.Parallel()
		privKey := genKey(t)
		pubKey := genPubKey(t)
		body := fmt.Sprintf(`[Interface]
privatekey = %s
address = 10.66.0.5/32

[Peer]
publickey = %s
endpoint = 185.1.2.3:51820
`, privKey, pubKey)
		path := writeConf(t, body)
		logger, _ := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		assert.Equal(t, privKey, cfg.Interface.PrivateKey)
		assert.Equal(t, pubKey, cfg.Peer.PublicKey)
	})

	t.Run("lowercase section header [interface] returns error", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[interface]
PrivateKey = %s
Address = 10.66.0.5/32

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
`, genKey(t), genPubKey(t))
		path := writeConf(t, body)
		logger, _ := captureLogger()
		_, err := wgconf.Parse(path, logger)
		require.Error(t, err)
	})

	t.Run("inline comments are stripped", func(t *testing.T) {
		t.Parallel()
		privKey := genKey(t)
		pubKey := genPubKey(t)
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820  # primary server
`, privKey, pubKey)
		path := writeConf(t, body)
		logger, _ := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		assert.Equal(t, "185.1.2.3:51820", cfg.Peer.Endpoint)
	})

	t.Run("Address with dual-stack CIDRs produces two prefix entries", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32, fc00::5/128

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
`, genKey(t), genPubKey(t))
		path := writeConf(t, body)
		logger, _ := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		require.Len(t, cfg.Interface.Addresses, 2)
		assert.Equal(t, netip.MustParsePrefix("10.66.0.5/32"), cfg.Interface.Addresses[0])
		assert.Equal(t, netip.MustParsePrefix("fc00::5/128"), cfg.Interface.Addresses[1])
	})

	t.Run("missing DNS defaults to [10.64.0.1]", func(t *testing.T) {
		t.Parallel()
		path := writeConf(t, minimalConf(t))
		logger, _ := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.64.0.1")}, cfg.Interface.DNS)
	})

	t.Run("missing AllowedIPs defaults to [0.0.0.0/0, ::/0]", func(t *testing.T) {
		t.Parallel()
		path := writeConf(t, minimalConf(t))
		logger, _ := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		require.Len(t, cfg.Peer.AllowedIPs, 2)
		assert.Equal(t, netip.MustParsePrefix("0.0.0.0/0"), cfg.Peer.AllowedIPs[0])
		assert.Equal(t, netip.MustParsePrefix("::/0"), cfg.Peer.AllowedIPs[1])
	})

	t.Run("missing MTU defaults to 1420", func(t *testing.T) {
		t.Parallel()
		path := writeConf(t, minimalConf(t))
		logger, _ := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		assert.Equal(t, 1420, cfg.Interface.MTU)
	})

	t.Run("missing PersistentKeepalive defaults to 25", func(t *testing.T) {
		t.Parallel()
		path := writeConf(t, minimalConf(t))
		logger, _ := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		assert.Equal(t, 25, cfg.Peer.PersistentKeepaliveSeconds)
	})

	t.Run("explicit PersistentKeepalive=0 is preserved and not defaulted", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
PersistentKeepalive = 0
`, genKey(t), genPubKey(t))
		path := writeConf(t, body)
		logger, _ := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		assert.Equal(t, 0, cfg.Peer.PersistentKeepaliveSeconds)
	})

	t.Run("PresharedKey propagated to PeerSection", func(t *testing.T) {
		t.Parallel()
		psk := genPubKey(t) // same base64 shape as a key
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
PresharedKey = %s
`, genKey(t), genPubKey(t), psk)
		path := writeConf(t, body)
		logger, _ := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		assert.Equal(t, psk, cfg.Peer.PresharedKey)
	})

	t.Run("unknown key produces one WARN and Parse succeeds", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32
Foo = bar

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
`, genKey(t), genPubKey(t))
		path := writeConf(t, body)
		logger, buf := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		output := buf.String()
		assert.Equal(t, 1, strings.Count(output, "Foo"), "expected exactly one WARN for unknown key Foo")
		assert.Contains(t, output, "test.conf")
	})

	t.Run("each warn-and-skip key produces one WARN line", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32
PreUp = iptables -A
PostUp = iptables -A
PreDown = iptables -D
PostDown = iptables -D
Table = off
FwMark = 0x1234
SaveConfig = true
ListenPort = 51820

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
`, genKey(t), genPubKey(t))
		path := writeConf(t, body)
		logger, buf := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		output := buf.String()
		for _, key := range []string{"PreUp", "PostUp", "PreDown", "PostDown", "Table", "FwMark", "SaveConfig", "ListenPort"} {
			assert.Equal(t, 1, strings.Count(output, key), "expected exactly one WARN for key %s", key)
		}
	})

	t.Run("DNS with multiple comma-separated servers produces two addr entries", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32
DNS = 10.64.0.1, 1.1.1.1

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
`, genKey(t), genPubKey(t))
		path := writeConf(t, body)
		logger, _ := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		require.Len(t, cfg.Interface.DNS, 2)
		assert.Equal(t, netip.MustParseAddr("10.64.0.1"), cfg.Interface.DNS[0])
		assert.Equal(t, netip.MustParseAddr("1.1.1.1"), cfg.Interface.DNS[1])
	})

	t.Run("DNS with CIDR suffix returns error", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32
DNS = 10.64.0.1/32

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
`, genKey(t), genPubKey(t))
		path := writeConf(t, body)
		logger, _ := captureLogger()
		_, err := wgconf.Parse(path, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "DNS")
	})

	t.Run("keys without spaces around equals are parsed", func(t *testing.T) {
		t.Parallel()
		privKey := genKey(t)
		pubKey := genPubKey(t)
		body := fmt.Sprintf(`[Interface]
PrivateKey=%s
Address=10.66.0.5/32

[Peer]
PublicKey=%s
Endpoint=185.1.2.3:51820
`, privKey, pubKey)
		path := writeConf(t, body)
		logger, _ := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		assert.Equal(t, privKey, cfg.Interface.PrivateKey)
		assert.Equal(t, pubKey, cfg.Peer.PublicKey)
		assert.Equal(t, "185.1.2.3:51820", cfg.Peer.Endpoint)
	})

	t.Run("IPv6 endpoint parsed verbatim", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32

[Peer]
PublicKey = %s
Endpoint = [2001:db8::1]:51820
`, genKey(t), genPubKey(t))
		path := writeConf(t, body)
		logger, _ := captureLogger()
		cfg, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		assert.Equal(t, "[2001:db8::1]:51820", cfg.Peer.Endpoint)
	})

	t.Run("nil logger does not panic", func(t *testing.T) {
		t.Parallel()
		path := writeConf(t, minimalConf(t))
		assert.NotPanics(t, func() {
			cfg, err := wgconf.Parse(path, nil)
			require.NoError(t, err)
			require.NotNil(t, cfg)
		})
	})

	t.Run("source basename appears in WARN messages", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5/32
UnknownKey = value

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
`, genKey(t), genPubKey(t))
		path := writeConf(t, body)
		logger, buf := captureLogger()
		_, err := wgconf.Parse(path, logger)
		require.NoError(t, err)
		output := buf.String()
		assert.Contains(t, output, "test.conf",
			"WARN must include source basename, not full path")
		assert.False(t, strings.Contains(output, t.TempDir()),
			"WARN must not include full dir path")
	})

	t.Run("Address without CIDR prefix returns error", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = 10.66.0.5

[Peer]
PublicKey = %s
Endpoint = 185.1.2.3:51820
`, genKey(t), genPubKey(t))
		path := writeConf(t, body)
		logger, _ := captureLogger()
		_, err := wgconf.Parse(path, logger)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Address")
	})

	t.Run("nonexistent file returns error", func(t *testing.T) {
		t.Parallel()
		logger, _ := captureLogger()
		_, err := wgconf.Parse("/nonexistent/file.conf", logger)
		require.Error(t, err)
	})
}
