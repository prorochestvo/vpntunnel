package lazy

import (
	"context"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vpntunnel/internal/notify"
	"vpntunnel/internal/publicerror"
	"vpntunnel/internal/tunnel"
)

// DeviceBuilderFn constructs one live DialerCloser from a config path. The
// production implementation calls BuildDialer (which parses and dials); tests
// inject a fake that ignores configPath and returns a stub device. configDir is
// only needed for resolving relative paths inside BuildDialer and may be ignored
// by test implementations.
//
// This is the supervisor/scheduler-facing seam: it owns path resolution and
// config parsing. For a seam at the post-parse level (given an already-parsed
// config, build a device — used to verify parsed options in tests), see
// BuilderFn and BuildDialer.
type DeviceBuilderFn func(ctx context.Context, configPath, configDir string, opLog *slog.Logger) (tunnel.DialerCloser, error)

// DefaultDeviceBuilder is the production DeviceBuilderFn. It calls BuildDialer
// with the DefaultBuilder as the wireguard factory.
func DefaultDeviceBuilder(ctx context.Context, configPath, configDir string, opLog *slog.Logger) (tunnel.DialerCloser, error) {
	return BuildDialer(ctx, configPath, configDir, opLog, nil)
}

// DefaultHandshakeMaxAge is the maximum age of the last successful WireGuard
// handshake before a device is considered stale. It is ~3x the 25s persistent
// keepalive plus a safety margin so quiet tunnels don't flap.
const DefaultHandshakeMaxAge = 180 * time.Second

// SupervisorOptions holds all dependencies for NewStreamingSupervisor.
type SupervisorOptions struct {
	// Eligible is the country-filtered pool the supervisor picks from. Required.
	Eligible *EligibleSet
	// DeviceBuilder builds a live DialerCloser from a config path. When nil,
	// DefaultDeviceBuilder is used. Inject a fake in tests.
	DeviceBuilder DeviceBuilderFn
	// HandshakeMaxAge is the maximum handshake age before the supervisor tears the
	// device down and reconnects. Typically wired from DefaultHandshakeMaxAge. Required > 0.
	HandshakeMaxAge time.Duration
	// ReconnectMin is the initial backoff between reconnect attempts. Required > 0.
	ReconnectMin time.Duration
	// ReconnectMax is the ceiling for the exponential backoff. Required >= ReconnectMin.
	ReconnectMax time.Duration
	// PollInterval is how often the supervisor checks the handshake age. When zero,
	// defaults to HandshakeMaxAge / 3 (at least 10s). Inject a short interval in tests.
	PollInterval time.Duration
	// Clock abstracts time operations for tests. When nil, NewRealClock() is used.
	Clock Clock
	// ConfigDir is used to resolve relative config paths via DeviceBuilder. Required.
	ConfigDir string
	// OpLog is the operational slog logger. When nil, slog.Default() is used.
	OpLog *slog.Logger
	// Notifier reports tunnel-change events (startup, reconnect). Optional;
	// defaults to notify.Nop{} when nil so call sites never need to nil-check.
	Notifier notify.Notifier
}

// NewStreamingSupervisor constructs a StreamingSupervisor from opts. It panics
// if any required field is zero/nil. Call Start to begin the health-poll loop.
func NewStreamingSupervisor(opts SupervisorOptions) *StreamingSupervisor {
	if opts.Eligible == nil {
		panic("lazy: StreamingSupervisor requires a non-nil EligibleSet")
	}
	if opts.HandshakeMaxAge <= 0 {
		panic("lazy: StreamingSupervisor requires HandshakeMaxAge > 0")
	}
	if opts.ReconnectMin <= 0 {
		panic("lazy: StreamingSupervisor requires ReconnectMin > 0")
	}
	if opts.ReconnectMax < opts.ReconnectMin {
		panic("lazy: StreamingSupervisor requires ReconnectMax >= ReconnectMin")
	}
	if opts.ConfigDir == "" {
		panic("lazy: StreamingSupervisor requires non-empty ConfigDir")
	}

	deviceBuilder := opts.DeviceBuilder
	if deviceBuilder == nil {
		deviceBuilder = DefaultDeviceBuilder
	}
	clk := opts.Clock
	if clk == nil {
		clk = NewRealClock()
	}
	notifier := opts.Notifier
	if notifier == nil {
		notifier = notify.Nop{}
	}

	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = opts.HandshakeMaxAge / 3
		if pollInterval < 10*time.Second {
			pollInterval = 10 * time.Second
		}
	}

	return &StreamingSupervisor{
		eligible:        opts.Eligible,
		deviceBuilder:   deviceBuilder,
		handshakeMaxAge: opts.HandshakeMaxAge,
		reconnectMin:    opts.ReconnectMin,
		reconnectMax:    opts.ReconnectMax,
		pollInterval:    pollInterval,
		clock:           clk,
		configDir:       opts.ConfigDir,
		opLog:           opts.OpLog,
		notifier:        notifier,
		// stopCh and loopDone are allocated here (not in Start) so Stop is safe to
		// call before or without Start — e.g. a deferred cleanup that runs on an early
		// return. loopDone is closed by the goroutine launched in Start; if Start was
		// never called Stop simply skips the join (started == false).
		stopCh:   make(chan struct{}),
		loopDone: make(chan struct{}),
	}
}

