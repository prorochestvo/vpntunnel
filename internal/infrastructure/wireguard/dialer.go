// Package wireguard provides a tunnel.Dialer backed by a userspace WireGuard
// device (wireguard-go) routed through a gVisor netstack TUN. The dialer
// owns the device's lifecycle and must be Close()d on shutdown.
//
// DNS resolution for CONNECT targets is performed inside the tunnel via the
// DNS servers passed in Options.DNSServers (typically the tunnel gateway, e.g.
// 10.64.0.1 for Mullvad). For DNS to work, Options.AllowedIPs must cover the
// DNS server address — the default ["0.0.0.0/0", "::/0"] covers all addresses;
// trimming it to a narrower prefix that excludes the DNS server will cause all
// name lookups to hang until DialTimeout.
package wireguard

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"vpntunnel/internal/tunnel"
)

// compile-time assertions that WireGuardDialer satisfies all four interfaces.
var (
	_ tunnel.Dialer         = (*WireGuardDialer)(nil)
	_ tunnel.DialerCloser   = (*WireGuardDialer)(nil)
	_ tunnel.HealthReporter = (*WireGuardDialer)(nil)
	_ tunnel.Resolver       = (*WireGuardDialer)(nil)
)

// NewDialer builds the WireGuard device and brings it up administratively.
// It does NOT block waiting for a handshake — the handshake is lazy and
// happens on the first packet sent via DialContext. If construction fails
// (key parse error, netstack init failure, IpcSet rejection), the device is
// torn down before NewDialer returns the error.
//
// ctx is used for the one-time DNS resolution of opts.PeerEndpoint. A
// SIGINT-notified context causes a hung DNS lookup to be aborted. Once
// NewDialer returns, ctx is no longer used — the dialer is self-contained.
//
// The caller must call Close() when done to release goroutines and the UDP
// port held by the wireguard-go device. Close is idempotent.
func NewDialer(ctx context.Context, opts Options) (*WireGuardDialer, error) {
	if opts.Logger == nil {
		panic("wireguard.NewDialer: Logger is required")
	}

	endpointAddr, err := resolveEndpoint(ctx, opts.PeerEndpoint)
	if err != nil {
		return nil, fmt.Errorf("wireguard: resolve peer endpoint: %w", err)
	}

	tunDev, tnet, err := netstack.CreateNetTUN(opts.LocalAddresses, opts.DNSServers, opts.MTU)
	if err != nil {
		return nil, fmt.Errorf("wireguard: create netstack TUN: %w", err)
	}

	wgLogger := newDeviceLogger(opts.Logger)
	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), wgLogger)

	uapi, err := buildIpcSet(opts, endpointAddr)
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("wireguard: build UAPI config: %w", err)
	}

	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wireguard: IpcSet: %w", err)
	}

	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wireguard: device Up: %w", err)
	}

	return &WireGuardDialer{
		dev:  dev,
		ipc:  dev,
		tnet: tnet,
	}, nil
}

// Options configures a WireGuardDialer. All fields are required unless
// documented otherwise. The dialer takes ownership of all resolved values
// at construction time; changing Options after NewDialer returns has no effect.
type Options struct {
	// PrivateKey is the local WireGuard private key in base64 (wgtypes) format.
	PrivateKey string
	// LocalAddresses are the tunnel-side IP addresses for this end.
	LocalAddresses []netip.Addr
	// DNSServers are the DNS resolver IPs accessible inside the tunnel.
	// Must be covered by AllowedIPs or DNS resolution inside the tunnel will fail.
	DNSServers []netip.Addr
	// MTU is the tunnel MTU, typically 1420.
	MTU int
	// PeerPublicKey is the peer's WireGuard public key in base64.
	PeerPublicKey string
	// PeerEndpoint is the peer's UDP endpoint in "host:port" form.
	// The hostname is resolved once at NewDialer time via system DNS.
	PeerEndpoint string
	// AllowedIPs is the list of IP prefixes routed through the tunnel.
	AllowedIPs []netip.Prefix
	// PersistentKeepaliveSeconds is the keepalive interval in seconds. 0 disables.
	PersistentKeepaliveSeconds int
	// PresharedKey is the pre-shared key in base64; empty disables.
	// When set, buildIpcSet emits preshared_key=<64 hex chars> in the peer block.
	PresharedKey string
	// Logger bridges wireguard-go's internal logging to slog. Required;
	// NewDialer panics if nil (implementation bug, not a config error).
	Logger *slog.Logger
}

// WireGuardDialer implements tunnel.Dialer and tunnel.DialerCloser.
// It routes all outbound connections through a userspace WireGuard device.
// It is safe for concurrent use after construction. The caller must call
// Close() on shutdown to release the wireguard-go goroutines and UDP port.
type WireGuardDialer struct {
	dev       *device.Device
	ipc       ipcGetter // defaults to dev; overridable in tests via the ipc field
	tnet      *netstack.Net
	closeOnce sync.Once
}

// Close tears down the WireGuard device, joins its goroutines, and releases
// the UDP port. It is safe to call more than once; subsequent calls are no-ops.
// Also tolerates a nil device, which is the state of partially-constructed
// stubs used by LastHandshake unit tests.
func (d *WireGuardDialer) Close() error {
	d.closeOnce.Do(func() {
		if d.dev != nil {
			d.dev.Close()
		}
	})
	return nil
}

// DialContext opens a TCP connection to address through the WireGuard tunnel,
// honouring ctx for cancellation. DNS for address is resolved inside the
// tunnel via the DNSServers passed to NewDialer. If the WireGuard handshake
// has not completed (lazy), this call initiates it and waits; a dead peer
// causes the dial to fail with a timeout error once ctx expires.
func (d *WireGuardDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := d.tnet.DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("wireguard: dial %s: %w", address, err)
	}
	return conn, nil
}

// LookupHost resolves host via the tunnel's DNS (netstack-backed resolver).
// Returns netip.Addr values; non-parseable strings from netstack are dropped
// silently (defensive against future netstack API changes). The returned slice
// is nil-safe: a non-nil empty slice means the host resolved but had no
// addresses (NXDOMAIN / empty answer). Safe for concurrent use.
func (d *WireGuardDialer) LookupHost(ctx context.Context, host string) ([]netip.Addr, error) {
	strs, err := d.tnet.LookupContextHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("wireguard: lookup %s: %w", host, err)
	}
	addrs := make([]netip.Addr, 0, len(strs))
	for _, s := range strs {
		a, parseErr := netip.ParseAddr(s)
		if parseErr != nil {
			// skip non-parseable; defensive against netstack returning unexpected strings.
			continue
		}
		addrs = append(addrs, a)
	}
	return addrs, nil
}

// newDeviceLogger builds a wireguard-go device.Logger that bridges to slog.
// Verbosef maps to Debug (the wireguard-go internals are very chatty at this
// level; an Info-level operational logger drops them automatically). Errorf
// maps to Error. Both fields must be non-nil per device.Logger's contract.
func newDeviceLogger(l *slog.Logger) *device.Logger {
	return &device.Logger{
		// Verbosef maps to slog Debug. At the default info level these records are
		// suppressed. When debug is enabled, wireguard-go emits peer public key
		// fingerprints — accepted (public key is not secret) but documented here.
		Verbosef: func(format string, args ...any) {
			l.Debug(fmt.Sprintf(format, args...))
		},
		Errorf: func(format string, args ...any) {
			l.Error(fmt.Sprintf(format, args...))
		},
	}
}
