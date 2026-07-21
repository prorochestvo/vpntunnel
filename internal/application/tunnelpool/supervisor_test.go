package tunnelpool

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/egress"
	"vpntunnel/internal/infrastructure/notify"
)

// compile-time checks: the fakes satisfy the required interfaces.
var _ egress.DialerCloser = &stubDevice{}
var _ egress.HealthReporter = &stubDevice{}
var _ Clock = &fakeClock{}
var _ notify.Notifier = (*fakeNotifier)(nil)
var _ egress.DialerCloser = (*raceCheckDevice)(nil)
var _ egress.HealthReporter = (*raceCheckDevice)(nil)

// stubDevice is a controllable DialerCloser + HealthReporter used in supervisor
// tests. It records Close calls and exposes a settable LastHandshake.
type stubDevice struct {
	id string

	mu        sync.Mutex
	closedN   int
	handshake time.Time
	hsErr     error
}

func (s *stubDevice) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return nil, nil
}

func (s *stubDevice) Close() error {
	s.mu.Lock()
	s.closedN++
	s.mu.Unlock()
	return nil
}

func (s *stubDevice) LastHandshake() (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handshake, s.hsErr
}

func (s *stubDevice) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closedN
}

func (s *stubDevice) setHandshake(t time.Time) {
	s.mu.Lock()
	s.handshake = t
	s.mu.Unlock()
}

func (s *stubDevice) setHSErr(err error) {
	s.mu.Lock()
	s.hsErr = err
	s.mu.Unlock()
}

// orderedDevice wraps a *stubDevice and calls onClose before delegating Close.
// Used to record the ordering of teardown vs. new-build events.
type orderedDevice struct {
	*stubDevice
	onClose func()
}

func (o *orderedDevice) Close() error {
	if o.onClose != nil {
		o.onClose()
	}
	return o.stubDevice.Close()
}

// raceCheckDevice wraps a *stubDevice and records, into the shared violated
// flag, whether DialContext was ever called after Close — i.e. whether a
// caller ever reached the torn-down old device during a rotation instead of
// getting the supervisor's "temporarily unavailable" publicerror. Used by the
// concurrent-dials-during-rotation stress test.
type raceCheckDevice struct {
	*stubDevice
	closed   atomic.Bool
	violated *atomic.Bool
}

func (d *raceCheckDevice) Close() error {
	d.closed.Store(true)
	return d.stubDevice.Close()
}

func (d *raceCheckDevice) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d.closed.Load() {
		d.violated.Store(true)
	}
	return d.stubDevice.DialContext(ctx, network, address)
}

// fakeClock drives time without real sleeps. Each call to After enqueues a
// pending timer; Advance fires all timers whose deadline has elapsed in the
// fake clock's "now".
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	deadline time.Time
	ch       chan time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	deadline := c.now.Add(d)
	t := &fakeTimer{deadline: deadline, ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	// fire immediately when d <= 0.
	if !c.now.Before(deadline) {
		t.ch <- c.now
	}
	c.mu.Unlock()
	return t.ch
}

// AwaitTimers blocks until at least n timers are pending in the clock, or
// the real-time deadline elapses. Used in tests to wait until the goroutine
// has called After() and is blocked in a select before advancing the clock.
func (c *fakeClock) AwaitTimers(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		got := len(c.timers)
		c.mu.Unlock()
		if got >= n {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Advance moves the fake clock forward by d and fires all elapsed timers.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	remaining := c.timers[:0]
	for _, t := range c.timers {
		if !c.now.Before(t.deadline) {
			t.ch <- c.now
		} else {
			remaining = append(remaining, t)
		}
	}
	c.timers = remaining
}

// fakeNotifier is a notify.Notifier test double that records every event it
// receives, guarded by a mutex so concurrent Notify calls (supervisor +
// scheduler tests share this type) are race-safe.
type fakeNotifier struct {
	mu     sync.Mutex
	events []notify.Event
}

func (f *fakeNotifier) Notify(_ context.Context, ev notify.Event) {
	f.mu.Lock()
	f.events = append(f.events, ev)
	f.mu.Unlock()
}

func (f *fakeNotifier) recorded() []notify.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]notify.Event, len(f.events))
	copy(out, f.events)
	return out
}

