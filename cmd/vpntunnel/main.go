// Command vpntunnel is a forward HTTP proxy that routes all egress through a
// userspace WireGuard tunnel. It supports plain-HTTP forwarding (absolute-URI
// form) and HTTPS tunnelling via CONNECT.
//
// Tunnel configuration is discovered automatically from the tunnels/
// subdirectory of the directory containing proxy.json. Drop wg-quick .conf
// files into configs/tunnels/ and they are picked up on the next startup
// without any change to proxy.json. Discovered configs feed two views: a
// country-filtered set drives the always-on streaming supervisor; the full set
// drives the on-demand scheduler and the API zone checker (on-demand may route
// to any discovered zone regardless of allowed_countries).
//
// Bind address defaults to 127.0.0.1:7788. The WireGuard private key in the
// .conf file must be treated like an SSH key (file mode 0600, never logged).
//
// Optional Bearer-token authentication for the plain proxy listener is
// configured via the vpnstream.auth block in proxy.json. Set auth.token for an
// inline token or auth.token_file for a path to a file containing the token.
// The token is never logged. When the auth block is absent, the proxy serves
// all clients that can reach listen.
//
// The HTTPS API listener on the address set by api.listen (default
// 127.0.0.1:8888) requires one of three Bearer tokens: admin, deploy, or
// proxy. See the api block in proxy.json for details.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"vpntunnel/internal/asyncjob"
	"vpntunnel/internal/auth"
	"vpntunnel/internal/config"
	"vpntunnel/internal/notify"
	"vpntunnel/internal/observability"
	"vpntunnel/internal/service"
	"vpntunnel/internal/transport/apiserver"
	"vpntunnel/internal/transport/apiserver/apitls"
	"vpntunnel/internal/transport/apiserver/handlers"
	"vpntunnel/internal/transport/httpserver"
	lazy "vpntunnel/internal/tunnel/lazy"
)

// runOpt is a functional option for runWithOpts, used to override internals in
// tests without altering the production code path.
type runOpt func(*runOptions)

// runOptions holds optional overrides for run. Zero value is the production
// configuration (real WireGuard builder, no injection).
type runOptions struct {
	// supervisorBuilder overrides the DeviceBuilderFn for the streaming supervisor.
	// When nil (the default), DefaultDeviceBuilder is used.
	supervisorBuilder lazy.DeviceBuilderFn
	// schedulerBuilder overrides the DeviceBuilderFn for the on-demand scheduler.
	// When nil (the default), DefaultDeviceBuilder is used.
	schedulerBuilder lazy.DeviceBuilderFn
	// shutdownCtx, when non-nil, replaces signal.NotifyContext as the
	// cancellation source. Test-only seam — production omits this to get the
	// default signal-handling behaviour. The matching cancel func is held by
	// the caller; runWithOpts never calls it.
	shutdownCtx context.Context
}

// withSupervisorBuilder returns a runOpt that injects a fake DeviceBuilderFn
// into the streaming supervisor. Intended for tests only.
func withSupervisorBuilder(b lazy.DeviceBuilderFn) runOpt {
	return func(o *runOptions) { o.supervisorBuilder = b }
}

// withSchedulerBuilder returns a runOpt that injects a fake DeviceBuilderFn
// into the on-demand scheduler. Intended for tests only.
func withSchedulerBuilder(b lazy.DeviceBuilderFn) runOpt {
	return func(o *runOptions) { o.schedulerBuilder = b }
}

// withShutdownCtx returns a runOpt that replaces signal.NotifyContext with the
// caller-owned context as the shutdown trigger. Test-only seam.
func withShutdownCtx(ctx context.Context) runOpt {
	return func(o *runOptions) { o.shutdownCtx = ctx }
}

// tlsOptions carries the TLS settings sourced from CLI flags, parsed and
// validated before runWithOpts is called. Relative cert-dir paths are resolved
// against the process cwd during parsing; production default is absolute.
type tlsOptions struct {
	// CertDir is the directory holding the API TLS cert/key. Always absolute.
	CertDir string
	// Hostname is the TLS server name embedded in the certificate.
	Hostname string
	// IPSANs is the optional list of IP Subject Alternative Names.
	IPSANs []net.IP
}