// StreamingSupervisor owns exactly one live streaming WireGuard device for the
// process lifetime. It exposes a tunnel.Dialer whose DialContext delegates to
// the current live device under an RWMutex swap — in-flight connections to the
// old device die naturally when the device is closed; new dials get the new
// device (RESOLVED #5: no drain-before-close).
//
// Start picks a random eligible config, builds the device, and launches a
// background goroutine that polls the handshake age and reconnects with
// exponential backoff when the tunnel goes stale or errors. The backoff resets
// to ReconnectMin after a successful healthy reconnect.
//
// If the very first build fails, Start returns without error and the loop keeps
// retrying on backoff; DialContext returns an unavailable error until the first
// device is live.
//
// Stop (or ctx cancellation passed to Start) closes the live device exactly
// once and exits the poll loop without a goroutine leak.
//
// LiveHealth returns a snapshot of the current device's health for use by the
// /v1/admin/health endpoint (Task 9). It returns ok=false when no device is
// currently live.
type StreamingSupervisor struct {
	eligible        *EligibleSet
	deviceBuilder   DeviceBuilderFn
	handshakeMaxAge time.Duration
	reconnectMin    time.Duration
	reconnectMax    time.Duration
	pollInterval    time.Duration
	clock           Clock
	configDir       string
	opLog           *slog.Logger
	notifier        notify.Notifier

	// mu guards device, deviceID, and reporter.
	mu       sync.RWMutex
	device   tunnel.DialerCloser
	deviceID string
	reporter tunnel.HealthReporter

	// stopOnce ensures Close/Stop tears down the device exactly once.
	stopOnce sync.Once
	stopCh   chan struct{}
	// loopDone is closed by the goroutine launched in Start after it fully exits
	// (device closed). started records whether Start launched that goroutine; it
	// is atomic so Stop may read it from a different goroutine without a data
	// race. Stop waits on loopDone only when started is true.
	loopDone chan struct{}
	started  atomic.Bool
}

// DialContext implements tunnel.Dialer. It delegates to the current live device
// under a read lock. If no device is currently live (mid-backoff after a failure)
// it returns a publicerror so the proxy listener can surface a clean error.
func (s *StreamingSupervisor) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	s.mu.RLock()
	d := s.device
	s.mu.RUnlock()

	if d == nil {
		return nil, publicerror.New("Streaming tunnel temporarily unavailable; reconnecting.")
	}
	return d.DialContext(ctx, network, address)
}

// Stop stops the supervisor's poll loop and closes the live device exactly once.
// It is safe to call more than once. After Stop returns, the background
// goroutine has fully exited and the live device (if any) has been closed.
//
// If Start was never called, Stop closes the stop channel and returns
// immediately — there is no goroutine to join. Subsequent calls to Stop are
// no-ops; because loopDone is a closed channel, receiving from it returns
// immediately on any call after the first.
func (s *StreamingSupervisor) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	// only join the goroutine if Start launched it.
	if s.started.Load() {
		<-s.loopDone
	}
}

