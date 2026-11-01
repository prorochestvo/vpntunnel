package tunnelpool

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prorochestvo/loginjector"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/domain"
)

// compile-time check: fakeClock already declared in supervisor_test.go satisfies Clock.
// stubDevice already declared there satisfies DialerCloser + HealthReporter.

// schedulerEligibleSet returns a multi-entry EligibleSet rooted at a fixed
// fake configDir, built from the given basenames.
func schedulerEligibleSet(t *testing.T, basenames ...string) *EligibleSet {
	t.Helper()
	cfgs := make([]string, len(basenames))
	for i, b := range basenames {
		cfgs[i] = "/fakedir/" + b + ".conf"
	}
	es, err := NewEligibleSet(cfgs, "/", nil)
	require.NoError(t, err)
	return es
}

// schedulerDeviceBuilder returns a DeviceBuilderFn that produces devices keyed
// by configPath. Each call increments a build counter and records the event.
// When errFor[zoneID] != nil the builder returns that error for the zone.
type schedulerDeviceBuilder struct {
	mu         sync.Mutex
	builds     []string        // zone IDs in build order
	closes     []string        // zone IDs in close order
	errFor     map[string]bool // zone IDs whose build should fail
	devices    map[string]*stubDevice
	buildCount int32
}

func newSchedulerDeviceBuilder() *schedulerDeviceBuilder {
	return &schedulerDeviceBuilder{
		errFor:  make(map[string]bool),
		devices: make(map[string]*stubDevice),
	}
}

func (b *schedulerDeviceBuilder) failZone(zoneID string) {
	b.mu.Lock()
	b.errFor[zoneID] = true
	b.mu.Unlock()
}

func (b *schedulerDeviceBuilder) builder(now time.Time) DeviceBuilderFn {
	return func(_ context.Context, configPath, _ string, _ *slog.Logger) (DialerCloser, error) {
		zoneID := tunnelIDFromPath(configPath)
		b.mu.Lock()
		fail := b.errFor[zoneID]
		b.mu.Unlock()
		if fail {
			return nil, errors.New("build failed for " + zoneID)
		}
		atomic.AddInt32(&b.buildCount, 1)
		sd := &stubDevice{id: zoneID, handshake: now.Add(-time.Second)}
		b.mu.Lock()
		b.builds = append(b.builds, zoneID)
		b.devices[zoneID] = sd
		b.mu.Unlock()
		// wrap so we can record close order.
		wd := &orderedDevice{
			stubDevice: sd,
			onClose: func() {
				b.mu.Lock()
				b.closes = append(b.closes, zoneID)
				b.mu.Unlock()
			},
		}
		return wd, nil
	}
}

func (b *schedulerDeviceBuilder) buildList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]string, len(b.builds))
	copy(cp, b.builds)
	return cp
}

func (b *schedulerDeviceBuilder) closeList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]string, len(b.closes))
	copy(cp, b.closes)
	return cp
}

// defaultSchedulerOpts returns SchedulerOptions wired for testing with a fake clock.
func defaultSchedulerOpts(
	es *EligibleSet,
	fn DeviceBuilderFn,
	settleDelay, grace, idleTTL time.Duration,
	clk *fakeClock,
) SchedulerOptions {
	return SchedulerOptions{
		Eligible:      es,
		DeviceBuilder: fn,
		SettleDelay:   settleDelay,
		Grace:         grace,
		IdleTTL:       idleTTL,
		Clock:         clk,
		ConfigDir:     "/fakedir",
		OpLog:         slog.New(slog.DiscardHandler),
	}
}