// parseTLSOptions validates and resolves the raw CLI flag values into a
// tlsOptions ready for use. An empty certDir is the HTTP-mode trigger: it is
// preserved as-is (not resolved via filepath.Abs, which would return the cwd
// and silently re-enable HTTPS). A non-empty relative CertDir is resolved
// against the process cwd via filepath.Abs. Returns a descriptive error for
// any invalid input.
func parseTLSOptions(certDir, hostname, ipSANsRaw string) (tlsOptions, error) {
	if hostname == "" {
		return tlsOptions{}, errors.New("-tls-hostname: must not be empty")
	}

	// parse IP SANs once; both the HTTP-mode early return and the HTTPS path use
	// the same result.
	var ipSANs []net.IP
	for _, raw := range strings.Split(ipSANsRaw, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		ip := net.ParseIP(raw)
		if ip == nil {
			return tlsOptions{}, fmt.Errorf("-tls-ip-sans: %q is not a valid IP address", raw)
		}
		ipSANs = append(ipSANs, ip)
	}

	if certDir == "" {
		// empty cert dir is the HTTP-mode trigger: leave CertDir empty (do NOT
		// resolve it to an absolute path, which would yield the cwd and silently
		// re-enable HTTPS). Hostname stays required but its only use is the cert
		// SAN, so it is harmless when no cert is generated.
		return tlsOptions{CertDir: "", Hostname: hostname, IPSANs: ipSANs}, nil
	}

	resolvedDir := certDir
	if !filepath.IsAbs(certDir) {
		abs, err := filepath.Abs(certDir)
		if err != nil {
			return tlsOptions{}, fmt.Errorf("-tls-cert-dir: resolve %q: %w", certDir, err)
		}
		resolvedDir = abs
	}

	return tlsOptions{
		CertDir:  resolvedDir,
		Hostname: hostname,
		IPSANs:   ipSANs,
	}, nil
}

