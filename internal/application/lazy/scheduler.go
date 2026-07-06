package lazy

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vpntunnel/internal/domain"
	"vpntunnel/internal/infrastructure/notify"
	"vpntunnel/internal/publicerror"
)

// PrefixUnknownZone and PrefixZoneBringUpFailure are the leading tokens of the
// publicerror messages OnDemandScheduler.Route returns for its two routing
// failure classes. They are exported so a caller (the async forwarder) can
// classify a Route error into an HTTP status without coupling to the full,
// human-readable message text — a single source of truth shared by the producer
// (Route) and the consumer.
const (
	PrefixUnknownZone        = "unknown_zone:"
	PrefixZoneBringUpFailure = "zone_bring_up_failure:"
)

// SchedulerOptions holds all dependencies for NewOnDemandScheduler.
type SchedulerOptions struct {
	// Eligible is the country-filtered pool used to validate zone IDs. Required.
	Eligible *EligibleSet
	// DeviceBuilder builds a live DialerCloser from a config path. When nil,
	// DefaultDeviceBuilder is used. Inject a fake in tests.
	DeviceBuilder DeviceBuilderFn
	// SettleDelay is the mandatory pause between tearing down one on-demand
	// device and bringing the next up. Required > 0.
	SettleDelay time.Duration
	// Grace is the time the scheduler waits after the last same-zone active job
	// before switching to the next zone's oldest pending job. Required > 0.
	Grace time.Duration
	// IdleTTL is how long the scheduler keeps a live device after the last
	// request before proactively tearing it down. Required > 0.
	IdleTTL time.Duration
	// Clock abstracts time operations for tests. When nil, NewRealClock() is used.
	Clock Clock
	// ConfigDir is used to resolve relative config paths via DeviceBuilder. Required.
	ConfigDir string
	// OpLog is the operational slog logger. When nil, slog.Default() is used.
	OpLog *slog.Logger
	// Notifier reports on-demand zone-switch events. Optional; defaults to
	// notify.Nop{} when nil so call sites never need to nil-check.
	Notifier notify.Notifier
}