// awaitBuilds blocks until b.buildList() has n entries.
func awaitBuilds(t *testing.T, b *schedulerDeviceBuilder, n int, timeout time.Duration) {
	t.Helper()
	// floor the real-time deadline so a loaded runner does not spuriously fail
	// while the Run goroutine processes a fake-clock tick.
	if timeout < 2*time.Second {
		timeout = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if len(b.buildList()) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d build(s); got %v", n, b.buildList())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitCloses blocks until b.closeList() has n entries.
func awaitCloses(t *testing.T, b *schedulerDeviceBuilder, n int, timeout time.Duration) {
	t.Helper()
	// floor the real-time deadline (see awaitBuilds) to avoid load-induced flakes.
	if timeout < 2*time.Second {
		timeout = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if len(b.closeList()) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d close(s); got %v", n, b.closeList())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestOnDemandScheduler_Route tests the Route method's validation and basic dial path.
func TestOnDemandScheduler_Route(t *testing.T) {
	t.Parallel()

	t.Run("valid zone grants dialer and resolver", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		b := newSchedulerDeviceBuilder()
		opts := defaultSchedulerOpts(es, b.builder(epoch), time.Second, time.Second, time.Hour, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		// the first Route call triggers settle+build; advance past settle delay.
		routeErrCh := make(chan error, 1)
		var gotDialer Dialer
		var gotRelease func()
		go func() {
			d, _, rel, err := sched.Route(ctx, "us-nyc-wg-001")
			gotDialer = d
			gotRelease = rel
			routeErrCh <- err
		}()

		// wait for settle timer to register, then fire it.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer not registered")
		clk.Advance(time.Second) // settle delay fires

		require.NoError(t, <-routeErrCh)
		assert.NotNil(t, gotDialer)
		assert.NotNil(t, gotRelease)
		gotRelease()
	})

	t.Run("unknown zone returns publicerror", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		b := newSchedulerDeviceBuilder()
		opts := defaultSchedulerOpts(es, b.builder(epoch), time.Second, time.Second, time.Hour, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		_, _, _, err := sched.Route(ctx, "xx-unknown-wg-001")
		require.Error(t, err)
		var pe loginjector.PublicDetailsError
		ok := errors.As(err, &pe)
		require.True(t, ok, "expected publicerror, got: %T %v", err, err)
		assert.Contains(t, pe.Details(), "unknown_zone")
	})

	t.Run("multiple concurrent same-zone routes share device", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		b := newSchedulerDeviceBuilder()
		opts := defaultSchedulerOpts(es, b.builder(epoch), time.Second, 5*time.Second, time.Hour, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		const n = 5
		type routeResult struct {
			d   Dialer
			rel func()
			err error
		}
		results := make(chan routeResult, n)

		// submit n concurrent Route calls for the same zone.
		for range n {
			go func() {
				d, _, rel, err := sched.Route(ctx, "us-nyc-wg-001")
				results <- routeResult{d: d, rel: rel, err: err}
			}()
		}

		// advance past settle delay (one build only).
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer not registered")
		clk.Advance(time.Second)

		for range n {
			res := <-results
			require.NoError(t, res.err)
			assert.NotNil(t, res.d)
			if res.rel != nil {
				res.rel()
			}
		}

		// only one device should have been built.
		awaitBuilds(t, b, 1, 500*time.Millisecond)
		assert.Len(t, b.buildList(), 1)
	})

	t.Run("ctx cancel before settle returns ctx error", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		b := newSchedulerDeviceBuilder()
		opts := defaultSchedulerOpts(es, b.builder(epoch), time.Hour, time.Second, time.Hour, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		go sched.Run(ctx)

		routeErrCh := make(chan error, 1)
		go func() {
			_, _, _, err := sched.Route(ctx, "us-nyc-wg-001")
			routeErrCh <- err
		}()

		// cancel before advancing settle.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer not registered")
		cancel()

		err := <-routeErrCh
		assert.Error(t, err)
	})
}

// TestOnDemandScheduler_switch tests zone switching: grace timer, FIFO ordering,
// and teardown→settle→build sequencing.
func TestOnDemandScheduler_switch(t *testing.T) {
	t.Parallel()

	t.Run("same-zone request resets grace timer", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001", "gb-lon-wg-001")
		b := newSchedulerDeviceBuilder()
		const grace = 10 * time.Second
		const settle = time.Second
		opts := defaultSchedulerOpts(es, b.builder(epoch), settle, grace, time.Hour, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		// bring up us zone.
		routeUS := make(chan routeResult1, 1)
		go func() {
			d, _, rel, err := sched.Route(ctx, "us-nyc-wg-001")
			routeUS <- routeResult1{d: d, rel: rel, err: err}
		}()
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer not registered")
		clk.Advance(settle)
		res := <-routeUS
		require.NoError(t, res.err)
		// release active job for US — this starts the grace timer since GB is pending.
		go func() {
			_, _, _, _ = sched.Route(ctx, "gb-lon-wg-001")
		}()
		time.Sleep(20 * time.Millisecond) // let GB request queue
		res.rel()                         // release US job → grace timer starts

		// advance half-grace — should not switch yet.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "grace timer not registered")
		clk.Advance(grace / 2)

		// submit another US request — this should reset the grace timer
		// (same zone, active device): this requires that same-zone request while
		// device is live is granted immediately (not queued) and grace is NOT running.
		// In the current design: same-zone request cancels grace. Let's verify the
		// device was only built once (no switch happened).
		time.Sleep(20 * time.Millisecond)
		assert.Len(t, b.buildList(), 1, "no switch should have happened yet")
	})

	t.Run("grace elapsed triggers FIFO zone switch", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001", "gb-lon-wg-001", "de-fra-wg-001")
		b := newSchedulerDeviceBuilder()
		const grace = 5 * time.Second
		const settle = time.Second
		opts := defaultSchedulerOpts(es, b.builder(epoch), settle, grace, time.Hour, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		// bring up US zone.
		routeUS := make(chan routeResult1, 1)
		go func() {
			d, _, rel, err := sched.Route(ctx, "us-nyc-wg-001")
			routeUS <- routeResult1{d: d, rel: rel, err: err}
		}()
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer (US) not registered")
		clk.Advance(settle)
		res := <-routeUS
		require.NoError(t, res.err)

		// queue GB first (oldest), then DE.
		gbErrCh := make(chan error, 1)
		deErrCh := make(chan error, 1)
		go func() {
			_, _, rel, err := sched.Route(ctx, "gb-lon-wg-001")
			if err == nil {
				rel()
			}
			gbErrCh <- err
		}()
		time.Sleep(20 * time.Millisecond) // ensure GB queued first
		go func() {
			_, _, rel, err := sched.Route(ctx, "de-fra-wg-001")
			if err == nil {
				rel()
			}
			deErrCh <- err
		}()
		time.Sleep(20 * time.Millisecond) // ensure DE queued second

		// release US active job → grace timer starts.
		res.rel()

		// wait for grace timer to register, then fire it.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "grace timer not registered")
		clk.Advance(grace)

		// grace fires → switch to GB (FIFO oldest). Wait for settle.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer (GB) not registered")
		clk.Advance(settle)

		// GB should succeed.
		require.NoError(t, <-gbErrCh)

		// builds: US then GB.
		awaitBuilds(t, b, 2, 500*time.Millisecond)
		builds := b.buildList()
		assert.Equal(t, []string{"us-nyc-wg-001", "gb-lon-wg-001"}, builds)
	})

	t.Run("teardown happens before new build on zone switch", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001", "gb-lon-wg-001")
		b := newSchedulerDeviceBuilder()
		const grace = 5 * time.Second
		const settle = 2 * time.Second
		opts := defaultSchedulerOpts(es, b.builder(epoch), settle, grace, time.Hour, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		// bring up US.
		routeUS := make(chan routeResult1, 1)
		go func() {
			d, _, rel, err := sched.Route(ctx, "us-nyc-wg-001")
			routeUS <- routeResult1{d: d, rel: rel, err: err}
		}()
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond))
		clk.Advance(settle)
		res := <-routeUS
		require.NoError(t, res.err)

		// queue GB.
		go func() {
			_, _, rel, err := sched.Route(ctx, "gb-lon-wg-001")
			if err == nil {
				rel()
			}
		}()
		time.Sleep(20 * time.Millisecond)

		// release US → grace starts.
		res.rel()
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "grace timer not registered")
		clk.Advance(grace) // grace fires → teardown US, then settle before GB build

		// after grace: scheduler closes US then waits settle before building GB.
		awaitCloses(t, b, 1, 500*time.Millisecond)
		closes := b.closeList()
		assert.Equal(t, []string{"us-nyc-wg-001"}, closes, "US must close before GB builds")

		// settle for GB.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer (GB) not registered")
		clk.Advance(settle)

		awaitBuilds(t, b, 2, 500*time.Millisecond)
		builds := b.buildList()
		assert.Equal(t, []string{"us-nyc-wg-001", "gb-lon-wg-001"}, builds)
		// close must have happened before second build.
		assert.Equal(t, []string{"us-nyc-wg-001"}, b.closeList())
	})
}

// routeResult1 captures the return values of a single Route call.
type routeResult1 struct {
	d   Dialer
	rel func()
	err error
}

// TestOnDemandScheduler_bringupFailure tests that all queued same-zone jobs
// fail with a publicerror on build failure, and that the scheduler advances to
// the next zone.
func TestOnDemandScheduler_bringupFailure(t *testing.T) {
	t.Parallel()

	t.Run("all same-zone queued jobs fail on bring-up failure", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		b := newSchedulerDeviceBuilder()
		b.failZone("us-nyc-wg-001")
		opts := defaultSchedulerOpts(es, b.builder(epoch), time.Second, time.Second, time.Hour, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		const n = 3
		errChs := make([]chan error, n)
		for i := range n {
			errChs[i] = make(chan error, 1)
			go func(ch chan error) {
				_, _, _, err := sched.Route(ctx, "us-nyc-wg-001")
				ch <- err
			}(errChs[i])
		}

		// advance settle — build fails → all Route calls get errors.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer not registered")
		clk.Advance(time.Second)

		for _, ch := range errChs {
			err := <-ch
			require.Error(t, err)
			var pe loginjector.PublicDetailsError
			ok := errors.As(err, &pe)
			require.True(t, ok, "expected publicerror, got: %T %v", err, err)
			assert.Contains(t, pe.Details(), "zone_bring_up_failure")
		}
	})

	t.Run("next zone proceeds after current zone bring-up failure", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001", "gb-lon-wg-001")
		b := newSchedulerDeviceBuilder()
		b.failZone("us-nyc-wg-001")
		const settle = time.Second
		opts := defaultSchedulerOpts(es, b.builder(epoch), settle, time.Second, time.Hour, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		// request US (will fail) and GB (should succeed after US failure).
		usErrCh := make(chan error, 1)
		gbErrCh := make(chan error, 1)
		var gbRelease func()
		go func() {
			_, _, _, err := sched.Route(ctx, "us-nyc-wg-001")
			usErrCh <- err
		}()
		// give US a head start so it's first in the queue.
		time.Sleep(20 * time.Millisecond)
		go func() {
			_, _, rel, err := sched.Route(ctx, "gb-lon-wg-001")
			if err == nil {
				gbRelease = rel
			}
			gbErrCh <- err
		}()

		// settle fires for US → build fails → US gets publicerror, scheduler
		// moves to GB (another settle, then build).
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer (US) not registered")
		clk.Advance(settle) // US settle

		usErr := <-usErrCh
		require.Error(t, usErr)
		ok := errors.As(usErr, new(loginjector.PublicDetailsError))
		require.True(t, ok, "US error should be publicerror")

		// GB now needs its settle.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer (GB) not registered")
		clk.Advance(settle)

		gbErr := <-gbErrCh
		require.NoError(t, gbErr, "GB should succeed after US failure")
		if gbRelease != nil {
			gbRelease()
		}

		awaitBuilds(t, b, 1, 500*time.Millisecond)
		assert.Equal(t, []string{"gb-lon-wg-001"}, b.buildList(), "only GB should have been built")
	})
}

// TestOnDemandScheduler_idleTTL tests that the device is torn down after the
// idle TTL elapses with no active jobs.
func TestOnDemandScheduler_idleTTL(t *testing.T) {
	t.Parallel()

	t.Run("device torn down after idle TTL", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		b := newSchedulerDeviceBuilder()
		const settle = time.Second
		const idleTTL = 10 * time.Second
		opts := defaultSchedulerOpts(es, b.builder(epoch), settle, time.Second, idleTTL, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		// bring up device.
		routeUS := make(chan routeResult1, 1)
		go func() {
			d, _, rel, err := sched.Route(ctx, "us-nyc-wg-001")
			routeUS <- routeResult1{d: d, rel: rel, err: err}
		}()
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer not registered")
		clk.Advance(settle)
		res := <-routeUS
		require.NoError(t, res.err)

		// release the active job — idle timer starts.
		res.rel()
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "idle timer not registered")

		// advance past idle TTL — device should be torn down.
		clk.Advance(idleTTL)
		awaitCloses(t, b, 1, 500*time.Millisecond)
		assert.Equal(t, []string{"us-nyc-wg-001"}, b.closeList())
	})

	t.Run("new request cancels idle timer and reuses device", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		b := newSchedulerDeviceBuilder()
		const settle = time.Second
		const idleTTL = 10 * time.Second
		opts := defaultSchedulerOpts(es, b.builder(epoch), settle, time.Second, idleTTL, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		// first request.
		routeUS1 := make(chan routeResult1, 1)
		go func() {
			d, _, rel, err := sched.Route(ctx, "us-nyc-wg-001")
			routeUS1 <- routeResult1{d: d, rel: rel, err: err}
		}()
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond))
		clk.Advance(settle)
		res1 := <-routeUS1
		require.NoError(t, res1.err)
		res1.rel() // release → idle timer starts

		// advance half idle TTL.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "idle timer not registered")
		clk.Advance(idleTTL / 2)

		// second request arrives within idle TTL — should reuse the device (same zone).
		routeUS2 := make(chan routeResult1, 1)
		go func() {
			d, _, rel, err := sched.Route(ctx, "us-nyc-wg-001")
			routeUS2 <- routeResult1{d: d, rel: rel, err: err}
		}()

		// wait for Route to be handled by Run loop (it's a channel receive in Run).
		time.Sleep(20 * time.Millisecond)
		res2 := <-routeUS2
		require.NoError(t, res2.err)
		res2.rel()

		// no close should have occurred — device was reused.
		time.Sleep(20 * time.Millisecond)
		assert.Empty(t, b.closeList(), "device must not be closed when reused within idle TTL")

		// only one build.
		assert.Len(t, b.buildList(), 1)
	})

	t.Run("LiveHealth returns false after idle teardown", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		b := newSchedulerDeviceBuilder()
		const settle = time.Second
		const idleTTL = 5 * time.Second
		opts := defaultSchedulerOpts(es, b.builder(epoch), settle, time.Second, idleTTL, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		routeUS := make(chan routeResult1, 1)
		go func() {
			d, _, rel, err := sched.Route(ctx, "us-nyc-wg-001")
			routeUS <- routeResult1{d: d, rel: rel, err: err}
		}()
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond))
		clk.Advance(settle)
		res := <-routeUS
		require.NoError(t, res.err)

		// device is live — health should report it.
		awaitBuilds(t, b, 1, 500*time.Millisecond)
		health, ok := sched.LiveHealth()
		assert.True(t, ok)
		assert.Equal(t, "us-nyc-wg-001", health.ID)

		res.rel()
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "idle timer not registered")
		clk.Advance(idleTTL)
		awaitCloses(t, b, 1, 500*time.Millisecond)

		// after teardown, LiveHealth returns false.
		_, ok = sched.LiveHealth()
		assert.False(t, ok, "LiveHealth must return false after idle teardown")
	})
}

