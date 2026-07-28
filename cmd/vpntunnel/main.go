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

	"github.com/prorochestvo/dsninjector"

	"vpntunnel/internal/application"
	"vpntunnel/internal/application/asyncjob"
	"vpntunnel/internal/application/tunnelpool"
	"vpntunnel/internal/gateway/httpV1/handlers"
	"vpntunnel/internal/gateway/httpserver"
	"vpntunnel/internal/gateway/middleware"
	"vpntunnel/internal/gateway/router"
	"vpntunnel/internal/gateway/router/apitls"
	"vpntunnel/internal/infrastructure/config"
	"vpntunnel/internal/infrastructure/notify"
	"vpntunnel/internal/infrastructure/observability"
	"vpntunnel/internal/policy"
	"vpntunnel/internal/tools/bearerauth"
	"vpntunnel/internal/tools/hmackey"
)

// telegramAppTag is the app-identity prefix injected into every Telegram
// notification (see notify.NewTelegram). It is non-secret and safe to log.
const telegramAppTag = "#VPNTUNNEL"

// bootstrap parses the process CLI flags and turns them into the finished
// startup objects run needs: the validated configuration, the API TLS
// certificate (nil in plain-HTTP mode), and the operational logger. The order
// is forced — the logger is built from cfg.Operational, and the TLS
// load/generate step logs through it — so config load, logger construction and
// cert load all happen here rather than inside run.
//
// It calls flag.Parse, so it is only ever called from main, never from a test:
// flag.Parse in a test process hits the -test.* flags and aborts.
func bootstrap() (config.Config, *tls.Certificate, *slog.Logger, error) {
	var (
		rawConfigPath string
		tlsCertDir    string
		tlsHostname   string
		tlsIPSANsRaw  string
	)
	flag.StringVar(&rawConfigPath, "config", "./configs/proxy.json", "path to JSON config file")
	flag.StringVar(&tlsCertDir, "tls-cert-dir", "", "directory holding the API TLS cert/key; empty runs the API as plain HTTP (loopback/dev only). Relative resolves against CWD.")
	flag.StringVar(&tlsHostname, "tls-hostname", "localhost", "TLS server name embedded in the API certificate")
	flag.StringVar(&tlsIPSANsRaw, "tls-ip-sans", "", "comma-separated list of IP Subject Alternative Names")
	flag.Parse()

	// resolve the config path so cfg.Dir, tunnelsDir, and every operator-facing
	// error message are cwd-independent.
	configPath, err := filepath.Abs(rawConfigPath)
	if err != nil {
		return config.Config{}, nil, nil, fmt.Errorf("resolve config path %q: %w", rawConfigPath, err)
	}

	certDir, err := resolveCertDir(tlsCertDir)
	if err != nil {
		return config.Config{}, nil, nil, err
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return config.Config{}, nil, nil, fmt.Errorf("load config: %w", err)
	}

	opLog := newOperationalLogger(cfg.Operational)

	cert, err := loadAPICert(certDir, tlsHostname, tlsIPSANsRaw, cfg.API.Listen, opLog)
	if err != nil {
		return config.Config{}, nil, nil, err
	}

	return cfg, cert, opLog, nil
}