// awaitNotifierLen blocks until len(f.recorded()) >= n or the real-time
// deadline elapses.
func awaitNotifierLen(t *testing.T, f *fakeNotifier, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if len(f.recorded()) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d notifier event(s); got %d", n, len(f.recorded()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stubDeviceBuilderFn returns a DeviceBuilderFn that appends a new *stubDevice
// to *devices (guarded by mu). When *buildErr is non-nil the fn returns that
// error without creating a device. The device's initial handshake is hs.
func stubDeviceBuilderFn(mu *sync.Mutex, devices *[]*stubDevice, buildErr *error, hs time.Time) DeviceBuilderFn {
	return func(_ context.Context, _, _ string, _ *slog.Logger) (egress.DialerCloser, error) {
		if *buildErr != nil {
			return nil, *buildErr
		}
		d := &stubDevice{id: "stub", handshake: hs}
		mu.Lock()
		*devices = append(*devices, d)
		mu.Unlock()
		return d, nil
	}
}

// counterDeviceBuilderFn wraps a no-arg factory into a DeviceBuilderFn,
// ignoring config path, configDir, and logger. Useful when build behaviour is
// driven by a call counter.
func counterDeviceBuilderFn(f func() (egress.DialerCloser, error)) DeviceBuilderFn {
	return func(_ context.Context, _, _ string, _ *slog.Logger) (egress.DialerCloser, error) {
		return f()
	}
}

// supervisorEligibleSet returns a single-entry EligibleSet rooted at configDir.
func supervisorEligibleSet(t *testing.T, configDir, basename string) *EligibleSet {
	t.Helper()
	es, err := NewEligibleSet([]string{configDir + "/" + basename + ".conf"}, "/", nil)
	require.NoError(t, err)
	return es
}

// awaitDeviceLen blocks until len(*devices) >= n or the real-time deadline elapses.
func awaitDeviceLen(t *testing.T, mu *sync.Mutex, devices *[]*stubDevice, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		mu.Lock()
		got := len(*devices)
		mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			mu.Lock()
			got = len(*devices)
			mu.Unlock()
			t.Fatalf("timed out waiting for %d device(s); got %d", n, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitCloseCount blocks until d.closeCount() >= n or the real-time deadline elapses.
func awaitCloseCount(t *testing.T, d *stubDevice, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if d.closeCount() >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for closeCount >= %d; got %d", n, d.closeCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitAtomicCount blocks until the atomic counter reaches >= n or the real-time
// deadline elapses.
func awaitAtomicCount(t *testing.T, counter *int32, n int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if atomic.LoadInt32(counter) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for counter >= %d; got %d", n, atomic.LoadInt32(counter))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitEventLen blocks until len(*events) >= n or the real-time deadline elapses.
func awaitEventLen(t *testing.T, mu *sync.Mutex, events *[]string, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		mu.Lock()
		got := len(*events)
		mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			mu.Lock()
			got = len(*events)
			mu.Unlock()
			t.Fatalf("timed out waiting for %d event(s); got %d", n, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// testRotateSettle is a small RotateSettle value used across supervisor (and
// integration) tests that do not exercise rotation directly; it only needs to
// satisfy the required > 0 validation in NewStreamingSupervisor. Tests that
// DO exercise RotateIfIdle drive it explicitly via the fake clock, so the
// exact value here is otherwise immaterial.
const testRotateSettle = 50 * time.Millisecond

// defaultTestOpts builds SupervisorOptions with a 24h poll interval so no poll
// ever fires accidentally; tests control timing via clk.Advance.
func defaultTestOpts(
	es *EligibleSet,
	fn DeviceBuilderFn,
	maxAge, reconnMin, reconnMax time.Duration,
	clk *fakeClock,
) SupervisorOptions {
	return SupervisorOptions{
		Eligible:        es,
		DeviceBuilder:   fn,
		HandshakeMaxAge: maxAge,
		ReconnectMin:    reconnMin,
		ReconnectMax:    reconnMax,
		PollInterval:    24 * time.Hour,
		RotateSettle:    testRotateSettle,
		Clock:           clk,
		ConfigDir:       "/fakedir",
		OpLog:           slog.New(slog.DiscardHandler),
	}
}

// TestStreamingSupervisor_Start tests that Start builds exactly one device and
// exposes a working Dialer.
func TestStreamingSupervisor_Start(t *testing.T) {
	t.Parallel()

	t.Run("builds exactly one device on start", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		var mu sync.Mutex
		var devices []*stubDevice
		var buildErr error
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch.Add(-time.Second))

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))

		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)
		mu.Lock()
		n := len(devices)
		mu.Unlock()
		assert.Equal(t, 1, n, "expected exactly one device built on Start")
	})

	t.Run("DialContext delegates to live device", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		var mu sync.Mutex
		var devices []*stubDevice
		var buildErr error
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch.Add(-time.Second))

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		conn, err := sup.DialContext(ctx, "tcp", "example.com:443")
		assert.NoError(t, err)
		assert.Nil(t, conn) // stubDevice returns nil conn, nil err
	})

	t.Run("first build failure does not block Start", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		buildErr := errors.New("network unreachable")
		var mu sync.Mutex
		var devices []*stubDevice
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch)

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		done := make(chan error, 1)
		go func() { done <- sup.Start(ctx) }()

		select {
		case err := <-done:
			assert.NoError(t, err, "Start must return nil even when the first build fails")
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Start blocked for too long on first-build failure")
		}
	})

	t.Run("DialContext returns unavailable error when no device is live", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		buildErr := errors.New("device init failed")
		var mu sync.Mutex
		var devices []*stubDevice
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch)

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		time.Sleep(20 * time.Millisecond)

		_, err := sup.DialContext(ctx, "tcp", "example.com:443")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "temporarily unavailable")
	})
}