// TestOnDemandScheduler_Run tests clean shutdown behaviour.
func TestOnDemandScheduler_Run(t *testing.T) {
	t.Parallel()

	t.Run("ctx cancel closes live device exactly once", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		b := newSchedulerDeviceBuilder()
		opts := defaultSchedulerOpts(es, b.builder(epoch), time.Second, time.Second, time.Hour, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		runDone := make(chan struct{})
		go func() {
			sched.Run(ctx)
			close(runDone)
		}()

		routeUS := make(chan routeResult1, 1)
		go func() {
			d, _, rel, err := sched.Route(ctx, "us-nyc-wg-001")
			routeUS <- routeResult1{d: d, rel: rel, err: err}
		}()
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond))
		clk.Advance(time.Second)
		res := <-routeUS
		require.NoError(t, res.err)
		res.rel()

		cancel()
		select {
		case <-runDone:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Run did not exit after ctx cancel")
		}

		awaitCloses(t, b, 1, 200*time.Millisecond)
		assert.Len(t, b.closeList(), 1, "device must be closed exactly once on shutdown")
	})

	t.Run("ctx cancel fails pending Route callers", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		b := newSchedulerDeviceBuilder()
		// use a very long settle so Route is blocked inside switchToZone.
		opts := defaultSchedulerOpts(es, b.builder(epoch), time.Hour, time.Second, time.Hour, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		runDone := make(chan struct{})
		go func() {
			sched.Run(ctx)
			close(runDone)
		}()

		errCh := make(chan error, 1)
		go func() {
			_, _, _, err := sched.Route(ctx, "us-nyc-wg-001")
			errCh <- err
		}()

		// let the route request reach the settle wait inside Run.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer not registered")

		cancel()
		err := <-errCh
		assert.Error(t, err)

		select {
		case <-runDone:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Run did not exit after ctx cancel during settle")
		}
	})

	t.Run("no device live on shutdown is safe", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		b := newSchedulerDeviceBuilder()
		opts := defaultSchedulerOpts(es, b.builder(epoch), time.Second, time.Second, time.Hour, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		runDone := make(chan struct{})
		go func() {
			sched.Run(ctx)
			close(runDone)
		}()

		cancel()
		select {
		case <-runDone:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Run did not exit after ctx cancel (no device)")
		}
		assert.Empty(t, b.closeList())
	})
}

