// Command httpproxy is a forward HTTP proxy that routes all egress through a
// userspace WireGuard tunnel. It supports plain-HTTP forwarding (absolute-URI
// form) and HTTPS tunnelling via CONNECT.
//
// Tunnel configuration is read from a wg-quick .conf file whose path is listed
// in configs/proxy.json under upstream.configs. The active tunnel is selected by
// upstream.active (basename without ".conf"); when active is empty, the first
// entry in configs is used.
//
// Bind address defaults to 127.0.0.1:8080. The WireGuard private key in the
// .conf file must be treated like an SSH key (file mode 0600, never logged).
//
// Optional Bearer-token authentication is configured via the auth block in
// proxy.json. Set auth.token for an inline token or auth.token_file for a
// path to a file containing the token. The token is never logged. When the
// auth block is absent, the proxy serves all clients that can reach listen.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"httpproxy/internal/auth"
	"httpproxy/internal/config"
	"httpproxy/internal/health"
	"httpproxy/internal/observability"
	"httpproxy/internal/service"
	"httpproxy/internal/transport/adminserver"
	"httpproxy/internal/transport/httpserver"
	"httpproxy/internal/tunnel"
	"httpproxy/internal/tunnel/wireguard"
	"httpproxy/internal/tunnel/wireguard/wgconf"
)