// TestStreamingSupervisor_reconnect tests that a stale or errored handshake
// causes the old device to be closed BEFORE a new device is built.
func TestStreamingSupervisor_reconnect(t *testing.T) {
	t.Parallel()

	t.Run("stale handshake: old device closed before new build", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		const maxAge = 180 * time.Second
		const reconnMin = 10 * time.Minute

		// events records "build" and "close" in order so we can assert close < build.
		var eventMu sync.Mutex
		var events []string
		var devices []*stubDevice

		fn := DeviceBuilderFn(func(_ context.Context, _, _ string, _ *slog.Logger) (egress.DialerCloser, error) {
			base := &stubDevice{handshake: epoch.Add(-(maxAge + time.Second))}
			d := &orderedDevice{
				stubDevice: base,
				onClose: func() {
					eventMu.Lock()
					events = append(events, "close")
					eventMu.Unlock()
				},
			}
			eventMu.Lock()
			events = append(events, "build")
			devices = append(devices, base)
			eventMu.Unlock()
			return d, nil
		})

		sup := NewStreamingSupervisor(SupervisorOptions{
			Eligible:        es,
			DeviceBuilder:   fn,
			HandshakeMaxAge: maxAge,
			ReconnectMin:    reconnMin,
			ReconnectMax:    3 * time.Hour,
			PollInterval:    5 * time.Minute,
			RotateSettle:    testRotateSettle,
			Clock:           clk,
			ConfigDir:       "/fakedir",
			OpLog:           slog.New(slog.DiscardHandler),
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))

		// wait for first build event.
		awaitEventLen(t, &eventMu, &events, 1, 500*time.Millisecond)
		// wait for the goroutine to register the poll timer before advancing.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "poll timer not registered in time")

		// trigger poll → stale → teardown (records "close" event).
		clk.Advance(5*time.Minute + time.Second)
		// wait for "close" event, then wait for backoff timer to be registered
		// before advancing to fire it.
		awaitEventLen(t, &eventMu, &events, 2, 2*time.Second) // "build" + "close"
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "backoff timer not registered in time")
		clk.Advance(reconnMin)

		// expect: "build", "close", "build" (3 events minimum).
		awaitEventLen(t, &eventMu, &events, 3, 2*time.Second)

		eventMu.Lock()
		snap := make([]string, len(events))
		copy(snap, events)
		eventMu.Unlock()

		// first close must come before second build.
		firstClose, secondBuild := -1, -1
		seen := 0
		for i, e := range snap {
			switch e {
			case "close":
				if firstClose < 0 {
					firstClose = i
				}
			case "build":
				seen++
				if seen == 2 {
					secondBuild = i
				}
			}
		}
		require.Greater(t, firstClose, -1, "must have at least one close event")
		require.Greater(t, secondBuild, -1, "must have a second build event")
		assert.Less(t, firstClose, secondBuild, "old device must be closed before new device is built")
	})

	t.Run("health reporter error triggers teardown then rebuild", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		const maxAge = 180 * time.Second
		const reconnMin = 10 * time.Minute

		var mu sync.Mutex
		var devices []*stubDevice
		var buildErr error
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch.Add(-time.Second))

		sup := NewStreamingSupervisor(SupervisorOptions{
			Eligible:        es,
			DeviceBuilder:   fn,
			HandshakeMaxAge: maxAge,
			ReconnectMin:    reconnMin,
			ReconnectMax:    3 * time.Hour,
			PollInterval:    5 * time.Minute,
			RotateSettle:    testRotateSettle,
			Clock:           clk,
			ConfigDir:       "/fakedir",
			OpLog:           slog.New(slog.DiscardHandler),
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		mu.Lock()
		d0 := devices[0]
		mu.Unlock()
		d0.setHSErr(errors.New("uapi broken"))

		// wait for poll timer to be registered, then fire it.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "poll timer not registered")
		clk.Advance(5*time.Minute + time.Second) // poll fires → health_query_error
		// wait for teardown (d0 closed), then wait for backoff timer registration.
		awaitCloseCount(t, d0, 1, 2*time.Second)
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "backoff timer not registered")
		clk.Advance(reconnMin) // backoff elapses → rebuild

		awaitDeviceLen(t, &mu, &devices, 2, 2*time.Second)
		assert.Equal(t, 1, d0.closeCount(), "old device closed exactly once on health error")
	})

	t.Run("no handshake yet is treated as unhealthy", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		const maxAge = 180 * time.Second
		const reconnMin = 10 * time.Minute

		var mu sync.Mutex
		var devices []*stubDevice
		var buildErr error
		// zero time.Time = no handshake yet.
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, time.Time{})

		sup := NewStreamingSupervisor(SupervisorOptions{
			Eligible:        es,
			DeviceBuilder:   fn,
			HandshakeMaxAge: maxAge,
			ReconnectMin:    reconnMin,
			ReconnectMax:    3 * time.Hour,
			PollInterval:    5 * time.Minute,
			RotateSettle:    testRotateSettle,
			Clock:           clk,
			ConfigDir:       "/fakedir",
			OpLog:           slog.New(slog.DiscardHandler),
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		mu.Lock()
		d0 := devices[0]
		mu.Unlock()

		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "poll timer not registered")
		clk.Advance(5*time.Minute + time.Second) // poll fires → no_handshake
		awaitCloseCount(t, d0, 1, 2*time.Second) // wait for teardown
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "backoff timer not registered")
		clk.Advance(reconnMin) // backoff elapses → rebuild

		awaitDeviceLen(t, &mu, &devices, 2, 2*time.Second)
		assert.Equal(t, 1, d0.closeCount())
	})
}

