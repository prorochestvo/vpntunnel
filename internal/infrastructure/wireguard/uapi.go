package wireguard

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// buildIpcSet constructs the UAPI configuration string for device.IpcSet.
// Keys are hex-encoded (64-char hex strings), not base64 — UAPI requires hex
// even though wgtypes.Key.String() uses base64. The caller is responsible for
// pre-resolving opts.PeerEndpoint to a literal IP:port via resolveEndpoint
// before building the UAPI string.
// When opts.PresharedKey is non-empty (base64), it is decoded and emitted as
// preshared_key=<64 hex chars> inside the peer block.
func buildIpcSet(opts Options, endpointAddr netip.AddrPort) (string, error) {
	privKey, err := wgtypes.ParseKey(opts.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	peerKey, err := wgtypes.ParseKey(opts.PeerPublicKey)
	if err != nil {
		return "", fmt.Errorf("parse peer key: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", hex.EncodeToString(privKey[:]))
	fmt.Fprintf(&b, "public_key=%s\n", hex.EncodeToString(peerKey[:]))
	// endpointAddr.String() produces the bracketed IPv6 form when needed.
	fmt.Fprintf(&b, "endpoint=%s\n", endpointAddr.String())
	if opts.PresharedKey != "" {
		psk, pskErr := wgtypes.ParseKey(opts.PresharedKey)
		if pskErr != nil {
			return "", fmt.Errorf("parse preshared key: %w", pskErr)
		}
		fmt.Fprintf(&b, "preshared_key=%s\n", hex.EncodeToString(psk[:]))
	}
	fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", opts.PersistentKeepaliveSeconds)
	for _, p := range opts.AllowedIPs {
		fmt.Fprintf(&b, "allowed_ip=%s\n", p.String())
	}
	return b.String(), nil
}

// resolveEndpoint resolves the host part of a "host:port" endpoint to a
// literal IP address, returning a netip.AddrPort ready for the UAPI
// endpoint= directive. The resolution uses the system DNS resolver;
// this is a deliberate one-time control-plane leak accepted by the operator.
// If the host is already a numeric IP, no lookup is performed.
// ctx is honoured for cancellation — a SIGINT during startup will abort a
// hung DNS lookup instead of blocking indefinitely.
func resolveEndpoint(ctx context.Context, endpoint string) (netip.AddrPort, error) {
	host, portStr, err := net.SplitHostPort(endpoint)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("split host:port: %w", err)
	}
	port, err := net.LookupPort("udp", portStr)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("parse port: %w", err)
	}

	// if the host is already a numeric IP, skip DNS.
	if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
		return netip.AddrPortFrom(addr, uint16(port)), nil
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return netip.AddrPort{}, fmt.Errorf("resolve %s: no addresses returned", host)
	}
	// prefer IPv4 to avoid issues with IPv6-only tunnels
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			continue
		}
		if ip.Is4() {
			return netip.AddrPortFrom(ip.Unmap(), uint16(port)), nil
		}
	}
	// fall back to first address
	ip, ok := netip.AddrFromSlice(addrs[0].IP)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("resolve %s: could not convert IP", host)
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(port)), nil
}
