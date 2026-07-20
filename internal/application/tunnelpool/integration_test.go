package tunnelpool

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/domain"
	"vpntunnel/internal/egress"
	"vpntunnel/internal/publicerror"
	"vpntunnel/internal/tools/hmackey"
)

// invariantDevice is an egress.DialerCloser tracked by the invariantBuilder. On
// Close, it decrements the shared live counter and records the zone id so the
// integration test can assert ordering.
type invariantDevice struct {
	zoneID  string
	counter *atomic.Int32
	events  *eventLog

	mu sync.Mutex
	hs time.Time
}

func (d *invariantDevice) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	// returns nil,nil so DialContext callers don't error but don't actually dial.
	return nil, nil
}

func (d *invariantDevice) Close() error {
	d.counter.Add(-1)
	d.events.record("close:" + d.zoneID)
	return nil
}

func (d *invariantDevice) LastHandshake() (time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hs, nil
}

// invariantDevice satisfies these interfaces at compile time.
var _ egress.DialerCloser = (*invariantDevice)(nil)
var _ egress.HealthReporter = (*invariantDevice)(nil)

// eventLog is a concurrency-safe ordered log of string events.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) record(e string) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]string, len(l.events))
	copy(cp, l.events)
	return cp
}

func (l *eventLog) await(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	if timeout < 2*time.Second {
		timeout = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		l.mu.Lock()
		got := len(l.events)
		l.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			l.mu.Lock()
			got = len(l.events)
			l.mu.Unlock()
			t.Fatalf("timed out waiting for %d event(s); got %v", n, l.snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// invariantBuilder is the shared DeviceBuilderFn used across both the supervisor
// and the scheduler in integration tests. It enforces the ≤2-live-device ceiling:
// the atomic live counter is incremented on each successful build; the Close of
// the returned invariantDevice decrements it. If a build would push the counter
// above maxLive the test fails immediately.
type invariantBuilder struct {
	counter *atomic.Int32
	maxLive int32
	events  *eventLog
	epoch   time.Time
	// violated is set true if a build would ever push the live-device count above
	// maxLive. It is checked from the owning subtest's goroutine — recording into
	// an atomic rather than calling t.Errorf from this background goroutine, which
	// is unsafe once the test has returned.
	violated atomic.Bool

	mu     sync.Mutex
	errFor map[string]bool // zone IDs whose build should fail
}

func newInvariantBuilder(t *testing.T, maxLive int32, events *eventLog, epoch time.Time) *invariantBuilder {
	t.Helper()
	return &invariantBuilder{
		counter: new(atomic.Int32),
		maxLive: maxLive,
		events:  events,
		epoch:   epoch,
		errFor:  make(map[string]bool),
	}
}

func (b *invariantBuilder) failZone(zoneID string) {
	b.mu.Lock()
	b.errFor[zoneID] = true
	b.mu.Unlock()
}

func (b *invariantBuilder) build(_ context.Context, configPath, _ string, _ *slog.Logger) (egress.DialerCloser, error) {
	zoneID := tunnelIDFromPath(configPath)

	b.mu.Lock()
	fail := b.errFor[zoneID]
	b.mu.Unlock()

	if fail {
		return nil, errBuildFailed(zoneID)
	}

	// increment BEFORE creating the device so Close (which decrements) is always
	// paired with a prior increment. Check the ceiling under the same increment so
	// there's no window between the check and the update.
	after := b.counter.Add(1)
	if after > b.maxLive {
		// structural invariant breach: record it for the test goroutine to assert,
		// then decrement so the counter stays consistent and refuse the build.
		b.violated.Store(true)
		b.counter.Add(-1)
		return nil, errBuildFailed(zoneID + ":invariant_violated")
	}

	d := &invariantDevice{
		zoneID:  zoneID,
		counter: b.counter,
		events:  b.events,
		hs:      b.epoch.Add(-time.Second), // fresh enough to pass health checks
	}

	b.events.record("build:" + zoneID)
	return d, nil
}

func (b *invariantBuilder) liveCount() int32 { return b.counter.Load() }

// errBuildFailed returns a plain error for a build failure — not a publicerror.
func errBuildFailed(zoneID string) error {
	return &buildFailedError{zone: zoneID}
}

type buildFailedError struct{ zone string }

func (e *buildFailedError) Error() string { return "build failed for zone: " + e.zone }

// integrationEligibleSet returns an EligibleSet with the given zone basenames,
// all rooted at the fake configDir "/fakedir/".
func integrationEligibleSet(t *testing.T, basenames ...string) *EligibleSet {
	t.Helper()
	cfgs := make([]string, len(basenames))
	for i, b := range basenames {
		cfgs[i] = "/fakedir/" + b + ".conf"
	}
	es, err := NewEligibleSet(cfgs, "/", nil)
	require.NoError(t, err)
	return es
}

// TestLazyTwoRoleFlow is the end-to-end integration test for the two-role lazy
// tunnel manager. It drives the real StreamingSupervisor and OnDemandScheduler
// through a shared fake DeviceBuilderFn that enforces the ≤2-live-device ceiling
// as a structural invariant: the invariantBuilder's atomic counter must never
// exceed 2 at any instant across all scenarios.
//
// Scenarios (subtests):
//  1. streaming_serves: supervisor brings up exactly one device; DialContext succeeds.
//  2. ondemand_AB_switch: zone-A → zone-B switch obeys close(A)→settle→build(B) ordering;
//     the live counter never exceeds 2 across the switch.
//  3. bringup_failure: a zone whose build fails → all callers receive a publicerror;
//     the live counter is unaffected.
//  4. health_live_only: LiveHealthModel reports only the live devices at each stage.
func TestLazyTwoRoleFlow(t *testing.T) {
	t.Parallel()

	t.Run("streaming_serves", func(t *testing.T) {
		t.Parallel()

		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		events := &eventLog{}
		ib := newInvariantBuilder(t, 2, events, epoch)
		clk := newFakeClock(epoch)

		es := integrationEligibleSet(t, "se-sto-wg-001")

		sup := NewStreamingSupervisor(SupervisorOptions{
			Eligible:        es,
			DeviceBuilder:   ib.build,
			HandshakeMaxAge: 3 * time.Minute,
			ReconnectMin:    10 * time.Minute,
			ReconnectMax:    3 * time.Hour,
			// large poll interval so the poll loop never fires during the test.
			PollInterval: 24 * time.Hour,
			RotateSettle: testRotateSettle,
			Clock:        clk,
			ConfigDir:    "/fakedir",
			OpLog:        slog.New(slog.DiscardHandler),
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))

		// wait for the first device to build.
		events.await(t, 1, 500*time.Millisecond)

		// exactly one device live; invariant holds.
		assert.Equal(t, int32(1), ib.liveCount(), "streaming: exactly one device live after Start")

		// DialContext must succeed (returns nil,nil from invariantDevice).
		conn, err := sup.DialContext(ctx, "tcp", "example.com:443")
		require.NoError(t, err)
		assert.Nil(t, conn, "DialContext returns nil conn from fake device")

		sup.Stop()

		// after Stop: device closed, counter back to 0.
		assert.Equal(t, int32(0), ib.liveCount(), "streaming: counter at 0 after Stop")

		// verify the event sequence: build then close.
		snap := events.snapshot()
		require.GreaterOrEqual(t, len(snap), 2, "need at least build+close events")
		assert.Equal(t, "build:se-sto-wg-001", snap[0])
		assert.Equal(t, "close:se-sto-wg-001", snap[len(snap)-1])

		assert.False(t, ib.violated.Load(), "the ≤2-live-device invariant was breached")
	})

	t.Run("ondemand_AB_switch", func(t *testing.T) {
		t.Parallel()

		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		events := &eventLog{}
		// maxLive=2: streaming(1) + on-demand(1) — the structural ceiling.
		ib := newInvariantBuilder(t, 2, events, epoch)
		clk := newFakeClock(epoch)

		// both roles share the same eligible set.
		es := integrationEligibleSet(t, "us-nyc-wg-001", "gb-lon-wg-001")

		const (
			grace        = 5 * time.Second
			settle       = 2 * time.Second
			idleTTL      = time.Hour
			handshakeAge = 3 * time.Minute
			reconnMin    = 10 * time.Minute
		)

		// streaming supervisor — uses invariantBuilder, maxLive=2. It runs a
		// dedicated single-zone eligible set (se-sto-wg-001) distinct from the
		// on-demand zones (us-nyc, gb-lon) so close events are unambiguous: the only
		// close:us-nyc-wg-001 in the log is the on-demand zone-A teardown.
		supES := integrationEligibleSet(t, "se-sto-wg-001")
		sup := NewStreamingSupervisor(SupervisorOptions{
			Eligible:        supES,
			DeviceBuilder:   ib.build,
			HandshakeMaxAge: handshakeAge,
			ReconnectMin:    reconnMin,
			ReconnectMax:    3 * time.Hour,
			PollInterval:    24 * time.Hour,
			RotateSettle:    testRotateSettle,
			// the supervisor gets its own clock: its 24h poll timer must not pollute
			// the scheduler's fake clock (otherwise AwaitTimers can't isolate the
			// scheduler's settle/grace timers). This clock is never advanced — the
			// poll never fires and Stop tears down via stopCh, not the clock.
			Clock:     newFakeClock(epoch),
			ConfigDir: "/fakedir",
			OpLog:     slog.New(slog.DiscardHandler),
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		events.await(t, 1, 500*time.Millisecond) // streaming device built

		assert.Equal(t, int32(1), ib.liveCount(), "after streaming start: counter=1")

		// on-demand scheduler over the 2-zone eligible set.
		sched := NewOnDemandScheduler(SchedulerOptions{
			Eligible:      es,
			DeviceBuilder: ib.build,
			SettleDelay:   settle,
			Grace:         grace,
			IdleTTL:       idleTTL,
			Clock:         clk,
			ConfigDir:     "/fakedir",
			OpLog:         slog.New(slog.DiscardHandler),
		})

		runDone := make(chan struct{})
		go func() {
			sched.Run(ctx)
			close(runDone)
		}()

		// route zone A (us-nyc-wg-001).
		routeACh := make(chan routeResult2, 1)
		go func() {
			d, _, rel, err := sched.Route(ctx, "us-nyc-wg-001")
			routeACh <- routeResult2{d: d, rel: rel, err: err}
		}()

		// wait for settle timer, then fire it.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer for zone A not registered")
		clk.Advance(settle)

		resA := <-routeACh
		require.NoError(t, resA.err, "zone A Route must succeed")
		assert.NotNil(t, resA.d)

		// counter: streaming(1) + on-demand-A(1) = 2. invariant holds.
		assert.Equal(t, int32(2), ib.liveCount(), "after zone A build: counter=2")

		// queue zone B while zone A is active, then release A. The grace timer is
		// armed only once BOTH the release and B's enqueue have been processed by
		// the Run loop — regardless of their order — so wait for a timer at exactly
		// the grace deadline rather than racing on a fixed sleep.
		routeBCh := make(chan routeResult2, 1)
		go func() {
			d, _, rel, err := sched.Route(ctx, "gb-lon-wg-001")
			routeBCh <- routeResult2{d: d, rel: rel, err: err}
		}()
		resA.rel()
		awaitTimerAt(t, clk, clk.Now().Add(grace), 2*time.Second)

		// advance grace → scheduler tears down A, starts settle for B.
		clk.Advance(grace)

		// zone A must close before zone B builds.
		awaitEventContaining(t, events, "close:us-nyc-wg-001", 2*time.Second)

		// counter after zone A teardown: streaming(1) + on-demand=0 = 1; the
		// invariant counter never exceeded 2 during the operation. Wait for the
		// zone-B settle timer at its exact deadline, then fire it.
		awaitTimerAt(t, clk, clk.Now().Add(settle), 2*time.Second)
		clk.Advance(settle)

		resB := <-routeBCh
		require.NoError(t, resB.err, "zone B Route must succeed")
		assert.NotNil(t, resB.d)

		// counter: streaming(1) + on-demand-B(1) = 2. invariant holds.
		assert.Equal(t, int32(2), ib.liveCount(), "after zone B build: counter=2")

		// assert event ordering: close:us before build:gb.
		snap := events.snapshot()
		closeAIdx := indexOfEvent(snap, "close:us-nyc-wg-001")
		buildBIdx := indexOfEvent(snap, "build:gb-lon-wg-001")
		require.GreaterOrEqual(t, closeAIdx, 0, "close:us-nyc-wg-001 event must exist")
		require.GreaterOrEqual(t, buildBIdx, 0, "build:gb-lon-wg-001 event must exist")
		assert.Less(t, closeAIdx, buildBIdx,
			"close(A) must happen before build(B); events: %v", snap)

		resB.rel()

		sup.Stop()
		cancel()

		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
			t.Fatal("scheduler Run did not exit after ctx cancel")
		}

		// after full shutdown: counter must be 0.
		assert.Equal(t, int32(0), ib.liveCount(), "after shutdown: counter=0")
		assert.False(t, ib.violated.Load(), "the ≤2-live-device invariant was breached")
	})

	t.Run("bringup_failure", func(t *testing.T) {
		t.Parallel()

		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		events := &eventLog{}
		ib := newInvariantBuilder(t, 2, events, epoch)
		clk := newFakeClock(epoch)

		// only the on-demand zone will fail; streaming zone succeeds.
		es := integrationEligibleSet(t, "us-nyc-wg-001", "gb-lon-wg-001")
		ib.failZone("us-nyc-wg-001") // on-demand zone fails; streaming uses gb-lon-wg-001

		// streaming uses gb-lon-wg-001 (the only non-failing zone in a separate set).
		supES := integrationEligibleSet(t, "gb-lon-wg-001")
		sup := NewStreamingSupervisor(SupervisorOptions{
			Eligible:        supES,
			DeviceBuilder:   ib.build,
			HandshakeMaxAge: 3 * time.Minute,
			ReconnectMin:    10 * time.Minute,
			ReconnectMax:    3 * time.Hour,
			PollInterval:    24 * time.Hour,
			RotateSettle:    testRotateSettle,
			// the supervisor gets its own clock: its 24h poll timer must not pollute
			// the scheduler's fake clock (otherwise AwaitTimers can't isolate the
			// scheduler's settle/grace timers). This clock is never advanced — the
			// poll never fires and Stop tears down via stopCh, not the clock.
			Clock:     newFakeClock(epoch),
			ConfigDir: "/fakedir",
			OpLog:     slog.New(slog.DiscardHandler),
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		events.await(t, 1, 500*time.Millisecond) // streaming device built

		assert.Equal(t, int32(1), ib.liveCount(), "streaming up; counter=1")

		sched := NewOnDemandScheduler(SchedulerOptions{
			Eligible:      es,
			DeviceBuilder: ib.build,
			SettleDelay:   time.Second,
			Grace:         time.Second,
			IdleTTL:       time.Hour,
			Clock:         clk,
			ConfigDir:     "/fakedir",
			OpLog:         slog.New(slog.DiscardHandler),
		})

		runDone := make(chan struct{})
		go func() {
			sched.Run(ctx)
			close(runDone)
		}()

		// Route three concurrent callers to the failing zone.
		const n = 3
		errChs := make([]chan error, n)
		for i := range n {
			errChs[i] = make(chan error, 1)
			go func(ch chan error) {
				_, _, _, err := sched.Route(ctx, "us-nyc-wg-001")
				ch <- err
			}(errChs[i])
		}

		// advance past settle → build attempt fails.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer not registered")
		clk.Advance(time.Second)

		// all three callers must receive a publicerror carrying the bring-up-failure prefix.
		for i, ch := range errChs {
			err := <-ch
			require.Error(t, err, "caller %d: expected error on bring-up failure", i)
			pe, ok := publicerror.Is(err)
			require.True(t, ok, "caller %d: error must be a publicerror, got %T", i, err)
			assert.Contains(t, pe.Details(), PrefixZoneBringUpFailure,
				"caller %d: publicerror must mention the bring-up-failure prefix", i)
		}

		// counter must still be 1 (streaming only); the failing zone never incremented it.
		assert.Equal(t, int32(1), ib.liveCount(),
			"counter must remain 1 after failed on-demand build")

		sup.Stop()
		cancel()

		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
			t.Fatal("scheduler Run did not exit after ctx cancel")
		}

		assert.Equal(t, int32(0), ib.liveCount(), "counter=0 after shutdown")
		assert.False(t, ib.violated.Load(), "the ≤2-live-device invariant was breached")
	})

	t.Run("health_live_only", func(t *testing.T) {
		t.Parallel()

		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		events := &eventLog{}
		ib := newInvariantBuilder(t, 2, events, epoch)
		clk := newFakeClock(epoch)

		// streaming uses se-sto-wg-001; on-demand uses gb-lon-wg-001.
		supES := integrationEligibleSet(t, "se-sto-wg-001")
		odES := integrationEligibleSet(t, "se-sto-wg-001", "gb-lon-wg-001")

		sup := NewStreamingSupervisor(SupervisorOptions{
			Eligible:        supES,
			DeviceBuilder:   ib.build,
			HandshakeMaxAge: 3 * time.Minute,
			ReconnectMin:    10 * time.Minute,
			ReconnectMax:    3 * time.Hour,
			PollInterval:    24 * time.Hour,
			RotateSettle:    testRotateSettle,
			// the supervisor gets its own clock: its 24h poll timer must not pollute
			// the scheduler's fake clock (otherwise AwaitTimers can't isolate the
			// scheduler's settle/grace timers). This clock is never advanced — the
			// poll never fires and Stop tears down via stopCh, not the clock.
			Clock:     newFakeClock(epoch),
			ConfigDir: "/fakedir",
			OpLog:     slog.New(slog.DiscardHandler),
		})

		sched := NewOnDemandScheduler(SchedulerOptions{
			Eligible:      odES,
			DeviceBuilder: ib.build,
			SettleDelay:   time.Second,
			Grace:         time.Second,
			IdleTTL:       time.Hour,
			Clock:         clk,
			ConfigDir:     "/fakedir",
			OpLog:         slog.New(slog.DiscardHandler),
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		runDone := make(chan struct{})
		go func() {
			sched.Run(ctx)
			close(runDone)
		}()

		require.NoError(t, sup.Start(ctx))
		events.await(t, 1, 500*time.Millisecond) // streaming built

		// liveReports aggregates the two roles the same way handlers.LiveHealthModel
		// does (streaming first, on-demand only when live). The aggregator itself and
		// the /v1/admin/health body shape are covered by the handlers package tests;
		// here we only prove the live-only set tracks the device lifecycle end to end.
		liveReports := func() []domain.TunnelHealth {
			var out []domain.TunnelHealth
			if h, ok := sup.LiveHealth(); ok {
				out = append(out, h)
			}
			if h, ok := sched.LiveHealth(); ok {
				out = append(out, h)
			}
			return out
		}

		// stage 1: only streaming live before any on-demand request.
		streamH, ok := sup.LiveHealth()
		require.True(t, ok, "streaming device must be live")
		assert.Equal(t, "se-sto-wg-001", streamH.ID)
		_, odOk := sched.LiveHealth()
		assert.False(t, odOk, "no on-demand device should be live yet")
		assert.Len(t, liveReports(), 1)
		assertNoLeakedFields(t, liveReports())

		// bring up on-demand gb-lon-wg-001.
		routeGBCh := make(chan routeResult2, 1)
		go func() {
			d, _, rel, err := sched.Route(ctx, "gb-lon-wg-001")
			routeGBCh <- routeResult2{d: d, rel: rel, err: err}
		}()

		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer not registered")
		clk.Advance(time.Second) // settle fires → on-demand builds

		resGB := <-routeGBCh
		require.NoError(t, resGB.err, "gb-lon-wg-001 Route must succeed")

		// stage 2: streaming + on-demand both live.
		odH, ok := sched.LiveHealth()
		require.True(t, ok, "on-demand device must be live after Route")
		assert.Equal(t, "gb-lon-wg-001", odH.ID)
		reports := liveReports()
		require.Len(t, reports, 2, "both devices should be live")
		assert.Equal(t, "se-sto-wg-001", reports[0].ID)
		assert.Equal(t, "gb-lon-wg-001", reports[1].ID)
		assertNoLeakedFields(t, reports)

		// release and wait for idle teardown — after idle TTL elapses the on-demand
		// device disappears from the live set.
		resGB.rel()
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "idle timer not registered")
		clk.Advance(time.Hour) // idle TTL fires → on-demand teardown
		awaitEventContaining(t, events, "close:gb-lon-wg-001", 2*time.Second)

		// stage 3: only streaming again.
		_, odOk = sched.LiveHealth()
		assert.False(t, odOk, "on-demand device must be gone after idle teardown")
		require.Len(t, liveReports(), 1, "only streaming after on-demand idle teardown")

		sup.Stop()
		cancel()

		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
			t.Fatal("scheduler Run did not exit after ctx cancel")
		}

		assert.Equal(t, int32(0), ib.liveCount(), "counter=0 after shutdown")
		assert.False(t, ib.violated.Load(), "the ≤2-live-device invariant was breached")
	})

	t.Run("on-demand routes to a country excluded from the streaming filter", func(t *testing.T) {
		t.Parallel()

		// allConfigs contains two zones: se-sto-wg-001 (allowed by the streaming
		// filter) and us-nyc-wg-001 (excluded from streaming but routable on-demand).
		// Note: both share one WireGuard key in this fake build context — the
		// invariantBuilder ignores file contents, so the single-key invariant is
		// automatically satisfied.
		allConfigs := []string{
			"/fakedir/se-sto-wg-001.conf",
			"/fakedir/us-nyc-wg-001.conf",
		}
		configDir := "/fakedir"

		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		events := &eventLog{}
		// maxLive=2: streaming(se-sto=1) + on-demand(us-nyc=1) = ceiling.
		ib := newInvariantBuilder(t, 2, events, epoch)
		clk := newFakeClock(epoch)

		const settle = 2 * time.Second

		// country-filtered streaming set: only se-sto-wg-001 is eligible.
		streamingSet, err := NewEligibleSet(allConfigs, configDir, []string{"se"})
		require.NoError(t, err)
		assert.Equal(t, 1, streamingSet.Len(), "streaming set must contain only the se zone")

		// full set for on-demand: both zones present.
		// use a fixed 32-byte test key so HMAC ids are deterministic.
		integKey := []byte("integration-test-hmac-key-000000")
		fullSet, err := NewFullSet(allConfigs, configDir, integKey)
		require.NoError(t, err)
		assert.Equal(t, 2, fullSet.Len(), "full set must contain all discovered zones")

		// streaming supervisor uses the filtered set — can only ever pick se-sto.
		sup := NewStreamingSupervisor(SupervisorOptions{
			Eligible:        streamingSet,
			DeviceBuilder:   ib.build,
			HandshakeMaxAge: 3 * time.Minute,
			ReconnectMin:    10 * time.Minute,
			ReconnectMax:    3 * time.Hour,
			PollInterval:    24 * time.Hour,
			RotateSettle:    testRotateSettle,
			Clock:           newFakeClock(epoch),
			ConfigDir:       configDir,
			OpLog:           slog.New(slog.DiscardHandler),
		})

		// on-demand scheduler uses the full set — can route to any discovered zone.
		sched := NewOnDemandScheduler(SchedulerOptions{
			Eligible:      fullSet,
			DeviceBuilder: ib.build,
			SettleDelay:   settle,
			Grace:         5 * time.Second,
			IdleTTL:       time.Hour,
			Clock:         clk,
			ConfigDir:     configDir,
			OpLog:         slog.New(slog.DiscardHandler),
		})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		runDone := make(chan struct{})
		go func() {
			sched.Run(ctx)
			close(runDone)
		}()

		require.NoError(t, sup.Start(ctx))

		// wait for the streaming supervisor to build its device. Because streamingSet
		// holds only se-sto-wg-001, RandomPath can only ever return that zone — this
		// is the core property under test.
		awaitEventContaining(t, events, "build:se-sto-wg-001", 2*time.Second)

		// assert us-nyc was NOT built by the supervisor — the snapshot at this point
		// contains only the streaming build event.
		snapBefore := events.snapshot()
		for _, e := range snapBefore {
			assert.NotEqual(t, "build:us-nyc-wg-001", e,
				"us-nyc must not be built by the streaming supervisor; events so far: %v", snapBefore)
		}

		// now prove the on-demand scheduler CAN route to us-nyc (excluded by
		// the streaming filter but present in the full set).
		// the full set is keyed by HMAC id, so derive the id from the basename.
		usNycID := hmackey.DeriveID(integKey, "us-nyc-wg-001")
		routeUSCh := make(chan routeResult2, 1)
		go func() {
			d, _, rel, err := sched.Route(ctx, usNycID)
			routeUSCh <- routeResult2{d: d, rel: rel, err: err}
		}()

		// wait for the settle timer then fire it.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer for us-nyc not registered")
		clk.Advance(settle)

		resUS := <-routeUSCh
		require.NoError(t, resUS.err, "on-demand Route to us-nyc must succeed")
		assert.NotNil(t, resUS.d, "on-demand dialer for us-nyc must be non-nil")

		// after the successful Route the on-demand build event is present — that is
		// the expected and permitted state (on-demand built the country the streaming
		// filter excluded).
		awaitEventContaining(t, events, "build:us-nyc-wg-001", 2*time.Second)

		// the ≤2-live-device invariant must still hold (streaming=1 + on-demand=1).
		assert.Equal(t, int32(2), ib.liveCount(), "live count must be 2: streaming + on-demand")

		// prove the full set is not blanket-accept-all: de-fra is not in allConfigs.
		// pass the HMAC id (not the basename) since the full set is keyed by HMAC id.
		deFraID := hmackey.DeriveID(integKey, "de-fra-wg-001")
		_, _, _, deErr := sched.Route(ctx, deFraID)
		require.Error(t, deErr, "routing to an unknown zone must return an error")
		pe, ok := publicerror.Is(deErr)
		require.True(t, ok, "unknown-zone error must be a publicerror, got %T: %v", deErr, deErr)
		assert.Contains(t, pe.Details(), PrefixUnknownZone,
			"unknown-zone publicerror must carry PrefixUnknownZone")

		// release the on-demand device and shut down.
		if resUS.rel != nil {
			resUS.rel()
		}

		sup.Stop()
		cancel()

		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
			t.Fatal("scheduler Run did not exit after ctx cancel")
		}

		assert.Equal(t, int32(0), ib.liveCount(), "counter must be 0 after full shutdown")
		assert.False(t, ib.violated.Load(), "the ≤2-live-device invariant was breached")
	})
}

// routeResult2 captures the return values of a single scheduler Route call.
type routeResult2 struct {
	d   egress.Dialer
	rel func()
	err error
}

// assertNoLeakedFields verifies that a []domain.TunnelHealth slice does not
// contain any fields that must not appear in external responses
// (CLAUDE.md invariant: never expose Err text, key material, or peer endpoint
// in any handler-visible output). Health entries from LiveHealthModel are
// projected through the JSON encoder; we round-trip through JSON and check the
// decoded map for forbidden keys.
func assertNoLeakedFields(t *testing.T, reports []domain.TunnelHealth) {
	t.Helper()
	forbidden := []string{"peer_endpoint", "err", "peerPublicKey", "peer_public_key", "private_key"}
	for _, rep := range reports {
		data, err := json.Marshal(rep)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(data, &m))
		for _, key := range forbidden {
			_, ok := m[key]
			assert.False(t, ok, "TunnelHealth must not expose field %q in serialised form; report: %v", key, m)
		}
	}
}

// awaitTimerAt blocks until clk has a pending timer whose deadline equals at.
// Unlike fakeClock.AwaitTimers (which only counts timers), this waits for a
// SPECIFIC deadline, so it is unaffected by other timers already armed on the
// same clock and is immune to the order in which two events (e.g. a release and
// a cross-zone enqueue) arm the timer being awaited.
func awaitTimerAt(t *testing.T, clk *fakeClock, at time.Time, timeout time.Duration) {
	t.Helper()
	if timeout < 2*time.Second {
		timeout = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		clk.mu.Lock()
		for _, tm := range clk.timers {
			if tm.deadline.Equal(at) {
				clk.mu.Unlock()
				return
			}
		}
		clk.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no timer armed at offset %v within %v", at, timeout)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// awaitEventContaining blocks until an event containing substr appears in the log.
func awaitEventContaining(t *testing.T, events *eventLog, substr string, timeout time.Duration) {
	t.Helper()
	if timeout < 2*time.Second {
		timeout = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		for _, e := range events.snapshot() {
			if e == substr {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for event %q; got: %v", substr, events.snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// indexOfEvent returns the index of the first occurrence of event in snap, or -1.
func indexOfEvent(snap []string, event string) int {
	for i, e := range snap {
		if e == event {
			return i
		}
	}
	return -1
}
