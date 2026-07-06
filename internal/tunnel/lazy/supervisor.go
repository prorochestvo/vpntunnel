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

// RotateOutcome enumerates the possible results of a RotateIfIdle call.
type RotateOutcome int

const (
	// RotateRotated means the streaming device was torn down and rebuilt
	// against a fresh random exit; RotateResult.Country is set.
	RotateRotated RotateOutcome = iota
	// RotateSkippedActive means the rotation was gated because active
	// streaming sessions were in flight and force was false — nothing changed.
	RotateSkippedActive
	// RotateUnavailable covers four cases: RotateIfIdle was called before
	// Start (no loop goroutine to service it yet), no device was live to
	// rotate (mid-backoff), the post-teardown build failed, or loopCtx was
	// cancelled during the settle wait. In the latter three the device is left
	// nil and the loop's existing backoff/reconnect logic self-heals on its
	// own schedule.
	RotateUnavailable
)

// RotateResult is the outcome of a RotateIfIdle call.
type RotateResult struct {
	// Outcome selects which of the other fields (if any) is meaningful.
	Outcome RotateOutcome
	// Country is the lowercase two-letter country code of the newly built
	// exit. Set only when Outcome == RotateRotated.
	Country string
}

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
	// RotateSettle is the mandatory pause between tearing the old streaming
	// device down and building the new one during RotateIfIdle — the
	// streaming-role twin of the on-demand scheduler's settleDelay, sized the
	// same (~15s) so the provider frees the old session before the new
	// handshake. Required > 0; production wires it from
	// cfg.API.VPN.Demand.SettleDelay.
	RotateSettle time.Duration
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
	if opts.RotateSettle <= 0 {
		panic("lazy: StreamingSupervisor requires RotateSettle > 0")
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
		rotateSettle:    opts.RotateSettle,
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
		// rotateCh is unbuffered — a nil channel would block RotateIfIdle
		// forever; unbuffered is correct because the loop goroutine is the
		// only reader and RotateIfIdle already guards the send against a
		// stopped loop via loopDone.
		rotateCh: make(chan rotateReq),
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
//
// RotateIfIdle asks the loop goroutine to perform a graceful break-before-make
// + settle rotation to a fresh random exit. It is routed through rotateCh so
// s.device is only ever mutated on the loop goroutine, never concurrently
// with the health-poll/reconnect logic.
type StreamingSupervisor struct {
	eligible        *EligibleSet
	deviceBuilder   DeviceBuilderFn
	handshakeMaxAge time.Duration
	reconnectMin    time.Duration
	reconnectMax    time.Duration
	pollInterval    time.Duration
	rotateSettle    time.Duration
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

	// rotateCh carries RotateIfIdle requests to the loop goroutine, the sole
	// writer of device/deviceID/reporter. Unbuffered — see NewStreamingSupervisor.
	rotateCh chan rotateReq
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

// RotateIfIdle asks the loop goroutine to perform a graceful streaming
// rotation: break-before-make + settle (tear the current device down, wait
// RotateSettle so the provider frees the session, then build a fresh
// random-pick device) — never make-before-break, which would transiently
// hold two streaming sessions and risk exceeding the host's session budget.
//
// When force is false, busy is consulted twice — a cheap pre-check, then an
// authoritative re-check under the write lock — and a true result skips the
// rotation entirely (RotateSkippedActive), touching nothing. When force is
// true, both busy checks are skipped, but break-before-make + settle still
// runs: force means "drop my active connections and rotate," never "run a
// second concurrent streaming session."
//
// The rotation always executes on the loop goroutine (the sole writer of
// s.device), so it never races the health-poll/reconnect logic.
//
// If Start has not yet been called, RotateIfIdle returns RotateUnavailable
// immediately rather than blocking on a loop goroutine that will never read
// rotateCh.
//
// The returned error is non-nil ONLY when ctx is cancelled before the loop
// replies (e.g. the HTTP caller disconnected) — it is always a plain error
// (ctx.Err()), never a *publicerror.Error. A caller ctx-cancel does not abort
// an in-flight rotation: the loop finishes it on its own loopCtx regardless;
// the reply simply lands unread in the cap-1 buffered reply channel.
func (s *StreamingSupervisor) RotateIfIdle(ctx context.Context, force bool, busy func() bool) (RotateResult, error) {
	if !s.started.Load() {
		// the loop goroutine that reads rotateCh has not been launched (Start
		// not called), so a send would block until ctx expires; report
		// unavailable immediately instead of hanging on the caller's deadline.
		return RotateResult{Outcome: RotateUnavailable}, nil
	}

	reply := make(chan rotateReply, 1)
	req := rotateReq{force: force, busy: busy, reply: reply}

	select {
	case s.rotateCh <- req:
	case <-ctx.Done():
		return RotateResult{}, ctx.Err()
	case <-s.loopDone:
		// the loop has already exited (post-Stop) — do not hang waiting on a
		// goroutine that will never read rotateCh.
		return RotateResult{Outcome: RotateUnavailable}, nil
	}

	select {
	case r := <-reply:
		return r.result, nil
	case <-ctx.Done():
		return RotateResult{}, ctx.Err()
	}
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
			case req := <-s.rotateCh:
				// serviced here too so a rotate during backoff is answered
				// promptly (RotateUnavailable) instead of waiting out the
				// backoff timer; this iteration's backoff timer is abandoned.
				s.handleRotate(ctx, req)
				continue
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
		case req := <-s.rotateCh:
			// this iteration's poll timer is abandoned; the next loop
			// iteration re-reads s.device and re-arms a fresh poll wait.
			s.handleRotate(ctx, req)
			continue
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

// handleRotate performs one graceful rotation attempt. It runs exclusively on
// the loop goroutine, so it never races loop's own device mutations.
//
// It implements break-before-make + settle: tear the live device down (under
// s.mu.Lock, so a racing DialContext sees nil and gets the standard
// "temporarily unavailable" publicerror instead of ever touching a closed
// device), wait rotateSettle with NO lock held (so DialContext is never
// blocked for the multi-second settle+build), then build a fresh random-pick
// device and swap it in.
//
// Race-freedom argument: every proxy handler path increments activeSessions
// strictly before its DialContext call (the ProxyService.ActiveSessions
// invariant), so activeSessions==0 observed under s.mu's write lock implies no
// open connection to the device about to be torn down — tearing it down drops
// nothing live. Any DialContext racing this call blocks on the RWMutex's read
// lock until this method releases it; by then s.device is nil, so the racing
// dial gets the unavailable error and the caller retries — it can never reach
// the torn-down old device. The final swap-in needs no re-check because it
// only adds a device, never removes one.
func (s *StreamingSupervisor) handleRotate(loopCtx context.Context, req rotateReq) {
	if !req.force && req.busy() { // cheap pre-check, no lock held
		req.reply <- rotateReply{result: RotateResult{Outcome: RotateSkippedActive}}
		return
	}

	s.mu.Lock()
	if !req.force && req.busy() { // authoritative re-check UNDER the write lock
		s.mu.Unlock()
		req.reply <- rotateReply{result: RotateResult{Outcome: RotateSkippedActive}}
		return
	}
	if s.device == nil { // mid-backoff: nothing live to rotate
		s.mu.Unlock()
		req.reply <- rotateReply{result: RotateResult{Outcome: RotateUnavailable}}
		return
	}
	old := s.device
	oldID := s.deviceID
	s.device, s.deviceID, s.reporter = nil, "", nil // TEARDOWN under lock: new dials now see nil → unavailable
	s.mu.Unlock()

	if cerr := old.Close(); cerr != nil {
		s.logger().Warn("streaming supervisor: rotate old-device close failed", slog.String("err", cerr.Error()))
	}

	// settle so the provider frees the old session before the new handshake —
	// upholds the host's session ceiling. No lock held during the wait.
	select {
	case <-loopCtx.Done():
		// shutdown mid-rotate; device stays nil, the loop exits on its next check.
		req.reply <- rotateReply{result: RotateResult{Outcome: RotateUnavailable}}
		return
	case <-s.clock.After(s.rotateSettle):
	}

	configPath := s.eligible.RandomPath()
	newDev, id, rep, err := s.build(loopCtx, configPath)
	if err != nil {
		// the old device is already torn down, so a failed rebuild leaves
		// streaming genuinely down until the loop's d==nil backoff branch
		// reconnects — log it (matching the "first connect"/"reconnect" build
		// failures, tunnel_id included) so a resulting 503 is diagnosable.
		s.logger().Warn("streaming supervisor: rotate rebuild failed; streaming down until reconnect",
			slog.String("tunnel_id", tunnelIDFromPath(configPath)),
			slog.String("err", err.Error()),
		)
		req.reply <- rotateReply{result: RotateResult{Outcome: RotateUnavailable}}
		return
	}

	s.mu.Lock()
	s.device, s.deviceID, s.reporter = newDev, id, rep // swap in — no re-check (adding, not tearing down)
	s.mu.Unlock()

	newCountry := strings.ToLower(tunnel.CountryFromID(id))
	s.logger().Info("streaming supervisor: rotated tunnel",
		slog.String("old_country", strings.ToLower(tunnel.CountryFromID(oldID))),
		slog.String("new_country", newCountry),
		slog.String("new_tunnel_id", id),
	)
	s.notifyChange("rotated", id, newDev) // reuse the existing SourceStreaming notify path
	req.reply <- rotateReply{result: RotateResult{Outcome: RotateRotated, Country: newCountry}}
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

// rotateReq is a single RotateIfIdle call enqueued to the loop goroutine via
// rotateCh. reply is per-call and MUST be buffered cap 1 (see rotateReply)
// so the loop never blocks replying to a caller that has already given up on
// ctx-cancel.
type rotateReq struct {
	force bool
	busy  func() bool
	reply chan rotateReply
}

// rotateReply is the loop goroutine's response to a rotateReq.
type rotateReply struct {
	result RotateResult
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