// TestStreamingSupervisor_backoff tests backoff growth, cap, and reset.
func TestStreamingSupervisor_backoff(t *testing.T) {
	t.Parallel()

	t.Run("backoff grows min to max then caps", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		const maxAge = 3 * time.Minute
		const reconnMin = 10 * time.Minute
		const reconnMax = 3 * time.Hour

		var buildCount int32
		var mu sync.Mutex
		var devices []*stubDevice

		fn := counterDeviceBuilderFn(func() (egress.DialerCloser, error) {
			n := atomic.AddInt32(&buildCount, 1)
			if n == 1 {
				// first build: succeed with stale handshake so the poll tears it down.
				d := &stubDevice{handshake: epoch.Add(-(maxAge + time.Second))}
				mu.Lock()
				devices = append(devices, d)
				mu.Unlock()
				return d, nil
			}
			return nil, errors.New("still down")
		})

		sup := NewStreamingSupervisor(SupervisorOptions{
			Eligible:        es,
			DeviceBuilder:   fn,
			HandshakeMaxAge: maxAge,
			ReconnectMin:    reconnMin,
			ReconnectMax:    reconnMax,
			PollInterval:    5 * time.Minute,
			RotateSettle:    testRotateSettle,
			Clock:           clk,
			ConfigDir:       "/fakedir",
			OpLog:           slog.New(slog.DiscardHandler),
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		// poll fires → stale → teardown → enter reconnMin backoff.
		mu.Lock()
		d0 := devices[0]
		mu.Unlock()

		// wait for poll timer to be registered, fire it, then wait for teardown.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "poll timer not registered")
		clk.Advance(5 * time.Minute)
		awaitCloseCount(t, d0, 1, 2*time.Second) // teardown complete

		// drive through several doublings. For each attempt: wait for the timer to
		// be registered (goroutine is in select), then fire it.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "backoff timer 1 not registered")
		clk.Advance(reconnMin) // attempt 2 (10m fires, fails)
		awaitAtomicCount(t, &buildCount, 2, 2*time.Second)

		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "backoff timer 2 not registered")
		clk.Advance(2 * reconnMin) // attempt 3 (20m fires, fails)
		awaitAtomicCount(t, &buildCount, 3, 2*time.Second)

		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "backoff timer 3 not registered")
		clk.Advance(4 * reconnMin) // attempt 4 (40m fires, fails)
		awaitAtomicCount(t, &buildCount, 4, 2*time.Second)

		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "backoff timer 4 not registered")
		clk.Advance(8 * reconnMin) // attempt 5 (80m fires, fails)
		awaitAtomicCount(t, &buildCount, 5, 2*time.Second)

		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "backoff timer 5 not registered")
		clk.Advance(reconnMax) // attempt 6 (3h capped fires, fails)
		awaitAtomicCount(t, &buildCount, 6, 2*time.Second)

		got := int(atomic.LoadInt32(&buildCount))
		assert.GreaterOrEqual(t, got, 5, "expected at least 5 build attempts across backoff doublings")
	})

	t.Run("backoff resets to min after healthy reconnect", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		const maxAge = 3 * time.Minute
		const reconnMin = 10 * time.Minute
		const reconnMax = 3 * time.Hour

		var buildCount int32
		var mu sync.Mutex
		var devices []*stubDevice

		fn := counterDeviceBuilderFn(func() (egress.DialerCloser, error) {
			n := atomic.AddInt32(&buildCount, 1)
			var hs time.Time
			if n == 1 {
				// stale so the first poll tears it down.
				hs = epoch.Add(-(maxAge + time.Second))
			} else {
				// fresh handshake so the poll sees subsequent devices as healthy.
				hs = epoch.Add(-time.Second)
			}
			d := &stubDevice{handshake: hs}
			mu.Lock()
			devices = append(devices, d)
			mu.Unlock()
			return d, nil
		})

		sup := NewStreamingSupervisor(SupervisorOptions{
			Eligible:        es,
			DeviceBuilder:   fn,
			HandshakeMaxAge: maxAge,
			ReconnectMin:    reconnMin,
			ReconnectMax:    reconnMax,
			PollInterval:    5 * time.Minute,
			RotateSettle:    testRotateSettle,
			Clock:           clk,
			ConfigDir:       "/fakedir",
			OpLog:           slog.New(slog.DiscardHandler),
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		mu.Lock()
		d0 := devices[0]
		mu.Unlock()

		// first poll: stale → teardown → backoff at reconnMin.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "poll timer 1 not registered")
		clk.Advance(5 * time.Minute)
		awaitCloseCount(t, d0, 1, 2*time.Second)

		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "backoff timer 1 not registered")
		clk.Advance(reconnMin) // backoff elapses → build #2 (fresh hs)
		awaitDeviceLen(t, &mu, &devices, 2, 2*time.Second)

		// update device #2 handshake to be fresh relative to current fake now.
		mu.Lock()
		d1 := devices[1]
		mu.Unlock()
		d1.setHandshake(clk.Now().Add(-time.Second))

		// second poll: healthy → backoff resets to reconnMin.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "poll timer 2 not registered")
		clk.Advance(5 * time.Minute)
		// There's no observable side-effect of a healthy poll besides backoff reset.
		// Sleep briefly to let the goroutine process the poll and re-register the next timer.
		time.Sleep(20 * time.Millisecond)

		// make device #2 unhealthy to trigger a third teardown+reconnect.
		d1.setHandshake(time.Time{}) // zero → no_handshake → unhealthy

		// third poll: stale → teardown. If backoff truly reset, advancing only
		// reconnMin is enough for the new build. If it had grown to 20m, this
		// Advance wouldn't trigger build #3.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "poll timer 3 not registered")
		clk.Advance(5 * time.Minute) // poll fires
		awaitCloseCount(t, d1, 1, 2*time.Second)
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "backoff timer 2 not registered")
		clk.Advance(reconnMin) // reconnMin elapses → build #3
		awaitDeviceLen(t, &mu, &devices, 3, 2*time.Second)
	})
}