// LiveHealth returns a health snapshot of the currently live device.
// ok is false when no device is live (nil device or no handshake reporter).
// The returned TunnelHealth.Err is non-nil when the HealthReporter itself
// failed to query its transport. Callers must not log TunnelHealth.Err verbatim
// in external responses (CLAUDE.md: never expose Err text in the health body).
func (s *StreamingSupervisor) LiveHealth() (tunnel.TunnelHealth, bool) {
	s.mu.RLock()
	id := s.deviceID
	rep := s.reporter
	s.mu.RUnlock()

	if rep == nil {
		return tunnel.TunnelHealth{}, false
	}
	hs, err := rep.LastHandshake()
	return tunnel.TunnelHealth{ID: id, LastHandshake: hs, Err: err}, true
}

// Start picks a random eligible config, attempts to build the device, and
// launches the health-poll / reconnect loop. If the first build fails, Start
// logs a WARN and the loop retries on backoff; Start itself returns nil so the
// caller (main.go, Task 8) can proceed with binding listeners. ctx cancellation
// acts as an alternative stop signal identical to Stop().
func (s *StreamingSupervisor) Start(ctx context.Context) error {
	// attempt first connect; log on failure but do not fail Start.
	configPath := s.eligible.RandomPath()
	d, id, rep, err := s.build(ctx, configPath)
	if err != nil {
		s.logger().Warn("streaming supervisor: first connect failed; retrying on backoff",
			slog.String("tunnel_id", tunnelIDFromPath(configPath)),
			slog.String("err", err.Error()),
		)
	} else {
		s.swapDevice(d, id, rep)
		s.notifyChange("started", id, d)
	}

	s.started.Store(true)
	go func() {
		defer close(s.loopDone)
		s.loop(ctx)
	}()
	return nil
}

// loop is the health-poll / reconnect background goroutine. It exits when either
// ctx is cancelled or s.stopCh is closed (Stop called). The loop follows this
// state machine:
//
//	if device is nil → in backoff: wait backoffDur, then attempt reconnect.
//	if device is non-nil → poll every pollInterval, tear down + reconnect on stale/error.
//	on successful reconnect: reset backoff to reconnectMin.
func (s *StreamingSupervisor) loop(ctx context.Context) {
	backoff := s.reconnectMin

	for {
		s.mu.RLock()
		d := s.device
		rep := s.reporter
		s.mu.RUnlock()

		if d == nil {
			// in backoff — wait then retry.
			select {
			case <-ctx.Done():
				s.shutdown()
				return
			case <-s.stopCh:
				s.shutdown()
				return
			case <-s.clock.After(backoff):
			}

			configPath := s.eligible.RandomPath()
			nd, id, nrep, err := s.build(ctx, configPath)
			if err != nil {
				s.logger().Warn("streaming supervisor: reconnect failed",
					slog.String("tunnel_id", tunnelIDFromPath(configPath)),
					slog.String("backoff", backoff.String()),
					slog.String("err", err.Error()),
				)
				backoff = growBackoff(backoff, s.reconnectMax)
				continue
			}
			s.swapDevice(nd, id, nrep)
			s.notifyChange("switched tunnel", id, nd)
			// a fresh build doesn't mean the tunnel is healthy yet (no handshake
			// has occurred). Only reset the backoff after the first healthy poll.
			// Leave backoff unchanged until we confirm a healthy handshake.
			continue
		}

		// device is live — poll on interval.
		select {
		case <-ctx.Done():
			s.shutdown()
			return
		case <-s.stopCh:
			s.shutdown()
			return
		case <-s.clock.After(s.pollInterval):
		}

		// check the handshake age.
		healthy, reason := s.isHealthy(rep)
		if healthy {
			// successful healthy poll — reset backoff.
			backoff = s.reconnectMin
			continue
		}

		// stale or error — tear down and re-enter the reconnect loop. The backoff
		// is NOT grown here: the first reconnect attempt after a healthy session
		// failing uses the current backoff (reset to reconnectMin on last healthy
		// poll). Backoff grows only when a reconnect attempt itself fails.
		s.mu.RLock()
		id := s.deviceID
		s.mu.RUnlock()

		s.logger().Warn("streaming supervisor: tunnel unhealthy; tearing down",
			slog.String("tunnel_id", id),
			slog.String("reason", reason),
			slog.String("backoff", backoff.String()),
		)
		s.teardown()
	}
}