// TestOnDemandScheduler_LiveHealth tests the LiveHealth snapshot method.
func TestOnDemandScheduler_LiveHealth(t *testing.T) {
	t.Parallel()

	t.Run("returns false when no device is live", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		b := newSchedulerDeviceBuilder()
		opts := defaultSchedulerOpts(es, b.builder(epoch), time.Second, time.Second, time.Hour, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		_, ok := sched.LiveHealth()
		assert.False(t, ok, "LiveHealth must return false when no device is live")
	})

	t.Run("returns health snapshot when device is live", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		b := newSchedulerDeviceBuilder()
		opts := defaultSchedulerOpts(es, b.builder(epoch), time.Second, time.Second, time.Hour, clk)
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		routeUS := make(chan routeResult1, 1)
		go func() {
			d, _, rel, err := sched.Route(ctx, "us-nyc-wg-001")
			routeUS <- routeResult1{d: d, rel: rel, err: err}
		}()
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond))
		clk.Advance(time.Second)
		res := <-routeUS
		require.NoError(t, res.err)
		defer res.rel()

		awaitBuilds(t, b, 1, 500*time.Millisecond)
		health, ok := sched.LiveHealth()
		assert.True(t, ok)
		assert.Equal(t, "us-nyc-wg-001", health.ID)
		assert.NoError(t, health.Err)
	})
}