// TestStreamingSupervisor_DialContext tests DialContext delegation.
func TestStreamingSupervisor_DialContext(t *testing.T) {
	t.Parallel()

	t.Run("returns unavailable error mid-backoff", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		buildErr := errors.New("no device")
		var mu sync.Mutex
		var devices []*stubDevice
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch)

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		time.Sleep(20 * time.Millisecond)

		_, err := sup.DialContext(ctx, "tcp", "example.com:443")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "temporarily unavailable")
	})

	t.Run("delegates to live device and returns its result", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		var mu sync.Mutex
		var devices []*stubDevice
		var buildErr error
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch.Add(-time.Second))

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		conn, err := sup.DialContext(ctx, "tcp", "example.com:443")
		assert.NoError(t, err)
		assert.Nil(t, conn) // stubDevice always returns nil, nil
	})
}

// TestStreamingSupervisor_Stop tests clean shutdown behaviour.
func TestStreamingSupervisor_Stop(t *testing.T) {
	t.Parallel()

	t.Run("stop closes live device exactly once", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		var mu sync.Mutex
		var devices []*stubDevice
		var buildErr error
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch.Add(-time.Second))

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		// Stop() is synchronous: when it returns the goroutine has exited and the
		// device has been closed. No sleep needed.
		sup.Stop()

		mu.Lock()
		d := devices[0]
		mu.Unlock()
		assert.Equal(t, 1, d.closeCount(), "device must be closed exactly once on Stop")
	})

	t.Run("stop is idempotent — multiple calls close device once", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		var mu sync.Mutex
		var devices []*stubDevice
		var buildErr error
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch.Add(-time.Second))

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		// all three calls must be safe and not deadlock. The first call joins the
		// goroutine; subsequent calls receive from the already-closed loopDone.
		sup.Stop()
		sup.Stop()
		sup.Stop()

		mu.Lock()
		n := devices[0].closeCount()
		mu.Unlock()
		assert.Equal(t, 1, n, "device closed exactly once regardless of Stop call count")
	})

	t.Run("ctx cancel stops the loop and closes device", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		var mu sync.Mutex
		var devices []*stubDevice
		var buildErr error
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch.Add(-time.Second))

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		require.NoError(t, sup.Start(ctx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		// cancel the context to drive the ctx.Done() exit branch, then call Stop()
		// to join the goroutine deterministically. Stop is idempotent so double-close
		// is safe; the loopDone join is what makes this race-free.
		cancel()
		sup.Stop()

		mu.Lock()
		n := devices[0].closeCount()
		mu.Unlock()
		assert.Equal(t, 1, n, "ctx cancel must close the device exactly once")
	})

	t.Run("stop with no live device is safe", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		buildErr := errors.New("always fails")
		var mu sync.Mutex
		var devices []*stubDevice
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch)

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		time.Sleep(20 * time.Millisecond)

		assert.NotPanics(t, func() { sup.Stop() })
	})
}

// TestStreamingSupervisor_LiveHealth tests the LiveHealth snapshot method.
func TestStreamingSupervisor_LiveHealth(t *testing.T) {
	t.Parallel()

	t.Run("returns false when no device is live", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		buildErr := errors.New("always fails")
		var mu sync.Mutex
		var devices []*stubDevice
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch)

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		time.Sleep(20 * time.Millisecond)

		_, ok := sup.LiveHealth()
		assert.False(t, ok, "LiveHealth must return ok=false when no device is live")
	})

	t.Run("returns device health snapshot when live", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		hsTime := epoch.Add(-5 * time.Second)
		var mu sync.Mutex
		var devices []*stubDevice
		var buildErr error
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, hsTime)

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		health, ok := sup.LiveHealth()
		require.True(t, ok, "LiveHealth must return ok=true when device is live")
		assert.Equal(t, hsTime, health.LastHandshake)
		assert.NoError(t, health.Err)
		assert.NotEmpty(t, health.ID)
	})
}

// TestNewStreamingSupervisor_panics tests the RotateSettle validation added
// alongside RotateIfIdle. It mirrors the panic-guard test convention already
// used for OnDemandScheduler's required options.
func TestNewStreamingSupervisor_panics(t *testing.T) {
	t.Parallel()

	t.Run("zero RotateSettle panics", func(t *testing.T) {
		t.Parallel()
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")
		assert.Panics(t, func() {
			NewStreamingSupervisor(SupervisorOptions{
				Eligible:        es,
				HandshakeMaxAge: 3 * time.Minute,
				ReconnectMin:    10 * time.Minute,
				ReconnectMax:    3 * time.Hour,
				ConfigDir:       "/fakedir",
			})
		})
	})

	t.Run("negative RotateSettle panics", func(t *testing.T) {
		t.Parallel()
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")
		assert.Panics(t, func() {
			NewStreamingSupervisor(SupervisorOptions{
				Eligible:        es,
				HandshakeMaxAge: 3 * time.Minute,
				ReconnectMin:    10 * time.Minute,
				ReconnectMax:    3 * time.Hour,
				RotateSettle:    -time.Second,
				ConfigDir:       "/fakedir",
			})
		})
	})
}