// NewOnDemandScheduler constructs an OnDemandScheduler from opts. It panics
// if any required field is zero/nil. Call Run to begin the device lifecycle loop.
func NewOnDemandScheduler(opts SchedulerOptions) *OnDemandScheduler {
	if opts.Eligible == nil {
		panic("lazy: OnDemandScheduler requires a non-nil EligibleSet")
	}
	if opts.SettleDelay <= 0 {
		panic("lazy: OnDemandScheduler requires SettleDelay > 0")
	}
	if opts.Grace <= 0 {
		panic("lazy: OnDemandScheduler requires Grace > 0")
	}
	if opts.IdleTTL <= 0 {
		panic("lazy: OnDemandScheduler requires IdleTTL > 0")
	}
	if opts.ConfigDir == "" {
		panic("lazy: OnDemandScheduler requires non-empty ConfigDir")
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

	return &OnDemandScheduler{
		eligible:      opts.Eligible,
		deviceBuilder: deviceBuilder,
		settleDelay:   opts.SettleDelay,
		grace:         opts.Grace,
		idleTTL:       opts.IdleTTL,
		clock:         clk,
		configDir:     opts.ConfigDir,
		opLog:         opts.OpLog,
		notifier:      notifier,
		reqCh:         make(chan routeRequest, schedulerChanBuf),
		releaseCh:     make(chan struct{}, schedulerChanBuf),
		done:          make(chan struct{}),
	}
}

// OnDemandScheduler owns at most one on-demand WireGuard device,
// time-multiplexed across zones. Zone switches obey a mandatory settle delay;
// the device is kept warm for IdleTTL after the last request.
//
// Route is the primary entry point: it validates the requested zone, enqueues
// the request into the Run loop, and blocks until the loop grants the dialer or
// returns an error.
//
// Run owns all device lifecycle decisions. It must be started before any Route
// calls; it exits on ctx cancellation and closes any live device. Run never
// blocks for the settle delay: a zone switch is modelled as a "settling" state
// inside the select loop, so concurrent same-zone requests arriving during the
// settle are folded into the in-flight batch and ctx cancellation is honoured
// immediately.
//
// Concurrency within the active zone is allowed (RESOLVED #6): multiple
// goroutines may hold the same device simultaneously when they target the same
// zone. Zone switches are serialized — the Run loop performs them one at a time
// after the grace window elapses.
//
// LiveHealth returns a snapshot of the current device for the health
// endpoint. It is safe for concurrent calls.
type OnDemandScheduler struct {
	eligible      *EligibleSet
	deviceBuilder DeviceBuilderFn
	settleDelay   time.Duration
	grace         time.Duration
	idleTTL       time.Duration
	clock         Clock
	configDir     string
	opLog         *slog.Logger
	notifier      notify.Notifier

	reqCh     chan routeRequest
	releaseCh chan struct{}
	// done is closed when Run exits so a release() call racing with shutdown
	// neither blocks forever nor is silently dropped.
	done chan struct{}

	// snapMu guards the snapshot fields below, written by Run and read by
	// LiveHealth. Using a mutex (not atomic) because reporter is an interface.
	snapMu       sync.RWMutex
	snapID       string
	snapReporter domain.HealthReporter
}

// Route acquires a dialer/resolver for the given tunnel and returns a release
// function the caller MUST defer. The release signals the scheduler that the
// job is done, so idle and grace timers are accurate.
//
// Returns a *publicerror.Error for unknown tunnels or device bring-up failures.
// Returns ctx.Err() when ctx is cancelled while waiting.
func (s *OnDemandScheduler) Route(ctx context.Context, tunnelID string) (dialer domain.Dialer, resolver domain.Resolver, release func(), err error) {
	configPath, ok := s.eligible.Lookup(tunnelID)
	if !ok {
		// tunnelID is an opaque string (HMAC id or basename) chosen by the operator
		// and not key material, so it is safe to echo in this client-facing error.
		return nil, nil, nil, publicerror.New(PrefixUnknownZone + " " + tunnelID + " is not in the eligible set")
	}

	replyCh := make(chan routeReply, 1)
	req := routeRequest{
		tunnelID:   tunnelID,
		configPath: configPath,
		replyCh:    replyCh,
	}

	select {
	case s.reqCh <- req:
	case <-ctx.Done():
		return nil, nil, nil, ctx.Err()
	}

	select {
	case reply := <-replyCh:
		if reply.err != nil {
			return nil, nil, nil, reply.err
		}
		rel := func() {
			// blocking send guarded by done: while Run is alive the send always
			// lands (so activeJobs stays exact, which the grace/idle timers rely
			// on); once Run has exited, done unblocks us instead of leaking.
			select {
			case s.releaseCh <- struct{}{}:
			case <-s.done:
			}
		}
		return reply.dialer, reply.resolver, rel, nil
	case <-ctx.Done():
		return nil, nil, nil, ctx.Err()
	}
}

// LiveHealth returns a health snapshot of the currently live on-demand device.
// ok is false when no device is live. Safe for concurrent callers; the snapshot
// may be transiently stale when a switch is in progress.
//
// The returned TunnelHealth.Err is non-nil when the HealthReporter failed to
// query its transport. Callers must not log TunnelHealth.Err verbatim in
// external responses (CLAUDE.md: never expose Err text in the health body).
func (s *OnDemandScheduler) LiveHealth() (domain.TunnelHealth, bool) {
	s.snapMu.RLock()
	id := s.snapID
	rep := s.snapReporter
	s.snapMu.RUnlock()

	if rep == nil {
		return domain.TunnelHealth{}, false
	}
	hs, err := rep.LastHandshake()
	return domain.TunnelHealth{ID: id, LastHandshake: hs, Err: err}, true
}

// Run is the device lifecycle loop. It must be called exactly once, in a
// goroutine. It exits when ctx is cancelled, closing any live device and
// failing all pending (and any in-flight settling) Route calls with ctx.Err().
func (s *OnDemandScheduler) Run(ctx context.Context) {
	defer close(s.done)

	var (
		currentZone     string
		currentDevice   domain.DialerCloser
		currentResolver domain.Resolver

		activeJobs int

		// pending maps zone id → FIFO slice of waiting route requests.
		pending     = make(map[string][]routeRequest)
		pendingFIFO []string // zone IDs in arrival order (deduplicated)

		graceTimer <-chan time.Time
		idleTimer  <-chan time.Time

		// settling state: while a zone switch's settle delay is in flight the
		// loop must NOT block. It keeps servicing reqCh (folding same-zone
		// requests into settleBatch), releaseCh, and ctx. The new device is
		// built only when settleTimer fires (finishSwitch).
		settling         bool
		settleZone       string
		settleConfigPath string
		settleBatch      []routeRequest
		settleTimer      <-chan time.Time
	)

	setSnapshot := func(id string, rep domain.HealthReporter) {
		s.snapMu.Lock()
		s.snapID = id
		s.snapReporter = rep
		s.snapMu.Unlock()
	}

	grant := func(req routeRequest, d domain.DialerCloser, res domain.Resolver) {
		activeJobs++
		req.replyCh <- routeReply{dialer: d, resolver: res}
	}

	grantAll := func(reqs []routeRequest, d domain.DialerCloser, res domain.Resolver) {
		for _, req := range reqs {
			grant(req, d, res)
		}
	}

	failAll := func(reqs []routeRequest, err error) {
		for _, req := range reqs {
			req.replyCh <- routeReply{err: err}
		}
	}

	enqueuePending := func(req routeRequest) {
		if _, exists := pending[req.tunnelID]; !exists {
			pendingFIFO = append(pendingFIFO, req.tunnelID)
		}
		pending[req.tunnelID] = append(pending[req.tunnelID], req)
	}

	teardown := func() {
		if currentDevice == nil {
			return
		}
		d := currentDevice
		id := currentZone
		currentDevice = nil
		currentZone = ""
		currentResolver = nil
		setSnapshot("", nil)
		if err := d.Close(); err != nil {
			s.logger().Warn("on-demand scheduler: close failed during teardown",
				slog.String("tunnel_id", id),
				slog.String("err", err.Error()),
			)
		}
		s.logger().Info("on-demand scheduler: device torn down",
			slog.String("tunnel_id", id),
		)
	}

	// compactFIFO drops zone IDs whose pending queue has drained. pendingFIFO is
	// kept duplicate-free on insert (enqueuePending only appends a zone on its
	// first request), so an in-place two-pointer pass with no allocation suffices.
	compactFIFO := func() {
		w := 0
		for _, z := range pendingFIFO {
			if len(pending[z]) > 0 {
				pendingFIFO[w] = z
				w++
			}
		}
		pendingFIFO = pendingFIFO[:w]
	}

	// pickNextZone selects the zone (excluding excludeZone) with the oldest
	// pending entry. Returns "" and nil when no eligible zone is found.
	// Ownership of the returned requests is transferred to the caller; the
	// queue entry is removed from pending and pendingFIFO.
	pickNextZone := func(excludeZone string) (string, []routeRequest) {
		compactFIFO()
		for i, z := range pendingFIFO {
			if z != excludeZone && len(pending[z]) > 0 {
				reqs := pending[z]
				delete(pending, z)
				pendingFIFO = append(pendingFIFO[:i], pendingFIFO[i+1:]...)
				return z, reqs
			}
		}
		return "", nil
	}

	hasCrossZonePending := func() bool {
		for z, reqs := range pending {
			if z != currentZone && len(reqs) > 0 {
				return true
			}
		}
		return false
	}

	// beginSwitch tears down the current device and enters the settling state
	// for tunnelID. The device is built only when settleTimer fires (finishSwitch);
	// the loop keeps running meanwhile. batch holds the same-tunnel requests to
	// resolve when the build completes; more same-tunnel requests arriving during
	// the settle are appended to settleBatch.
	beginSwitch := func(tunnelID, configPath string, batch []routeRequest) {
		teardown()
		settling = true
		settleZone = tunnelID
		settleConfigPath = configPath
		settleBatch = batch
		graceTimer = nil
		idleTimer = nil
		s.logger().Info("on-demand scheduler: settling before new zone",
			slog.String("tunnel_id", tunnelID),
			slog.String("settle_delay", s.settleDelay.String()),
		)
		settleTimer = s.clock.After(s.settleDelay)
	}

	// startNextIfPending begins a switch to the oldest pending tunnel when no
	// device is live and no switch is in progress. Used after a bring-up failure
	// so a failed tunnel does not stall service to the other tunnels.
	startNextIfPending := func() {
		tunnelID, reqs := pickNextZone("")
		if tunnelID == "" {
			return
		}
		configPath, _ := s.eligible.Lookup(tunnelID)
		beginSwitch(tunnelID, configPath, reqs)
	}

	// finishSwitch builds the settling tunnel's device (settleTimer has fired). On
	// success it grants the whole batch; on bring-up failure it fails the whole
	// batch with a publicerror and advances to the next pending domain.
	finishSwitch := func() {
		settling = false
		settleTimer = nil
		tunnelID := settleZone
		configPath := settleConfigPath
		batch := settleBatch
		settleZone = ""
		settleConfigPath = ""
		settleBatch = nil

		d, err := s.deviceBuilder(ctx, configPath, s.configDir, s.logger())
		if err != nil {
			buildErr := publicerror.New(PrefixZoneBringUpFailure + " " + tunnelID + " device could not be started")
			s.logger().Warn("on-demand scheduler: device bring-up failed",
				slog.String("tunnel_id", tunnelID),
				slog.String("err", err.Error()),
			)
			failAll(batch, buildErr)
			startNextIfPending()
			return
		}

		res, _ := d.(domain.Resolver)
		rep, _ := d.(domain.HealthReporter)
		currentZone = tunnelID
		currentDevice = d
		currentResolver = res
		setSnapshot(tunnelID, rep)
		s.logger().Info("on-demand scheduler: device built",
			slog.String("tunnel_id", tunnelID),
		)
		grantAll(batch, d, res)
		s.notifyChange(configPath, d)
	}

	for {
		select {
		case <-ctx.Done():
			if settling {
				failAll(settleBatch, ctx.Err())
				settleBatch = nil
			}
			for zone, reqs := range pending {
				failAll(reqs, ctx.Err())
				delete(pending, zone)
			}
			teardown()
			s.logger().Info("on-demand scheduler: stopped")
			return

		case req := <-s.reqCh:
			if settling {
				// a switch is in flight: same-tunnel requests join the batch,
				// cross-tunnel requests queue for later.
				if req.tunnelID == settleZone {
					settleBatch = append(settleBatch, req)
				} else {
					enqueuePending(req)
				}
				continue
			}

			if currentDevice != nil && currentZone == req.tunnelID {
				// same tunnel, live device: grant immediately and cancel any
				// pending grace/idle timers (fresh demand on the current tunnel).
				grant(req, currentDevice, currentResolver)
				graceTimer = nil
				idleTimer = nil
				continue
			}

			if currentDevice != nil {
				// different zone: queue it and, if no same-zone job is active,
				// arm the grace timer so the switch happens after the window.
				enqueuePending(req)
				if activeJobs == 0 && graceTimer == nil {
					graceTimer = s.clock.After(s.grace)
					idleTimer = nil
				}
				continue
			}

			// no device live and not settling: switch to the requested tunnel,
			// folding in any already-queued same-tunnel requests.
			samePending := pending[req.tunnelID]
			delete(pending, req.tunnelID)
			pendingFIFO = removeFIFO(pendingFIFO, req.tunnelID)
			batch := append([]routeRequest{req}, samePending...)
			beginSwitch(req.tunnelID, req.configPath, batch)

		case <-settleTimer:
			finishSwitch()

		case <-s.releaseCh:
			if activeJobs > 0 {
				activeJobs--
			}
			if activeJobs > 0 || settling {
				// more jobs in flight, or a switch is already underway.
				continue
			}
			// last active job released: arm grace if another zone is waiting,
			// otherwise arm the idle TTL on the live device.
			if hasCrossZonePending() {
				graceTimer = s.clock.After(s.grace)
				idleTimer = nil
			} else if currentDevice != nil {
				idleTimer = s.clock.After(s.idleTTL)
				graceTimer = nil
			}

		case <-graceTimer:
			graceTimer = nil
			nextTunnelID, queued := pickNextZone(currentZone)
			if nextTunnelID == "" {
				// no cross-tunnel work left — go idle on the current device.
				if currentDevice != nil {
					idleTimer = s.clock.After(s.idleTTL)
				}
				continue
			}
			configPath, _ := s.eligible.Lookup(nextTunnelID)
			beginSwitch(nextTunnelID, configPath, queued)

		case <-idleTimer:
			idleTimer = nil
			if currentDevice != nil && activeJobs == 0 {
				s.logger().Info("on-demand scheduler: idle TTL elapsed; tearing down",
					slog.String("tunnel_id", currentZone),
				)
				teardown()
			}
		}
	}
}

func (s *OnDemandScheduler) logger() *slog.Logger {
	if s.opLog != nil {
		return s.opLog
	}
	return slog.Default()
}

// notifyChange reports a successful on-demand zone switch to the configured
// Notifier. The zone id passed around the Run loop is the opaque HMAC id, not
// the .conf basename, so the filename/country must be derived from
// configPath instead. It is called only on the success branch of
// finishSwitch, after the new device is already live.
func (s *OnDemandScheduler) notifyChange(configPath string, d domain.Dialer) {
	base := filepath.Base(configPath)
	cc := strings.ToLower(domain.CountryFromID(strings.TrimSuffix(base, ".conf")))
	title := "on-demand: " + cc
	if cc == "" {
		title = "on-demand: " + base
	}

	s.notifier.Notify(context.Background(), notify.Event{
		Source:   notify.SourceOnDemand,
		Title:    title,
		Country:  cc,
		Filename: base,
		Dialer:   d,
	})
}

// routeRequest is a single Route call enqueued to the Run loop.
type routeRequest struct {
	tunnelID   string
	configPath string
	replyCh    chan<- routeReply
}

// routeReply is the Run loop's response to a routeRequest.
type routeReply struct {
	dialer   domain.Dialer
	resolver domain.Resolver
	err      error
}

// schedulerChanBuf is the capacity of the request and release channels.
// Sized generously so Route callers don't block when Run is briefly busy.
const schedulerChanBuf = 256

// removeFIFO removes the first occurrence of v from ss in-place and returns
// the resulting slice.
func removeFIFO(ss []string, v string) []string {
	for i, s := range ss {
		if s == v {
			return append(ss[:i], ss[i+1:]...)
		}
	}
	return ss
}