func main() {
	var configPath string
	var tlsCertDir string
	var tlsHostname string
	var tlsIPSANsRaw string
	flag.StringVar(&configPath, "config", "./configs/proxy.json", "path to JSON config file")
	flag.StringVar(&tlsCertDir, "tls-cert-dir", "", "directory holding the API TLS cert/key; empty runs the API as plain HTTP (loopback/dev only). Relative resolves against CWD.")
	flag.StringVar(&tlsHostname, "tls-hostname", "localhost", "TLS server name embedded in the API certificate")
	flag.StringVar(&tlsIPSANsRaw, "tls-ip-sans", "", "comma-separated list of IP Subject Alternative Names")
	flag.Parse()

	// resolve to an absolute path immediately so that configDir, tunnelsDir, and
	// every operator-facing error message are absolute and cwd-independent.
	absPath, err := filepath.Abs(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, fmt.Errorf("resolve config path %q: %w", configPath, err))
		os.Exit(1)
	}

	tlsOpts, err := parseTLSOptions(tlsCertDir, tlsHostname, tlsIPSANsRaw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := run(absPath, tlsOpts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is the production entry point. It delegates to runWithOpts with no
// option overrides.
func run(configPath string, tlsOpts tlsOptions) error {
	return runWithOpts(configPath, tlsOpts)
}

// resolveAuthToken returns the Bearer token to enforce. An empty string means
// auth is disabled. When a.TokenFile is set, the file is read and its contents
// are trimmed of surrounding whitespace; relative paths are resolved against
// configDir. The token value is never returned in any error message.
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

// runWithOpts is the full startup / run / shutdown path. tlsOpts carries the TLS
// settings resolved from CLI flags. opts allow tests to inject fakes (e.g. a
// fake device builder) without altering the production path.
func runWithOpts(configPath string, tlsOpts tlsOptions, opts ...runOpt) error {
	var ro runOptions
	for _, o := range opts {
		o(&ro)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// wrap the base slog handler with a scrub handler that redacts host:port
	// patterns from all operational log messages and attributes.
	baseOpLog := observability.NewOperationalLogger(cfg.Operational)
	scrubbed := observability.NewScrubHandler(baseOpLog.Handler())
	opLog := slog.New(scrubbed)
	opLog.Info("operational log scrubbing active", slog.String("strategy", "host-port-redact"))

	// build the Telegram tunnel-change notifier from the environment. Absent or
	// malformed input both leave the proxy running: a bad DSN is auxiliary
	// telemetry misconfiguration, never a reason to crash-loop the actual
	// product. tgNotifier is held separately (rather than type-asserting
	// notifier later) purely for its Close lifecycle at shutdown.
	var notifier notify.Notifier = notify.Nop{}
	var tgNotifier *notify.TelegramNotifier
	if dsn := os.Getenv("VPNTUNNEL_TELEGRAMBOT_DSN"); dsn != "" {
		tn, err := notify.NewTelegram(dsn, opLog)
		if err != nil {
			// NewTelegram returns a redacted error (never the DSN or the bot
			// token); warn and keep running with notifications disabled.
			opLog.Warn("telegram notifier disabled: invalid VPNTUNNEL_TELEGRAMBOT_DSN",
				slog.String("err", err.Error()),
			)
		} else {
			// NewTelegram already logs "telegram notifier enabled" with token_len;
			// do not log a second, redundant line here.
			notifier = tn
			tgNotifier = tn
		}
	} else {
		opLog.Info("telegram notifier disabled: VPNTUNNEL_TELEGRAMBOT_DSN not set")
	}

	// build the access-log sanitizer slice from the compiled patterns in cfg.
	sanitizers := make([]observability.PathSanitizePattern, 0, len(cfg.API.Log.PathSanitizePatterns))
	for _, p := range cfg.API.Log.PathSanitizePatterns {
		sanitizers = append(sanitizers, observability.PathSanitizePattern{
			Regexp:      p.Pattern,
			Replacement: p.Replacement,
		})
	}
	access, err := observability.NewAccessLogger(cfg.AccessLog, opLog, sanitizers)
	if err != nil {
		return fmt.Errorf("open access log: %w", err)
	}
	opLog.Info("access log sanitiser configured", slog.Int("patterns", len(sanitizers)))
	defer func() { _ = access.Close() }()

	// set up the shutdown context early so that a hung startup DNS lookup is
	// interrupted. In production, signal.NotifyContext is used; in tests a
	// caller-owned context is injected via withShutdownCtx to avoid sending
	// SIGTERM to the entire test process.
	var ctx context.Context
	var stop context.CancelFunc
	if ro.shutdownCtx != nil {
		ctx, stop = context.WithCancel(ro.shutdownCtx)
	} else {
		ctx, stop = signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	}
	defer stop()

	configDir := filepath.Dir(configPath)

	// discover all *.conf files in <configDir>/tunnels/. Discovery never parses
	// file contents — it returns a sorted list of absolute paths.
	tunnelsDir := filepath.Join(configDir, "tunnels")
	discovered, err := lazy.DiscoverConfigs(tunnelsDir)
	if err != nil {
		return fmt.Errorf("discover tunnel configs: %w", err)
	}
	opLog.Info("tunnel configs discovered",
		slog.String("dir", tunnelsDir),
		slog.Int("count", len(discovered)),
	)

	hmacKey, err := loadOrGenerateTunnelIDKey(cfg.TunnelIDHMACKeyFile, configDir, opLog)
	if err != nil {
		return fmt.Errorf("load tunnel-id hmac key: %w", err)
	}

	// build the full set first — covers every discovered config, used by the
	// on-demand scheduler and the API ZoneChecker. An empty discovered set is
	// caught here before the country-filter check below.
	fullSet, err := lazy.NewFullSet(discovered, configDir, hmacKey)
	if err != nil {
		return fmt.Errorf("build full tunnel set: %w", err)
	}
	// zero the key immediately after NewFullSet consumes it so the 32-byte secret
	// does not persist on the stack for the daemon lifetime.
	for i := range hmacKey {
		hmacKey[i] = 0
	}
	hmacKey = nil

	// build the country-filtered streaming set — feeds only the always-on
	// streaming supervisor's random pick. An empty result after filtering means
	// allowed_countries matches nothing; the supervisor is mandatory so this is
	// a fatal startup error (RandomPath panics on an empty set).
	streamingSet, err := lazy.NewEligibleSet(discovered, configDir, cfg.VPNStream.AllowedCountries)
	if err != nil {
		return fmt.Errorf("build streaming tunnel set: %w", err)
	}

	// single-key guard runs over the full discovered set (pre-country-filter) so
	// the single-account invariant is enforced across all configs the operator
	// deposited, not just the ones currently enabled by allowed_countries.
	if err := lazy.VerifySingleKey(discovered, configDir, opLog); err != nil {
		return fmt.Errorf("single-key guard: %w", err)
	}

	// build the streaming supervisor (always-on role).
	supervisor := lazy.NewStreamingSupervisor(lazy.SupervisorOptions{
		Eligible:        streamingSet,
		DeviceBuilder:   ro.supervisorBuilder,
		HandshakeMaxAge: lazy.DefaultHandshakeMaxAge,
		ReconnectMin:    cfg.VPNStream.ReconnectMin,
		ReconnectMax:    cfg.VPNStream.ReconnectMax,
		// RotateSettle reuses the operator-tuned on-demand settle delay so the
		// streaming role's rotate honours the same Mullvad-session-free window
		// without the lazy package importing on-demand config. SettleDelay
		// always resolves to a valid value (default 15s, 5s minimum,
		// config.go validation).
		RotateSettle: cfg.API.VPN.Demand.SettleDelay,
		ConfigDir:    configDir,
		OpLog:        opLog,
		Notifier:     notifier,
	})
	if err := supervisor.Start(ctx); err != nil {
		return fmt.Errorf("start streaming supervisor: %w", err)
	}

	// build the on-demand scheduler.
	scheduler := lazy.NewOnDemandScheduler(lazy.SchedulerOptions{
		Eligible:      fullSet,
		DeviceBuilder: ro.schedulerBuilder,
		SettleDelay:   cfg.API.VPN.Demand.SettleDelay,
		Grace:         cfg.API.VPN.Demand.Grace,
		IdleTTL:       cfg.API.VPN.Demand.IdleTTL,
		ConfigDir:     configDir,
		OpLog:         opLog,
		Notifier:      notifier,
	})
	var schedulerWg sync.WaitGroup
	schedulerWg.Add(1)
	go func() {
		defer schedulerWg.Done()
		scheduler.Run(ctx)
	}()

	// live health model for the API.
	liveHealth := handlers.NewLiveHealthModel(supervisor, scheduler)

	// ensure the async store's parent directory exists before opening it. bbolt
	// does not create parent dirs, so a fresh host (no pre-seeded state dir) would
	// otherwise crash-loop the daemon on first start. 0700: the dir holds job state
	// that need not be world-readable.
	asyncDir := filepath.Dir(cfg.API.VPN.Async.StoragePath)
	if err := os.MkdirAll(asyncDir, 0o700); err != nil {
		return fmt.Errorf("create async store dir %s: %w", asyncDir, err)
	}

	// open the async job store and run recovery before any listener binds.
	// recovery deletes orphaned pending records left by a prior crash so they
	// do not linger into the new process's lifetime.
	store, err := asyncjob.NewStore(cfg.API.VPN.Async.StoragePath)
	if err != nil {
		return fmt.Errorf("open async job store: %w", err)
	}
	// store.Close is called explicitly in the shutdown sequence below (after the
	// pool drains and the GC context is cancelled). The defer here is a safety net
	// for the early-return error paths above the shutdown block.
	storeClosed := false
	defer func() {
		if !storeClosed {
			if cerr := store.Close(); cerr != nil {
				opLog.Error("async store close failed (defer)", "err", cerr)
			}
		}
	}()

	if err := asyncjob.RunRecovery(store, opLog); err != nil {
		return fmt.Errorf("async job recovery: %w", err)
	}

	// build the zone-routing forwarder that bridges the on-demand scheduler into
	// the asyncjob.Forwarder interface.
	fwd := handlers.NewTunnelForwarder(cfg.API.MaxRequestBodyBytes, opLog)
	zoneForwarder := handlers.NewZoneRoutingForwarder(scheduler, fwd)

	jobPool := asyncjob.NewPool(store, zoneForwarder, asyncjob.DefaultMaxConcurrentJobs, opLog)

	gc := asyncjob.NewGC(asyncjob.GCConfig{
		Store:          store,
		Logger:         opLog,
		PendingTimeout: asyncjob.DefaultPendingTimeout,
		CompleteTTL:    asyncjob.DefaultCompleteTTL,
		TombstoneTTL:   asyncjob.DefaultTombstoneTTL,
	})
	// GC runs until ctx is cancelled. gcWg lets the shutdown path join the
	// goroutine before store.Close() so GC cannot be mid-passOnce when the
	// store is closed.
	var gcWg sync.WaitGroup
	gcWg.Add(1)
	go func() {
		defer gcWg.Done()
		_ = gc.Run(ctx)
	}()

	authToken, err := resolveAuthToken(cfg.VPNStream.Auth, configDir)
	if err != nil {
		return fmt.Errorf("resolve auth token: %w", err)
	}

	var verifier auth.Verifier
	if authToken != "" {
		verifier = auth.NewBearerVerifier(authToken)
		source := "inline"
		if cfg.VPNStream.Auth.TokenFile != "" {
			source = "file=" + filepath.Base(cfg.VPNStream.Auth.TokenFile)
		}
		opLog.Info("auth enabled",
			slog.String("source", source),
			slog.Int("token_len", len(authToken)),
		)
	} else {
		opLog.Info("auth disabled", slog.String("listen", cfg.VPNStream.Listen))
	}

	// wire the API tokens and TLS certificate for the HTTPS API listener.
	tokens, err := apiserver.LoadTokens(cfg.API.Auth, configDir)
	if err != nil {
		return fmt.Errorf("load api tokens: %w", err)
	}
	// log token count (never values, never individual lengths beyond the count).
	opLog.Info("api tokens loaded", slog.Int("token_count", apiserver.TokenRoleCount))

	var cert *tls.Certificate
	if tlsOpts.CertDir == "" {
		opLog.Warn("API running WITHOUT TLS — bearer tokens are sent in plaintext; intended for loopback/dev only",
			slog.String("api_listen", cfg.API.Listen),
		)
	} else {
		c, fp, err := apitls.LoadOrGenerate(tlsOpts.CertDir, tlsOpts.Hostname, tlsOpts.IPSANs, opLog)
		if err != nil {
			return fmt.Errorf("load tls cert: %w", err)
		}
		cert = c
		opLog.Info("API TLS enabled",
			slog.String("cert_dir", tlsOpts.CertDir),
			slog.String("sha256_fingerprint", fp),
		)
	}

	// svc is constructed before apiSrv (reordered from the historical layout)
	// so the rotateAdapter below can close over it: apiSrv's Options
	// reference only cfg, cert, tokens, liveHealth, fullSet, scheduler,
	// store, jobPool, access, opLog (never svc), and svc's Options reference
	// only supervisor, verifier, access, opLog, cfg (never apiSrv) — the two
	// constructions are independent, so reordering changes no behaviour.
	svc := service.NewProxyService(service.ProxyServiceOptions{
		Dialer:      supervisor,
		Verifier:    verifier,
		Access:      access,
		OpLog:       opLog,
		DialTimeout: cfg.VPNStream.DialTimeout,
	})

	apiSrv := apiserver.New(apiserver.Options{
		Addr:                cfg.API.Listen,
		ShutdownTimeout:     cfg.API.ShutdownTimeout,
		Cert:                cert,
		Tokens:              tokens,
		LiveHealth:          liveHealth,
		ZoneChecker:         fullSet,
		TunnelCatalog:       fullSet,
		ZoneRouter:          scheduler,
		MaxRequestBodyBytes: cfg.API.MaxRequestBodyBytes,
		UpstreamTimeout:     cfg.API.VPN.Timeout,
		MaxUpstreamTimeout:  cfg.API.VPN.MaxTimeout,
		HealthMaxAge:        lazy.DefaultHandshakeMaxAge,
		JobCounter:          store,
		JobPool:             jobPool,
		Access:              access,
		Rotator:             rotateAdapter{sup: supervisor, svc: svc},
	}, opLog)

	srv := httpserver.New(httpserver.Options{
		Listen:            cfg.VPNStream.Listen,
		ReadHeaderTimeout: cfg.VPNStream.DialTimeout,
		IdleTimeout:       cfg.VPNStream.IdleTimeout,
	}, svc, opLog)

	errCh := make(chan error, 2)
	go func() { errCh <- srv.Start() }()
	go func() { errCh <- apiSrv.Start() }()

	select {
	case err := <-errCh:
		// one side failed to start; best-effort shutdown of the surviving server
		// before returning so we don't leak a goroutine into the cleanup path.
		earlyCtx, earlyCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer earlyCancel()
		_ = srv.Shutdown(earlyCtx)
		_ = apiSrv.Shutdown(earlyCtx)
		return fmt.Errorf("server start: %w", err)
	case <-ctx.Done():
		opLog.Info("shutdown signal received")
	}

	// shutdown order:
	// 1. proxy listener — stops new inbound requests.
	// 2. API listener — stops operator probes.
	// 3. job pool drain — waits for in-flight upstream calls to finish.
	// 4. wait for on-demand scheduler goroutine (ctx already cancelled).
	// 5. stop streaming supervisor.
	// 6. GC is already stopping via the cancelled ctx.
	// 7. store.Close — last, after pool and GC have stopped touching it.
	//
	// Each step's error is logged but does NOT short-circuit the next step.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.VPNStream.ShutdownTimeout)
	defer cancel()

	var shutdownErr error
	if err := srv.Shutdown(shutdownCtx); err != nil {
		opLog.Error("proxy shutdown failed", "err", err.Error())
		shutdownErr = fmt.Errorf("proxy shutdown: %w", err)
	}

	apiShutdownCtx, apiCancel := context.WithTimeout(context.Background(), cfg.API.ShutdownTimeout)
	defer apiCancel()
	if err := apiSrv.Shutdown(apiShutdownCtx); err != nil {
		opLog.Error("api shutdown failed", "err", err.Error())
		if shutdownErr == nil {
			shutdownErr = fmt.Errorf("api shutdown: %w", err)
		}
	}

	// drain in-flight async workers.
	if err := jobPool.Shutdown(shutdownCtx); err != nil {
		opLog.Error("async job pool shutdown failed", "err", err.Error())
		if shutdownErr == nil {
			shutdownErr = fmt.Errorf("job pool shutdown: %w", err)
		}
	}

	// wait for the on-demand scheduler goroutine to exit (ctx is already cancelled).
	schedulerDone := make(chan struct{})
	go func() { schedulerWg.Wait(); close(schedulerDone) }()
	select {
	case <-schedulerDone:
	case <-time.After(5 * time.Second):
		opLog.Error("on-demand scheduler goroutine did not exit within 5s")
	}

	// stop the streaming supervisor.
	supervisor.Stop()

	// close the Telegram notifier: all event producers (supervisor, scheduler)
	// have stopped by this point, so no further Notify calls can race the
	// drain. Close cancels the notifier's internal ctx, which fails any
	// in-flight probe/send fast, bounding the wait.
	if tgNotifier != nil {
		tgNotifier.Close()
	}

	// join the GC goroutine before closing the store.
	gcDone := make(chan struct{})
	go func() { gcWg.Wait(); close(gcDone) }()
	select {
	case <-gcDone:
	case <-time.After(2 * time.Second):
		opLog.Error("async gc goroutine did not exit within 2s; proceeding to store close")
	}

	// close the store last, after pool workers and GC have stopped touching it.
	storeClosed = true
	if err := store.Close(); err != nil {
		opLog.Error("async store close failed", "err", err.Error())
		if shutdownErr == nil {
			shutdownErr = fmt.Errorf("store close: %w", err)
		}
	}

	if shutdownErr != nil {
		return shutdownErr
	}
	opLog.Info("shutdown complete")
	return nil
}