// TestStreamingSupervisor_Notifier tests that Start and a forced reconnect
// each report exactly one notify.Event, and that a nil Notifier defaults to
// notify.Nop (no panic, no send).
func TestStreamingSupervisor_Notifier(t *testing.T) {
	t.Parallel()

	t.Run("start records a started event, reconnect records a switched event", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")
		fn := DeviceBuilderFn(func(_ context.Context, _, _ string, _ *slog.Logger) (egress.DialerCloser, error) {
			return &stubDevice{handshake: epoch.Add(-time.Second)}, nil
		})

		notifier := &fakeNotifier{}
		opts := defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk)
		opts.PollInterval = 5 * time.Minute
		opts.Notifier = notifier

		sup := NewStreamingSupervisor(opts)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))

		awaitNotifierLen(t, notifier, 1, 500*time.Millisecond)
		first := notifier.recorded()[0]
		assert.Equal(t, notify.SourceStreaming, first.Source)
		assert.Equal(t, "started", first.Title)
		assert.Equal(t, "se-sto-wg-001.conf", first.Filename)

		// force a reconnect: poll fires, sees a stale handshake (device's
		// handshake never advances relative to the fake clock), tears down,
		// then rebuilds after the reconnect backoff.
		require.True(t, clk.AwaitTimers(1, 500*time.Millisecond), "poll timer not registered")
		clk.Advance(5*time.Minute + time.Second)
		require.True(t, clk.AwaitTimers(1, 2*time.Second), "backoff timer not registered")
		clk.Advance(10 * time.Minute)

		awaitNotifierLen(t, notifier, 2, 2*time.Second)
		second := notifier.recorded()[1]
		assert.Equal(t, notify.SourceStreaming, second.Source)
		assert.Equal(t, "switched tunnel", second.Title)
		assert.Equal(t, "se-sto-wg-001.conf", second.Filename)
	})

	t.Run("nil notifier defaults to Nop and does not panic", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		var mu sync.Mutex
		var devices []*stubDevice
		var buildErr error
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch.Add(-time.Second))

		opts := defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk)
		opts.Notifier = nil
		sup := NewStreamingSupervisor(opts)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		assert.NotPanics(t, func() { require.NoError(t, sup.Start(ctx)) })
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)
	})
}

// rotateCallResult carries a RotateIfIdle return across a channel so
// assertions stay on the test's own goroutine (calling require/assert from a
// background goroutine is unsafe).
type rotateCallResult struct {
	res RotateResult
	err error
}

