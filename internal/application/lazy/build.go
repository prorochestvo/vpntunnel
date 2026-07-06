package lazy

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"path/filepath"
	"strings"

	"vpntunnel/internal/infrastructure/wireguard"
	"vpntunnel/internal/infrastructure/wireguard/wgconf"
	"vpntunnel/internal/tunnel"
)

// BuilderFn constructs one live dialer from a parsed config. The production
// implementation calls wireguard.NewDialer; tests inject a fake that returns an
// in-memory dialer. The function must not retain parsed after it returns.
type BuilderFn func(ctx context.Context, parsed *wgconf.ParsedConfig, opLog *slog.Logger) (tunnel.DialerCloser, error)

// DefaultBuilder is the production BuilderFn. It maps a ParsedConfig to
// wireguard.Options and calls wireguard.NewDialer. Tests must inject a fake via
// BuildDialer's optional override rather than calling this directly.
func DefaultBuilder(ctx context.Context, parsed *wgconf.ParsedConfig, opLog *slog.Logger) (tunnel.DialerCloser, error) {
	localAddrs := make([]netip.Addr, len(parsed.Interface.Addresses))
	for i, prefix := range parsed.Interface.Addresses {
		localAddrs[i] = prefix.Addr()
	}
	opts := wireguard.Options{
		PrivateKey:                 parsed.Interface.PrivateKey,
		LocalAddresses:             localAddrs,
		DNSServers:                 parsed.Interface.DNS,
		MTU:                        parsed.Interface.MTU,
		PeerPublicKey:              parsed.Peer.PublicKey,
		PeerEndpoint:               parsed.Peer.Endpoint,
		AllowedIPs:                 parsed.Peer.AllowedIPs,
		PersistentKeepaliveSeconds: parsed.Peer.PersistentKeepaliveSeconds,
		PresharedKey:               parsed.Peer.PresharedKey,
		Logger:                     opLog,
	}
	return wireguard.NewDialer(ctx, opts)
}

// BuildDialer resolves configPath against configDir when relative, parses the
// wg-quick .conf file, and builds a live tunnel.DialerCloser using fn (or
// DefaultBuilder when fn is nil). On success it logs tunnel_id, peer_endpoint,
// and local_address — never key material. On failure it returns a plain wrapped
// error.
//
// This is the single choke point that both the streaming supervisor and the
// on-demand scheduler call to bring a WireGuard device up. It enforces nothing
// about the two-device cap; callers own that invariant.
func BuildDialer(ctx context.Context, configPath, configDir string, opLog *slog.Logger, fn BuilderFn) (tunnel.DialerCloser, error) {
	if fn == nil {
		fn = DefaultBuilder
	}

	path := configPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(configDir, path)
	}

	id := strings.TrimSuffix(filepath.Base(path), ".conf")

	parsed, err := wgconf.Parse(path, opLog)
	if err != nil {
		return nil, fmt.Errorf("lazy: parse %s: %w", id, err)
	}

	d, err := fn(ctx, parsed, opLog)
	if err != nil {
		return nil, fmt.Errorf("lazy: build %s: %w", id, err)
	}

	opLog.Info("tunnel built",
		slog.String("tunnel_id", id),
		slog.String("peer_endpoint", parsed.Peer.Endpoint),
		slog.String("local_address", firstLocalAddress(parsed)),
	)

	return d, nil
}

// firstLocalAddress returns the first Interface.Addresses entry as a string,
// or "" when the slice is empty.
func firstLocalAddress(parsed *wgconf.ParsedConfig) string {
	if len(parsed.Interface.Addresses) == 0 {
		return ""
	}
	return parsed.Interface.Addresses[0].String()
}
