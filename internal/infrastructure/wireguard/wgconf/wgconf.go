// Package wgconf parses wg-quick(8) .conf files into a ParsedConfig the
// proxy can hand to internal/infrastructure/wireguard. Supports vanilla
// [Interface]/[Peer] only; wg-quick fluff (PreUp, Table, ...) is
// WARN-and-skip. Multi-peer (mesh) is rejected.
package wgconf

import (
	"bufio"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Parse reads path, parses its wg-quick body, and returns a ParsedConfig.
// opLog receives one WARN per unsupported or unknown key, tagged with the
// source filename. If opLog is nil, slog.Default() is used.
func Parse(path string, opLog *slog.Logger) (*ParsedConfig, error) {
	if opLog == nil {
		opLog = slog.Default()
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("wgconf: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	p := &parser{
		path:   path,
		source: filepath.Base(path),
		opLog:  opLog,
	}
	return p.parse(bufio.NewScanner(f))
}

// ParsedConfig is the wg-quick body in struct form, ready to map to
// wireguard.Options.
type ParsedConfig struct {
	Interface InterfaceSection
	Peer      PeerSection
}

// InterfaceSection holds the fields from the [Interface] block of a wg-quick .conf file.
type InterfaceSection struct {
	// PrivateKey is the local WireGuard private key in base64 (wgtypes format); required.
	PrivateKey string
	// Addresses is parsed from the Address key; required, at least one entry.
	Addresses []netip.Prefix
	// DNS is parsed from the DNS key; optional, defaults to [10.64.0.1].
	DNS []netip.Addr
	// MTU is parsed from the MTU key; optional, defaults to 1420.
	MTU int
}

// PeerSection holds the fields from the [Peer] block of a wg-quick .conf file.
type PeerSection struct {
	// PublicKey is the peer's WireGuard public key in base64; required.
	PublicKey string
	// Endpoint is the peer's UDP endpoint in "host:port" or "[v6]:port" form; required.
	Endpoint string
	// AllowedIPs is parsed from the AllowedIPs key; optional, defaults to [0.0.0.0/0, ::/0].
	AllowedIPs []netip.Prefix
	// PersistentKeepaliveSeconds is parsed from the PersistentKeepalive key;
	// optional, defaults to 25. Explicit 0 disables keepalive and is preserved.
	PersistentKeepaliveSeconds int
	// PresharedKey is the peer's pre-shared key in base64; optional.
	PresharedKey string
}

// warnAndSkipKeys are wg-quick keys we understand but intentionally skip.
var warnAndSkipKeys = map[string]bool{
	"preup":      true,
	"postup":     true,
	"predown":    true,
	"postdown":   true,
	"table":      true,
	"fwmark":     true,
	"saveconfig": true,
	"listenport": true,
}

const (
	sectionInterface = "[Interface]"
	sectionPeer      = "[Peer]"

	defaultMTU          = 1420
	defaultKeepalive    = 25
	defaultDNS          = "10.64.0.1"
	defaultAllowedIPsV4 = "0.0.0.0/0"
	defaultAllowedIPsV6 = "::/0"

	keepaliveUnset = -1
)

type parser struct {
	path   string
	source string
	opLog  *slog.Logger

	currentSection string
	peerCount      int

	iface struct {
		privateKey string
		addresses  []netip.Prefix
		dns        []netip.Addr
		mtu        int
	}
	peer struct {
		publicKey    string
		endpoint     string
		allowedIPs   []netip.Prefix
		keepalive    int
		presharedKey string
	}

	seenInterface bool
	seenPeer      bool
	keepaliveSet  bool
}

func (p *parser) parse(scanner *bufio.Scanner) (*ParsedConfig, error) {
	p.peer.keepalive = keepaliveUnset

	for scanner.Scan() {
		line := scanner.Text()

		// strip inline comments
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// section header
		if strings.HasPrefix(line, "[") {
			if err := p.handleSection(line); err != nil {
				return nil, err
			}
			continue
		}

		// key = value line
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			// malformed line, skip silently (wg-quick ignores them too)
			continue
		}
		rawKey := strings.TrimSpace(line[:eq])
		rawVal := strings.TrimSpace(line[eq+1:])

		if err := p.handleKey(rawKey, rawVal); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("wgconf: read %s: %w", p.path, err)
	}

	return p.build()
}

func (p *parser) handleSection(line string) error {
	switch line {
	case sectionInterface:
		p.currentSection = sectionInterface
		p.seenInterface = true
	case sectionPeer:
		p.peerCount++
		if p.peerCount > 1 {
			return fmt.Errorf("wgconf: %s: multi-peer not supported (found second [Peer] block)", p.source)
		}
		p.currentSection = sectionPeer
		p.seenPeer = true
	default:
		return fmt.Errorf("wgconf: %s: unknown section %q (only [Interface] and [Peer] are supported)", p.source, line)
	}
	return nil
}

func (p *parser) handleKey(rawKey, rawVal string) error {
	lower := strings.ToLower(rawKey)

	if warnAndSkipKeys[lower] {
		p.opLog.Warn("wgconf: unsupported key, skipping",
			"key", rawKey,
			"section", p.currentSection,
			"source", p.source,
		)
		return nil
	}

	switch p.currentSection {
	case sectionInterface:
		return p.handleInterfaceKey(rawKey, lower, rawVal)
	case sectionPeer:
		return p.handlePeerKey(rawKey, lower, rawVal)
	default:
		// key before any section — warn and skip
		p.opLog.Warn("wgconf: key outside section, skipping",
			"key", rawKey,
			"source", p.source,
		)
	}
	return nil
}

func (p *parser) handleInterfaceKey(rawKey, lower, rawVal string) error {
	switch lower {
	case "privatekey":
		p.iface.privateKey = rawVal
	case "address":
		prefixes, err := parseCommaSeparatedPrefixes(rawVal)
		if err != nil {
			return fmt.Errorf("wgconf: %s: Address: %w", p.source, err)
		}
		p.iface.addresses = append(p.iface.addresses, prefixes...)
	case "dns":
		addrs, err := parseCommaSeparatedAddrs(rawVal)
		if err != nil {
			return fmt.Errorf("wgconf: %s: DNS: %w", p.source, err)
		}
		p.iface.dns = append(p.iface.dns, addrs...)
	case "mtu":
		v, err := strconv.Atoi(rawVal)
		if err != nil {
			return fmt.Errorf("wgconf: %s: MTU: %w", p.source, err)
		}
		p.iface.mtu = v
	default:
		p.opLog.Warn("wgconf: unknown key, skipping",
			"key", rawKey,
			"section", p.currentSection,
			"source", p.source,
		)
	}
	return nil
}

func (p *parser) handlePeerKey(rawKey, lower, rawVal string) error {
	switch lower {
	case "publickey":
		p.peer.publicKey = rawVal
	case "endpoint":
		p.peer.endpoint = rawVal
	case "allowedips":
		prefixes, err := parseCommaSeparatedPrefixes(rawVal)
		if err != nil {
			return fmt.Errorf("wgconf: %s: AllowedIPs: %w", p.source, err)
		}
		p.peer.allowedIPs = append(p.peer.allowedIPs, prefixes...)
	case "persistentkeepalive":
		v, err := strconv.Atoi(rawVal)
		if err != nil {
			return fmt.Errorf("wgconf: %s: PersistentKeepalive: %w", p.source, err)
		}
		p.peer.keepalive = v
		p.keepaliveSet = true
	case "presharedkey":
		p.peer.presharedKey = rawVal
	default:
		p.opLog.Warn("wgconf: unknown key, skipping",
			"key", rawKey,
			"section", p.currentSection,
			"source", p.source,
		)
	}
	return nil
}

func (p *parser) build() (*ParsedConfig, error) {
	if !p.seenInterface {
		return nil, fmt.Errorf("wgconf: %s: missing required [Interface] section", p.source)
	}
	if !p.seenPeer {
		return nil, fmt.Errorf("wgconf: %s: missing required [Peer] section", p.source)
	}
	if p.iface.privateKey == "" {
		return nil, fmt.Errorf("wgconf: %s: [Interface] missing required key PrivateKey", p.source)
	}
	if len(p.iface.addresses) == 0 {
		return nil, fmt.Errorf("wgconf: %s: [Interface] missing required key Address", p.source)
	}
	if p.peer.publicKey == "" {
		return nil, fmt.Errorf("wgconf: %s: [Peer] missing required key PublicKey", p.source)
	}
	if p.peer.endpoint == "" {
		return nil, fmt.Errorf("wgconf: %s: [Peer] missing required key Endpoint", p.source)
	}

	// apply defaults
	dns := p.iface.dns
	if len(dns) == 0 {
		dns = []netip.Addr{netip.MustParseAddr(defaultDNS)}
	}

	mtu := p.iface.mtu
	if mtu == 0 {
		mtu = defaultMTU
	}

	allowedIPs := p.peer.allowedIPs
	if len(allowedIPs) == 0 {
		allowedIPs = []netip.Prefix{
			netip.MustParsePrefix(defaultAllowedIPsV4),
			netip.MustParsePrefix(defaultAllowedIPsV6),
		}
	}

	keepalive := p.peer.keepalive
	if !p.keepaliveSet {
		keepalive = defaultKeepalive
	}

	return &ParsedConfig{
		Interface: InterfaceSection{
			PrivateKey: p.iface.privateKey,
			Addresses:  p.iface.addresses,
			DNS:        dns,
			MTU:        mtu,
		},
		Peer: PeerSection{
			PublicKey:                  p.peer.publicKey,
			Endpoint:                   p.peer.endpoint,
			AllowedIPs:                 allowedIPs,
			PersistentKeepaliveSeconds: keepalive,
			PresharedKey:               p.peer.presharedKey,
		},
	}, nil
}

func parseCommaSeparatedPrefixes(val string) ([]netip.Prefix, error) {
	parts := strings.Split(val, ",")
	out := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("parse CIDR: %w", err)
		}
		out = append(out, prefix)
	}
	return out, nil
}

func parseCommaSeparatedAddrs(val string) ([]netip.Addr, error) {
	parts := strings.Split(val, ",")
	out := make([]netip.Addr, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		addr, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("parse IP: %w", err)
		}
		out = append(out, addr)
	}
	return out, nil
}