// TestStreamingSupervisor_RotateIfIdle drives every RotateIfIdle outcome —
// idle rotation, the busy gate (plain and force-overridden), build failure
// and self-heal, no-live-device, loopCtx cancel during settle, caller ctx
// cancel, and a concurrent-access stress run — against the loop goroutine's
// real select-driven state machine, using the fake clock to prove the settle
// gates the build and break-before-make ordering holds.
func TestStreamingSupervisor_RotateIfIdle(t *testing.T) {
	t.Parallel()

	t.Run("rotates when idle, gating the build until settle elapses", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		var mu sync.Mutex
		var devices []*stubDevice
		var buildErr error
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch.Add(-time.Second))

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		mu.Lock()
		oldDevice := devices[0]
		mu.Unlock()

		t0 := clk.Now()
		ch := make(chan rotateCallResult, 1)
		go func() {
			res, err := sup.RotateIfIdle(t.Context(), false, func() bool { return false })
			ch <- rotateCallResult{res, err}
		}()

		// break-before-make: the old device must already be closed by the
		// time the settle timer is registered.
		awaitCloseCount(t, oldDevice, 1, 2*time.Second)
		awaitTimerAt(t, clk, t0.Add(testRotateSettle), 2*time.Second)

		// the settle must gate the build: no second device yet.
		mu.Lock()
		n := len(devices)
		mu.Unlock()
		assert.Equal(t, 1, n, "build must not start before rotateSettle elapses")

		clk.Advance(testRotateSettle)
		awaitDeviceLen(t, &mu, &devices, 2, 2*time.Second)

		got := <-ch
		require.NoError(t, got.err)
		assert.Equal(t, RotateRotated, got.res.Outcome)
		assert.Equal(t, "se", got.res.Country)
		assert.Equal(t, 1, oldDevice.closeCount())

		mu.Lock()
		newDevice := devices[1]
		mu.Unlock()
		assert.NotSame(t, oldDevice, newDevice, "rotation must build a fresh device instance")
	})

	t.Run("skips when busy: gate keeps the live device untouched", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		var mu sync.Mutex
		var devices []*stubDevice
		var buildErr error
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch.Add(-time.Second))

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		mu.Lock()
		oldDevice := devices[0]
		mu.Unlock()

		res, err := sup.RotateIfIdle(t.Context(), false, func() bool { return true })
		require.NoError(t, err)
		assert.Equal(t, RotateSkippedActive, res.Outcome)
		assert.Zero(t, oldDevice.closeCount(), "busy rotation must not close the live device")

		mu.Lock()
		n := len(devices)
		mu.Unlock()
		assert.Equal(t, 1, n, "busy rotation must not build a new device")
	})

	t.Run("force ignores busy but still breaks-before-making and settles", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		var mu sync.Mutex
		var devices []*stubDevice
		var buildErr error
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch.Add(-time.Second))

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		mu.Lock()
		oldDevice := devices[0]
		mu.Unlock()

		t0 := clk.Now()
		ch := make(chan rotateCallResult, 1)
		go func() {
			res, err := sup.RotateIfIdle(t.Context(), true, func() bool { return true }) // busy=true, force=true
			ch <- rotateCallResult{res, err}
		}()

		awaitCloseCount(t, oldDevice, 1, 2*time.Second) // proves break-before-make despite busy()==true
		awaitTimerAt(t, clk, t0.Add(testRotateSettle), 2*time.Second)
		clk.Advance(testRotateSettle)

		got := <-ch
		require.NoError(t, got.err)
		assert.Equal(t, RotateRotated, got.res.Outcome)
		assert.Equal(t, "se", got.res.Country)
	})

	t.Run("build failure leaves device nil then the loop self-heals", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		var buildCount int32
		var mu sync.Mutex
		var devices []*stubDevice
		fn := DeviceBuilderFn(func(_ context.Context, _, _ string, _ *slog.Logger) (egress.DialerCloser, error) {
			n := atomic.AddInt32(&buildCount, 1)
			if n == 2 {
				// the rotation's post-settle rebuild fails; the first (Start)
				// and third (loop self-heal) builds succeed.
				return nil, errors.New("second build fails")
			}
			d := &stubDevice{handshake: epoch.Add(-time.Second)}
			mu.Lock()
			devices = append(devices, d)
			mu.Unlock()
			return d, nil
		})

		const reconnMin = 10 * time.Minute
		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, reconnMin, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		mu.Lock()
		oldDevice := devices[0]
		mu.Unlock()

		t0 := clk.Now()
		ch := make(chan rotateCallResult, 1)
		go func() {
			res, err := sup.RotateIfIdle(t.Context(), false, func() bool { return false })
			ch <- rotateCallResult{res, err}
		}()

		awaitCloseCount(t, oldDevice, 1, 2*time.Second)
		awaitTimerAt(t, clk, t0.Add(testRotateSettle), 2*time.Second)
		clk.Advance(testRotateSettle) // settle fires -> rebuild attempt #2 -> fails

		got := <-ch
		require.NoError(t, got.err)
		assert.Equal(t, RotateUnavailable, got.res.Outcome)

		// the loop re-enters its d==nil backoff branch at reconnMin.
		t1 := clk.Now()
		awaitTimerAt(t, clk, t1.Add(reconnMin), 2*time.Second)
		clk.Advance(reconnMin) // backoff elapses -> rebuild attempt #3 -> succeeds

		awaitDeviceLen(t, &mu, &devices, 2, 2*time.Second)
		_, ok := sup.LiveHealth()
		assert.True(t, ok, "loop must self-heal to a live device after the failed rotation build")
	})

	t.Run("unavailable when no device is live", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		buildErr := errors.New("always fails")
		var mu sync.Mutex
		var devices []*stubDevice
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch)

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, sup.Start(ctx)) // Start tolerates a first-build failure

		res, err := sup.RotateIfIdle(t.Context(), false, func() bool { return false })
		require.NoError(t, err)
		assert.Equal(t, RotateUnavailable, res.Outcome)
	})

	t.Run("loopCtx cancel during settle returns Unavailable with the old device already closed", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		var mu sync.Mutex
		var devices []*stubDevice
		var buildErr error
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch.Add(-time.Second))

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		// runCtx is the supervisor's own run/loop context — distinct from the
		// caller ctx passed to RotateIfIdle below — so cancelling it exercises
		// loopCtx.Done() inside handleRotate's settle wait, not the caller-side
		// ctx-cancel path (covered by its own subtest).
		runCtx, runCancel := context.WithCancel(context.Background())
		defer runCancel()
		require.NoError(t, sup.Start(runCtx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		mu.Lock()
		oldDevice := devices[0]
		mu.Unlock()

		t0 := clk.Now()
		ch := make(chan rotateCallResult, 1)
		go func() {
			res, err := sup.RotateIfIdle(t.Context(), false, func() bool { return false })
			ch <- rotateCallResult{res, err}
		}()

		awaitCloseCount(t, oldDevice, 1, 2*time.Second)
		awaitTimerAt(t, clk, t0.Add(testRotateSettle), 2*time.Second)

		runCancel() // shutdown mid-rotate, before the settle timer fires

		got := <-ch
		require.NoError(t, got.err, "loopCtx cancel during settle must not surface as a RotateIfIdle error")
		assert.Equal(t, RotateUnavailable, got.res.Outcome)
		assert.Equal(t, 1, oldDevice.closeCount(), "old device must already be closed before the shutdown abort")

		sup.Stop() // loop already exited via runCtx.Done(); Stop just joins it
	})

	t.Run("not started returns unavailable without blocking", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")
		var buildErr error
		var mu sync.Mutex
		var devices []*stubDevice
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch.Add(-time.Second))

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		// deliberately never Started: RotateIfIdle must short-circuit on the
		// !started guard and return immediately, rather than block forever
		// sending on a rotateCh that no loop goroutine will ever read.
		res, err := sup.RotateIfIdle(t.Context(), false, func() bool { return false })
		require.NoError(t, err)
		assert.Equal(t, RotateUnavailable, res.Outcome)
	})

	t.Run("caller ctx cancel during in-flight rotation returns ctx.Err()", func(t *testing.T) {
		t.Parallel()
		epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		clk := newFakeClock(epoch)
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")
		var buildErr error
		var mu sync.Mutex
		var devices []*stubDevice
		fn := stubDeviceBuilderFn(&mu, &devices, &buildErr, epoch.Add(-time.Second))

		sup := NewStreamingSupervisor(defaultTestOpts(es, fn, 3*time.Minute, 10*time.Minute, 3*time.Hour, clk))
		// runCtx keeps the loop alive; the caller ctx passed to RotateIfIdle is
		// distinct, so cancelling it exercises the SECOND select (the reply
		// wait) while a rotation is genuinely in flight — the plan's R4 path,
		// which the pre-Start degenerate case never reached.
		runCtx, runCancel := context.WithCancel(context.Background())
		defer runCancel()
		require.NoError(t, sup.Start(runCtx))
		awaitDeviceLen(t, &mu, &devices, 1, 500*time.Millisecond)

		mu.Lock()
		oldDevice := devices[0]
		mu.Unlock()

		callerCtx, callerCancel := context.WithCancel(context.Background())
		defer callerCancel()

		t0 := clk.Now()
		ch := make(chan rotateCallResult, 1)
		go func() {
			res, err := sup.RotateIfIdle(callerCtx, false, func() bool { return false })
			ch <- rotateCallResult{res, err}
		}()

		// wait until the rotation is genuinely in flight: the old device torn
		// down and the settle timer registered means the request was already
		// handed off and RotateIfIdle is now blocked in its reply-wait select.
		awaitCloseCount(t, oldDevice, 1, 2*time.Second)
		awaitTimerAt(t, clk, t0.Add(testRotateSettle), 2*time.Second)

		callerCancel() // caller disconnects mid-rotation

		got := <-ch
		assert.ErrorIs(t, got.err, context.Canceled)
		assert.Zero(t, got.res)

		// the loop still finishes the rotation on runCtx; advance the settle so
		// it builds+swaps rather than being left mid-settle at cleanup.
		clk.Advance(testRotateSettle)
		awaitDeviceLen(t, &mu, &devices, 2, 2*time.Second)

		runCancel()
		sup.Stop()
	})

	t.Run("concurrent dials during rotation never reach the torn-down device", func(t *testing.T) {
		t.Parallel()
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		var violated atomic.Bool
		fn := DeviceBuilderFn(func(_ context.Context, _, _ string, _ *slog.Logger) (egress.DialerCloser, error) {
			return &raceCheckDevice{stubDevice: &stubDevice{handshake: time.Now()}, violated: &violated}, nil
		})

		sup := NewStreamingSupervisor(SupervisorOptions{
			Eligible:        es,
			DeviceBuilder:   fn,
			HandshakeMaxAge: time.Hour,
			ReconnectMin:    time.Hour,
			ReconnectMax:    time.Hour,
			PollInterval:    time.Hour,
			RotateSettle:    2 * time.Millisecond, // real clock: keep the stress run fast
			ConfigDir:       "/fakedir",
			OpLog:           slog.New(slog.DiscardHandler),
		})
		ctx, cancel := context.WithCancel(t.Context())
		require.NoError(t, sup.Start(ctx))

		var activeSessions atomic.Int64
		var wg sync.WaitGroup
		stop := make(chan struct{})

		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					activeSessions.Add(1)
					_, _ = sup.DialContext(ctx, "tcp", "example.com:443")
					activeSessions.Add(-1)
					time.Sleep(time.Millisecond) // natural idle window between dials
				}
			}()
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = sup.RotateIfIdle(ctx, false, func() bool { return activeSessions.Load() > 0 })
			}
		}()

		time.Sleep(200 * time.Millisecond)
		close(stop)
		wg.Wait()
		cancel()
		sup.Stop()

		assert.False(t, violated.Load(),
			"a dial must never reach a device already Closed by a rotation")
	})

	t.Run("concurrent dials during forced rotation stay race-clean", func(t *testing.T) {
		t.Parallel()
		es := supervisorEligibleSet(t, "/fakedir", "se-sto-wg-001")

		fn := DeviceBuilderFn(func(_ context.Context, _, _ string, _ *slog.Logger) (egress.DialerCloser, error) {
			return &stubDevice{handshake: time.Now()}, nil
		})

		sup := NewStreamingSupervisor(SupervisorOptions{
			Eligible:        es,
			DeviceBuilder:   fn,
			HandshakeMaxAge: time.Hour,
			ReconnectMin:    time.Hour,
			ReconnectMax:    time.Hour,
			PollInterval:    time.Hour,
			RotateSettle:    2 * time.Millisecond,
			ConfigDir:       "/fakedir",
			OpLog:           slog.New(slog.DiscardHandler),
		})
		ctx, cancel := context.WithCancel(t.Context())
		require.NoError(t, sup.Start(ctx))

		var wg sync.WaitGroup
		stop := make(chan struct{})

		// force=true accepts dropped in-flight dials — this variant asserts
		// only that concurrent access stays race-clean, per the plan.
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					_, _ = sup.DialContext(ctx, "tcp", "example.com:443")
				}
			}()
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = sup.RotateIfIdle(ctx, true, func() bool { return false })
			}
		}()

		time.Sleep(200 * time.Millisecond)
		close(stop)
		wg.Wait()
		cancel()
		sup.Stop()
	})
}