func main() {
	var configPath string
	var healthcheckMode bool
	var healthcheckAddr string
	flag.StringVar(&configPath, "config", "./configs/proxy.json", "path to JSON config file")
	flag.BoolVar(&healthcheckMode, "healthcheck", false, "run as healthcheck probe and exit")
	flag.StringVar(&healthcheckAddr, "healthcheck-addr", "127.0.0.1:8081", "addr to probe in healthcheck mode")
	flag.Parse()

	if healthcheckMode {
		os.Exit(runHealthcheck(healthcheckAddr))
	}

	if err := run(configPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// resolveActiveTunnel returns the absolute path of the .conf to load,
// given the validated Upstream block and the directory of proxy.json.
// Paths in upstream.Configs are joined onto configDir if relative;
// already-absolute paths are returned as-is.
//
// Precondition: up.Configs has at least one entry (guaranteed by config.Validate).
func resolveActiveTunnel(up config.Upstream, configDir string, opLog *slog.Logger) (string, error) {
	target := up.Active
	if target == "" {
		target = strings.TrimSuffix(filepath.Base(up.Configs[0]), ".conf")
		opLog.Info("upstream.active empty; using first config",
			"active", target,
			"source", filepath.Base(up.Configs[0]),
		)
	}
	for _, p := range up.Configs {
		if strings.TrimSuffix(filepath.Base(p), ".conf") == target {
			if filepath.IsAbs(p) {
				return p, nil
			}
			return filepath.Join(configDir, p), nil
		}
	}
	// unreachable if config.Validate() ran; defensive.
	return "", fmt.Errorf("active tunnel %q not found in configs", target)
}

// resolveAuthToken returns the Bearer token to enforce. An empty string means
// auth is disabled. When cfg.TokenFile is set, the file is read and its
// contents are trimmed of surrounding whitespace; relative paths are resolved
// against configDir like resolveActiveTunnel. The token value is never
// returned in any error message.
//
// Precondition: not both Token and TokenFile are non-empty (enforced by config
// validation). If that precondition is violated, Token takes precedence.
func resolveAuthToken(a config.Auth, configDir string) (string, error) {
	if a.Token != "" {
		return strings.TrimSpace(a.Token), nil
	}
	if a.TokenFile == "" {
		return "", nil
	}
	path := a.TokenFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(configDir, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read auth.token_file %s: %w", path, err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", fmt.Errorf("auth.token_file %s: token is empty after trim", path)
	}
	return tok, nil
}

// buildWireGuardOptions maps a parsed wg-quick body to wireguard.Options.
// Interface.Addresses are prefixes (CIDR); wireguard.Options.LocalAddresses
// wants plain addrs, so we extract the host part via Prefix.Addr().
func buildWireGuardOptions(p *wgconf.ParsedConfig, opLog *slog.Logger) wireguard.Options {
	localAddrs := make([]netip.Addr, len(p.Interface.Addresses))
	for i, prefix := range p.Interface.Addresses {
		localAddrs[i] = prefix.Addr()
	}
	return wireguard.Options{
		PrivateKey:                 p.Interface.PrivateKey,
		LocalAddresses:             localAddrs,
		DNSServers:                 p.Interface.DNS,
		MTU:                        p.Interface.MTU,
		PeerPublicKey:              p.Peer.PublicKey,
		PeerEndpoint:               p.Peer.Endpoint,
		AllowedIPs:                 p.Peer.AllowedIPs,
		PersistentKeepaliveSeconds: p.Peer.PersistentKeepaliveSeconds,
		PresharedKey:               p.Peer.PresharedKey,
		Logger:                     opLog,
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	opLog := observability.NewOperationalLogger(cfg.Operational)

	access, err := observability.NewAccessLogger(cfg.AccessLog, opLog)
	if err != nil {
		return fmt.Errorf("open access log: %w", err)
	}
	// deferred first so it closes AFTER wgClose (LIFO); final access-log writes
	// for in-flight CONNECTs can still be emitted while wgClose runs.
	defer func() { _ = access.Close() }()

	// set up the signal context early so that a hung startup DNS lookup
	// is interrupted by SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	configDir := filepath.Dir(configPath)
	activePath, err := resolveActiveTunnel(cfg.Upstream, configDir, opLog)
	if err != nil {
		return fmt.Errorf("resolve active tunnel: %w", err)
	}

	parsed, err := wgconf.Parse(activePath, opLog)
	if err != nil {
		return fmt.Errorf("parse tunnel config %s: %w", activePath, err)
	}

	opts := buildWireGuardOptions(parsed, opLog)

	wgDialer, err := wireguard.NewDialer(ctx, opts)
	if err != nil {
		return fmt.Errorf("init wireguard dialer: %w", err)
	}
	// wgClose deferred AFTER access.Close (which was deferred above). LIFO
	// means this runs FIRST: WG device is torn down before access.Close flushes
	// any remaining access-log writes.
	defer func() {
		if cerr := wgDialer.Close(); cerr != nil {
			opLog.Warn("wireguard dialer close failed", "err", cerr)
		}
	}()
	// hold as interface for the service; no unnecessary concrete type exposure.
	var dialer tunnel.Dialer = wgDialer

	// log peer_endpoint, local_address, and source (basename only).
	// never log the private key, PSK, or peer public key.
	opLog.Info("upstream wireguard ready",
		"source", filepath.Base(activePath),
		"peer_endpoint", opts.PeerEndpoint,
		"local_address", opts.LocalAddresses[0].String(),
	)

	authToken, err := resolveAuthToken(cfg.Auth, configDir)
	if err != nil {
		return fmt.Errorf("resolve auth token: %w", err)
	}

	var verifier auth.Verifier
	if authToken != "" {
		verifier = auth.NewBearerVerifier(authToken)
		source := "inline"
		if cfg.Auth.TokenFile != "" {
			source = "file=" + filepath.Base(cfg.Auth.TokenFile)
		}
		opLog.Info("auth enabled",
			slog.String("source", source),
			slog.Int("token_len", len(authToken)),
		)
	} else {
		opLog.Info("auth disabled", slog.String("listen", cfg.Listen))
	}

	// wgDialer also implements tunnel.HealthReporter; the concrete type is passed
	// directly so the implicit interface satisfaction works at compile time.
	healthHandler := health.NewHandler(wgDialer, cfg.Health.HandshakeMaxAge, opLog)
	adminSrv := adminserver.New(adminserver.Options{
		Addr:          cfg.Admin.Listen,
		HealthHandler: healthHandler,
	}, opLog)

	svc := service.NewProxyService(service.ProxyServiceOptions{
		Dialer:      dialer,
		Verifier:    verifier,
		Access:      access,
		OpLog:       opLog,
		DialTimeout: cfg.DialTimeout,
	})

	srv := httpserver.New(cfg, svc, opLog)

	errCh := make(chan error, 2)
	go func() { errCh <- srv.Start() }()
	go func() { errCh <- adminSrv.Start() }()

	select {
	case err := <-errCh:
		// one side failed to start; treat as fatal startup failure.
		// the other goroutine's send is non-blocking (buffer size 2)
		// so we don't leak it even though we never read its value.
		return fmt.Errorf("server start: %w", err)
	case <-ctx.Done():
		opLog.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	var shutdownErr error
	if err := srv.Shutdown(shutdownCtx); err != nil {
		opLog.Error("proxy shutdown failed", "err", err.Error())
		shutdownErr = fmt.Errorf("proxy shutdown: %w", err)
	}

	adminShutdownCtx, adminCancel := context.WithTimeout(context.Background(), cfg.Admin.ShutdownTimeout)
	defer adminCancel()
	if err := adminSrv.Shutdown(adminShutdownCtx); err != nil {
		opLog.Error("admin shutdown failed", "err", err.Error())
		if shutdownErr == nil {
			shutdownErr = fmt.Errorf("admin shutdown: %w", err)
		}
	}

	if shutdownErr != nil {
		return shutdownErr
	}
	opLog.Info("shutdown complete")
	return nil
}
