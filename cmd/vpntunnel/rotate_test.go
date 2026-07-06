package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/application"
	lazy "vpntunnel/internal/application/lazy"
	"vpntunnel/internal/gateway/router"
	"vpntunnel/internal/tunnel"
)

var _ tunnel.Dialer = (*blockingDialer)(nil)

// blockingDialer's DialContext blocks until release is closed or ctx is
// cancelled, then always fails. It lets a test hold a ProxyService session
// "in flight" long enough to observe ActiveSessions() > 0.
type blockingDialer struct {
	release chan struct{}
}

func (d *blockingDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	select {
	case <-d.release:
	case <-ctx.Done():
	}
	return nil, errors.New("blockingDialer: refused after release")
}

var _ tunnel.DialerCloser = (*closeSignalDevice)(nil)

// closeSignalDevice is a minimal DialerCloser whose Close runs an optional
// callback, letting a test observe the exact moment the supervisor tears the
// device down — the "break" in break-before-make.
type closeSignalDevice struct {
	onClose func()
}

func (*closeSignalDevice) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("closeSignalDevice: dial not supported")
}

func (d *closeSignalDevice) Close() error {
	if d.onClose != nil {
		d.onClose()
	}
	return nil
}

// newAdapterTestSupervisor builds a real *lazy.StreamingSupervisor wired with
// builder and a short rotateSettle, so rotateAdapter's outcome mapping can be
// exercised without a real WireGuard build. rotateAdapter closes over the
// concrete *lazy.StreamingSupervisor type (see rotate.go), so an
// interface-level fake is not an option here — smokeBuilder (main_test.go) is
// reused where a builder that always succeeds is enough.
func newAdapterTestSupervisor(t *testing.T, builder lazy.DeviceBuilderFn, rotateSettle time.Duration) *lazy.StreamingSupervisor {
	t.Helper()
	es, err := lazy.NewEligibleSet([]string{"/fake/se-sto-wg-001.conf"}, "/fake", nil)
	require.NoError(t, err)
	return lazy.NewStreamingSupervisor(lazy.SupervisorOptions{
		Eligible:        es,
		DeviceBuilder:   builder,
		HandshakeMaxAge: time.Hour,
		ReconnectMin:    time.Hour,
		ReconnectMax:    time.Hour,
		PollInterval:    time.Hour,
		RotateSettle:    rotateSettle,
		ConfigDir:       "/fake",
		OpLog:           slog.New(slog.DiscardHandler),
	})
}

// TestRotateAdapter_Rotate covers rotateAdapter's outcome mapping: each
// lazy.Rotate* maps to the matching router.Rotation*, the ctx-cancel error
// passes through unchanged, and ActiveSessions is populated only on the
// skipped branch.
func TestRotateAdapter_Rotate(t *testing.T) {
	t.Parallel()

	t.Run("maps RotateRotated to RotationRotated with country", func(t *testing.T) {
		t.Parallel()
		sup := newAdapterTestSupervisor(t, smokeBuilder, 5*time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		require.NoError(t, sup.Start(ctx))
		t.Cleanup(sup.Stop)

		svc := application.NewProxyService(application.ProxyServiceOptions{
			Dialer:      &blockingDialer{release: make(chan struct{})},
			DialTimeout: time.Second,
		})
		a := rotateAdapter{sup: sup, svc: svc}

		res, err := a.Rotate(t.Context(), false)
		require.NoError(t, err)
		assert.Equal(t, router.RotationRotated, res.Outcome)
		assert.Equal(t, "se", res.Country)
		assert.Zero(t, res.ActiveSessions)
	})

	t.Run("maps RotateSkippedActive with the live ActiveSessions count", func(t *testing.T) {
		t.Parallel()
		sup := newAdapterTestSupervisor(t, smokeBuilder, 5*time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		require.NoError(t, sup.Start(ctx))
		t.Cleanup(sup.Stop)

		release := make(chan struct{})
		svc := application.NewProxyService(application.ProxyServiceOptions{
			Dialer:      &blockingDialer{release: release},
			DialTimeout: 5 * time.Second,
		})
		a := rotateAdapter{sup: sup, svc: svc}

		// hold one HTTP forward mid-flight so ActiveSessions() > 0.
		handlerDone := make(chan struct{})
		go func() {
			defer close(handlerDone)
			req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
			rr := httptest.NewRecorder()
			svc.HandleHTTP(rr, req)
		}()
		require.Eventually(t, func() bool { return svc.ActiveSessions() == 1 }, time.Second, time.Millisecond)

		res, err := a.Rotate(t.Context(), false)
		require.NoError(t, err)
		assert.Equal(t, router.RotationSkippedActive, res.Outcome)
		assert.Equal(t, int64(1), res.ActiveSessions)
		assert.Empty(t, res.Country)

		close(release)
		<-handlerDone
	})

	t.Run("maps RotateUnavailable when no device is live", func(t *testing.T) {
		t.Parallel()
		alwaysFail := func(context.Context, string, string, *slog.Logger) (tunnel.DialerCloser, error) {
			return nil, errors.New("build always fails")
		}
		sup := newAdapterTestSupervisor(t, alwaysFail, 5*time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		require.NoError(t, sup.Start(ctx)) // Start tolerates a first-build failure
		t.Cleanup(sup.Stop)

		svc := application.NewProxyService(application.ProxyServiceOptions{
			Dialer:      &blockingDialer{release: make(chan struct{})},
			DialTimeout: time.Second,
		})
		a := rotateAdapter{sup: sup, svc: svc}

		res, err := a.Rotate(t.Context(), false)
		require.NoError(t, err)
		assert.Equal(t, router.RotationUnavailable, res.Outcome)
		assert.Empty(t, res.Country)
		assert.Zero(t, res.ActiveSessions)
	})

	t.Run("propagates an in-flight caller ctx-cancel as a plain error", func(t *testing.T) {
		t.Parallel()
		// a builder whose device signals its own Close lets the test wait until
		// the rotation has torn the old device down and entered the settle —
		// deterministically in flight — before cancelling the caller ctx, so
		// this exercises RotateIfIdle's reply-wait select (the real R4 path),
		// not the degenerate pre-Start case.
		torn := make(chan struct{})
		var once sync.Once
		builder := func(context.Context, string, string, *slog.Logger) (tunnel.DialerCloser, error) {
			return &closeSignalDevice{onClose: func() { once.Do(func() { close(torn) }) }}, nil
		}
		sup := newAdapterTestSupervisor(t, builder, 50*time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		require.NoError(t, sup.Start(ctx))
		t.Cleanup(sup.Stop)

		svc := application.NewProxyService(application.ProxyServiceOptions{
			Dialer:      &blockingDialer{release: make(chan struct{})},
			DialTimeout: time.Second,
		})
		a := rotateAdapter{sup: sup, svc: svc} // svc idle → busy()==false → rotation proceeds

		callerCtx, callerCancel := context.WithCancel(context.Background())
		defer callerCancel()
		ch := make(chan error, 1)
		go func() {
			_, err := a.Rotate(callerCtx, false)
			ch <- err
		}()

		<-torn         // teardown done: the rotation is in the settle, RotateIfIdle blocked on the reply
		callerCancel() // caller disconnects mid-rotation

		assert.ErrorIs(t, <-ch, context.Canceled)
		cancel() // abort the in-flight settle so the cleanup's Stop joins promptly
	})
}