// build calls s.deviceBuilder to create a live DialerCloser and casts the
// result to HealthReporter. Returns a plain error on failure.
func (s *StreamingSupervisor) build(ctx context.Context, configPath string) (tunnel.DialerCloser, string, tunnel.HealthReporter, error) {
	id := tunnelIDFromPath(configPath)

	s.logger().Info("streaming supervisor: building tunnel",
		slog.String("tunnel_id", id),
		slog.String("selected_via", "random"),
	)

	d, err := s.deviceBuilder(ctx, configPath, s.configDir, s.logger())
	if err != nil {
		// the build error is logged verbatim by callers. The wireguard-go /
		// wgconf build chain reports key-parse failures as size/format errors
		// (e.g. "incorrect key size") and never echoes the key bytes, so it is
		// safe to surface in logs per the never-log-key-material constraint.
		return nil, "", nil, err
	}

	rep, _ := d.(tunnel.HealthReporter)
	return d, id, rep, nil
}

// swapDevice atomically replaces the current device. The old device (if any)
// must already have been closed before calling swapDevice; this function does
// NOT close it (caller's responsibility to enforce the "close old before new
// build" invariant).
func (s *StreamingSupervisor) swapDevice(d tunnel.DialerCloser, id string, rep tunnel.HealthReporter) {
	s.mu.Lock()
	s.device = d
	s.deviceID = id
	s.reporter = rep
	s.mu.Unlock()
}

// teardown closes the current device and clears the device fields.
func (s *StreamingSupervisor) teardown() {
	s.mu.Lock()
	d := s.device
	s.device = nil
	s.deviceID = ""
	s.reporter = nil
	s.mu.Unlock()

	if d != nil {
		if err := d.Close(); err != nil {
			s.logger().Warn("streaming supervisor: close failed during teardown",
				slog.String("err", err.Error()),
			)
		}
	}
}

// shutdown closes any live device and is called on Stop/ctx-cancel. It uses
// teardown which is idempotent (device=nil is a no-op).
func (s *StreamingSupervisor) shutdown() {
	s.teardown()
	s.logger().Info("streaming supervisor: stopped")
}

// isHealthy queries the HealthReporter and returns true when the handshake
// age is within HandshakeMaxAge. reason is a short enum string for logging.
func (s *StreamingSupervisor) isHealthy(rep tunnel.HealthReporter) (bool, string) {
	if rep == nil {
		// no HealthReporter — treat as healthy so we never tear down a device
		// that doesn't expose health (e.g. a test fake without health support).
		return true, ""
	}
	hs, err := rep.LastHandshake()
	if err != nil {
		return false, "health_query_error"
	}
	if hs.IsZero() {
		return false, "no_handshake"
	}
	age := s.clock.Now().Sub(hs)
	if age > s.handshakeMaxAge {
		return false, "stale_handshake"
	}
	return true, ""
}

func (s *StreamingSupervisor) logger() *slog.Logger {
	if s.opLog != nil {
		return s.opLog
	}
	return slog.Default()
}

// notifyChange reports a streaming tunnel change (first connect or
// reconnect) to the configured Notifier. It is called after swapDevice has
// already made d the live device, with context.Background() rather than the
// supervisor's run ctx: Notify never blocks, so the send is fire-and-forget
// and outlives any single call's context.
func (s *StreamingSupervisor) notifyChange(title, id string, d tunnel.Dialer) {
	s.notifier.Notify(context.Background(), notify.Event{
		Source:   notify.SourceStreaming,
		Title:    title,
		Country:  strings.ToLower(tunnel.CountryFromID(id)),
		Filename: id + ".conf",
		Dialer:   d,
	})
}

// growBackoff doubles d up to max.
func growBackoff(d, max time.Duration) time.Duration {
	if d <= 0 {
		return max
	}
	doubled := d * 2
	if doubled > max || doubled <= 0 { // overflow guard
		return max
	}
	return doubled
}

// tunnelIDFromPath derives the tunnel ID (basename without ".conf") from an
// absolute or relative config path. This is the same derivation used by
// BuildDialer and EligibleSet.
func tunnelIDFromPath(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".conf")
}