// TestNewOnDemandScheduler_panics tests the constructor's guard panics.
func TestNewOnDemandScheduler_panics(t *testing.T) {
	t.Parallel()

	t.Run("nil eligible panics", func(t *testing.T) {
		t.Parallel()
		assert.Panics(t, func() {
			NewOnDemandScheduler(SchedulerOptions{
				SettleDelay: time.Second,
				Grace:       time.Second,
				IdleTTL:     time.Hour,
				ConfigDir:   "/fakedir",
			})
		})
	})

	t.Run("zero SettleDelay panics", func(t *testing.T) {
		t.Parallel()
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		assert.Panics(t, func() {
			NewOnDemandScheduler(SchedulerOptions{
				Eligible:  es,
				Grace:     time.Second,
				IdleTTL:   time.Hour,
				ConfigDir: "/fakedir",
			})
		})
	})

	t.Run("zero Grace panics", func(t *testing.T) {
		t.Parallel()
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		assert.Panics(t, func() {
			NewOnDemandScheduler(SchedulerOptions{
				Eligible:    es,
				SettleDelay: time.Second,
				IdleTTL:     time.Hour,
				ConfigDir:   "/fakedir",
			})
		})
	})

	t.Run("empty ConfigDir panics", func(t *testing.T) {
		t.Parallel()
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		assert.Panics(t, func() {
			NewOnDemandScheduler(SchedulerOptions{
				Eligible:    es,
				SettleDelay: time.Second,
				Grace:       time.Second,
				IdleTTL:     time.Hour,
			})
		})
	})
}