func main() {
	cfg, cert, opLog, err := bootstrap()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// the signal handler is installed only after bootstrap's blocking startup
	// I/O (config read, cert load/generate) has finished. Until a handler
	// exists SIGINT/SIGTERM keeps its default action and kills the process
	// immediately; installing it earlier would capture the signal into a
	// context nothing is watching yet, leaving startup uninterruptible.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, cert, opLog, tunnelpool.DefaultDeviceBuilder, tunnelpool.DefaultDeviceBuilder); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is the full startup / run / shutdown path. ctx is the caller-owned
// shutdown trigger: cancelling it starts the graceful shutdown sequence. cfg,
// cert and opLog are the finished objects bootstrap produced; a nil cert means
// the API listener serves plain HTTP. streamingBuilder and onDemandBuilder
// construct the tunnel devices for the always-on streaming supervisor and the
// on-demand scheduler respectively; production passes
// tunnelpool.DefaultDeviceBuilder for both, tests pass a fake.
func run(
	ctx context.Context,
	cfg config.Config,
	cert *tls.Certificate,
	opLog *slog.Logger,
	streamingBuilder tunnelpool.DeviceBuilderFn,
	onDemandBuilder tunnelpool.DeviceBuilderFn,
) error {
	// build the Telegram tunnel-change notifier from the environment. Absent or
	// malformed input both leave the proxy running: a bad DSN is auxiliary
	// telemetry misconfiguration, never a reason to crash-loop the actual
	// product. tgNotifier is held separately (rather than type-asserting
	// notifier later) purely for its Close lifecycle at shutdown.
	var notifier notify.Notifier = notify.Nop{}
	var tgNotifier *notify.TelegramNotifier
	if dsn := os.Getenv(policy.EnvTelegramBotDSN); dsn != "" {
		// dsninjector.Parse embeds its raw input — which IS the bot token — in
		// its error text, so a parse failure must NEVER log or format that error;
		// it warns with a generic message only. The DataSource is built here (not
		// inside notify) so the token-bearing parse error never crosses into the
		// notify package.
		ds, perr := dsninjector.Parse(dsn)
		if perr != nil {
			opLog.Warn("telegram notifier disabled: invalid VPNTUNNEL_TELEGRAMBOT_DSN")
		} else if tn, err := notify.NewTelegram(ds, telegramAppTag, opLog); err != nil {
			// NewTelegram returns a safe error (never the DSN or the bot token);
			// warn and keep running with notifications disabled.
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

	// derive a cancellable child of the caller's shutdown context so the deferred
	// stop bounds a hung startup (e.g. a stalled DNS lookup) and releases every
	// goroutine started below on all return paths, not only the shutdown one.
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	configDir := cfg.Dir

	// discover all *.conf files in <configDir>/tunnels/. Discovery never parses
	// file contents — it returns a sorted list of absolute paths.
	tunnelsDir := filepath.Join(configDir, "tunnels")
	discovered, err := tunnelpool.DiscoverConfigs(tunnelsDir)
	if err != nil {
		return fmt.Errorf("discover tunnel configs: %w", err)
	}
	opLog.Info("tunnel configs discovered",
		slog.String("dir", tunnelsDir),
		slog.Int("count", len(discovered)),
	)

	hmacKey, err := hmackey.LoadOrGenerate(cfg.TunnelIDHMACKeyFile, configDir, opLog)
	if err != nil {
		return fmt.Errorf("load tunnel-id hmac key: %w", err)
	}

	// build the full set first — covers every discovered config, used by the
	// on-demand scheduler and the API ZoneChecker. An empty discovered set is
	// caught here before the country-filter check below.
	fullSet, err := tunnelpool.NewFullSet(discovered, configDir, hmacKey)
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
	streamingSet, err := tunnelpool.NewEligibleSet(discovered, configDir, cfg.VPNStream.AllowedCountries)
	if err != nil {
		return fmt.Errorf("build streaming tunnel set: %w", err)
	}

	// single-key guard runs over the full discovered set (pre-country-filter) so
	// the single-account invariant is enforced across all configs the operator
	// deposited, not just the ones currently enabled by allowed_countries.
	if err := tunnelpool.VerifySingleKey(discovered, configDir, opLog); err != nil {
		return fmt.Errorf("single-key guard: %w", err)
	}

	// a signal delivered while the startup I/O above was running cancelled ctx
	// before anything was watching it. Check once here, before the first
	// goroutine and listener, so startup aborts instead of building the whole
	// daemon only to tear it straight back down.
	if ctx.Err() != nil {
		opLog.Info("shutdown signal received during startup; aborting before any listener starts")
		return nil
	}

	// build the streaming supervisor (always-on role).
	supervisor := tunnelpool.NewStreamingSupervisor(tunnelpool.SupervisorOptions{
		Eligible:        streamingSet,
		DeviceBuilder:   streamingBuilder,
		HandshakeMaxAge: tunnelpool.DefaultHandshakeMaxAge,
		ReconnectMin:    cfg.VPNStream.ReconnectMin,
		ReconnectMax:    cfg.VPNStream.ReconnectMax,
		// RotateSettle reuses the operator-tuned on-demand settle delay so the
		// streaming role's rotate honours the same Mullvad-session-free window
		// without the tunnelpool package importing on-demand config. SettleDelay
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
	scheduler := tunnelpool.NewOnDemandScheduler(tunnelpool.SchedulerOptions{
		Eligible:      fullSet,
		DeviceBuilder: onDemandBuilder,
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

	var verifier application.Verifier
	if authToken != "" {
		verifier = bearerauth.NewBearerVerifier(authToken)
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
	tokens, err := middleware.LoadTokens(cfg.API.Auth, configDir)
	if err != nil {
		return fmt.Errorf("load api tokens: %w", err)
	}
	// log token count (never values, never individual lengths beyond the count).
	opLog.Info("api tokens loaded", slog.Int("token_count", middleware.TokenRoleCount))

	// svc is constructed before apiSrv (reordered from the historical layout)
	// so the rotateAdapter below can close over it: apiSrv's Options
	// reference only cfg, cert, tokens, liveHealth, fullSet, scheduler,
	// store, jobPool, access, opLog (never svc), and svc's Options reference
	// only supervisor, verifier, access, opLog, cfg (never apiSrv) — the two
	// constructions are independent, so reordering changes no behaviour.
	proxy := application.NewProxyService(application.ProxyServiceOptions{
		Dialer:      supervisor,
		Verifier:    verifier,
		Access:      access,
		OpLog:       opLog,
		DialTimeout: cfg.VPNStream.DialTimeout,
	})

	apiSrv := router.New(router.Options{
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
		HealthMaxAge:        tunnelpool.DefaultHandshakeMaxAge,
		JobCounter:          store,
		JobPool:             jobPool,
		Access:              access,
		Rotator:             rotateAdapter{sup: supervisor, svc: proxy},
	}, opLog)

	vpnSrv := httpserver.New(httpserver.Options{
		Listen:            cfg.VPNStream.Listen,
		ReadHeaderTimeout: cfg.VPNStream.DialTimeout,
		IdleTimeout:       cfg.VPNStream.IdleTimeout,
	}, proxy, opLog)

	errCh := make(chan error, 2)
	go func() { errCh <- vpnSrv.Start() }()
	go func() { errCh <- apiSrv.Start() }()

	select {
	case err := <-errCh:
		// one side failed to start; best-effort shutdown of the surviving server
		// before returning so we don't leak a goroutine into the cleanup path.
		earlyCtx, earlyCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer earlyCancel()
		_ = vpnSrv.Shutdown(earlyCtx)
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
	if err := vpnSrv.Shutdown(shutdownCtx); err != nil {
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

// loadAPICert returns the API TLS certificate, or (nil, nil) when certDir is
// empty — the plain-HTTP mode trigger. hostname is the server name embedded in
// the certificate and is required in both modes; ipSANsRaw is the raw
// comma-separated -tls-ip-sans flag value, validated in both modes so a typo is
// reported even when no certificate is minted. apiListen is used only as an
// attribute on the no-TLS warning.
//
// The only certificate facts logged are the cert dir and the SHA256
// fingerprint; key material never reaches a log line.
func loadAPICert(certDir, hostname, ipSANsRaw, apiListen string, opLog *slog.Logger) (*tls.Certificate, error) {
	if hostname == "" {
		return nil, errors.New("-tls-hostname: must not be empty")
	}

	ipSANs, err := parseIPSANs(ipSANsRaw)
	if err != nil {
		return nil, err
	}

	if certDir == "" {
		opLog.Warn("API running WITHOUT TLS — bearer tokens are sent in plaintext; intended for loopback/dev only",
			slog.String("api_listen", apiListen),
		)
		return nil, nil
	}

	cert, fp, err := apitls.LoadOrGenerate(certDir, hostname, ipSANs, opLog)
	if err != nil {
		return nil, fmt.Errorf("load tls cert: %w", err)
	}
	opLog.Info("API TLS enabled",
		slog.String("cert_dir", certDir),
		slog.String("sha256_fingerprint", fp),
	)
	return cert, nil
}

// newOperationalLogger builds the operational logger wrapped in the scrub
// handler that redacts host:port patterns from every message and attribute. It
// is a named function rather than three inline lines so production and tests
// construct identically-configured loggers.
//
// The underlying handler binds os.Stdout at construction time: a caller that
// redirects os.Stdout must do so before calling this.
func newOperationalLogger(cfg config.Operational) *slog.Logger {
	base := observability.NewOperationalLogger(cfg)
	opLog := slog.New(observability.NewScrubHandler(base.Handler()))
	opLog.Info("operational log scrubbing active", slog.String("strategy", "host-port-redact"))
	return opLog
}

// parseIPSANs parses the comma-separated -tls-ip-sans flag value into IP
// Subject Alternative Names. Blank entries (including the one a trailing comma
// produces) are skipped; an unparseable entry is an error naming the offending
// value.
func parseIPSANs(raw string) ([]net.IP, error) {
	var ipSANs []net.IP
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		ip := net.ParseIP(entry)
		if ip == nil {
			return nil, fmt.Errorf("-tls-ip-sans: %q is not a valid IP address", entry)
		}
		ipSANs = append(ipSANs, ip)
	}
	return ipSANs, nil
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

// resolveCertDir returns certDir as an absolute path. An empty certDir is the
// plain-HTTP mode trigger and is preserved as-is: filepath.Abs("") returns the
// cwd, which would silently re-enable HTTPS against an unintended directory.
func resolveCertDir(certDir string) (string, error) {
	if certDir == "" || filepath.IsAbs(certDir) {
		return certDir, nil
	}
	abs, err := filepath.Abs(certDir)
	if err != nil {
		return "", fmt.Errorf("-tls-cert-dir: resolve %q: %w", certDir, err)
	}
	return abs, nil
}