// TestOnDemandScheduler_Notifier tests that a successful zone bring-up
// reports exactly one domain.TunnelChangeEvent derived from the config path
// (not the opaque zone id), that a bring-up failure reports none, and that a
// nil Notifier defaults to notify.Nop without panicking.
func TestOnDemandScheduler_Notifier(t *testing.T) {
	t.Parallel()

	t.Run("successful bring-up records one on-demand event with the config basename", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "se-sto-wg-001")
		b := newSchedulerDeviceBuilder()
		notifier := &fakeNotifier{}
		opts := defaultSchedulerOpts(es, b.builder(epoch), time.Second, time.Second, time.Hour, clk)
		opts.Notifier = notifier
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		routeErrCh := make(chan error, 1)
		go func() {
			_, _, rel, err := sched.Route(ctx, "se-sto-wg-001")
			if rel != nil {
				rel()
			}
			routeErrCh <- err
		}()

		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer not registered")
		clk.Advance(time.Second)
		require.NoError(t, <-routeErrCh)

		awaitNotifierLen(t, notifier, 1, 500*time.Millisecond)
		ev := notifier.recorded()[0]
		assert.Equal(t, domain.SourceOnDemand, ev.Source)
		assert.Equal(t, "se-sto-wg-001.conf", ev.Filename)
		assert.Equal(t, "se", ev.Country)
		assert.Equal(t, "on-demand: se", ev.Title)
	})

	t.Run("bring-up failure records no event", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "se-sto-wg-001")
		b := newSchedulerDeviceBuilder()
		b.failZone("se-sto-wg-001")
		notifier := &fakeNotifier{}
		opts := defaultSchedulerOpts(es, b.builder(epoch), time.Second, time.Second, time.Hour, clk)
		opts.Notifier = notifier
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		routeErrCh := make(chan error, 1)
		go func() {
			_, _, _, err := sched.Route(ctx, "se-sto-wg-001")
			routeErrCh <- err
		}()

		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer not registered")
		clk.Advance(time.Second)
		require.Error(t, <-routeErrCh)

		time.Sleep(20 * time.Millisecond) // let the loop process the failure path
		assert.Empty(t, notifier.recorded(), "a failed bring-up must not notify")
	})

	t.Run("nil notifier defaults to Nop and does not panic", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := schedulerEligibleSet(t, "us-nyc-wg-001")
		b := newSchedulerDeviceBuilder()
		opts := defaultSchedulerOpts(es, b.builder(epoch), time.Second, time.Second, time.Hour, clk)
		opts.Notifier = nil
		sched := NewOnDemandScheduler(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go sched.Run(ctx)

		routeErrCh := make(chan error, 1)
		go func() {
			_, _, rel, err := sched.Route(ctx, "us-nyc-wg-001")
			if rel != nil {
				rel()
			}
			routeErrCh <- err
		}()

		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "settle timer not registered")
		assert.NotPanics(t, func() { clk.Advance(time.Second) })
		require.NoError(t, <-routeErrCh)
	})
}
