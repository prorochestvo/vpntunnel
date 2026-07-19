package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/application/asyncjob"
	"vpntunnel/internal/egress"
	"vpntunnel/internal/publicerror"
)

// compile-time assertions: fakes must satisfy their target interfaces.
var _ Router = (*fakeRouter)(nil)
var _ RawForwarder = (*fakeRawForwarder)(nil)

// fakeRouter is a test double for Router.
type fakeRouter struct {
	// routeErr, when non-nil, is returned by Route.
	routeErr error
	// releaseCount records how many times release() was called.
	releaseCount int
	// dialer and resolver to return on success.
	dialer   egress.Dialer
	resolver egress.Resolver
}

func (f *fakeRouter) Route(_ context.Context, _ string) (egress.Dialer, egress.Resolver, func(), error) {
	if f.routeErr != nil {
		return nil, nil, nil, f.routeErr
	}
	release := func() { f.releaseCount++ }
	return f.dialer, f.resolver, release, nil
}

// fakeRawForwarder is a test double for RawForwarder.
type fakeRawForwarder struct {
	// resp is returned on success.
	resp asyncjob.UpstreamResponse
	// err, when non-nil, is returned instead of resp.
	err error
	// capturedReq is set to the request passed to ForwardRaw.
	capturedReq *http.Request
}

func (f *fakeRawForwarder) ForwardRaw(_ context.Context, req *http.Request, _ string, _ egress.Dialer, _ egress.Resolver) (asyncjob.UpstreamResponse, error) {
	f.capturedReq = req
	if f.err != nil {
		return asyncjob.UpstreamResponse{}, f.err
	}
	return f.resp, nil
}

// panicForwarder panics inside ForwardRaw to verify release() is deferred.
type panicForwarder struct{}

func (p *panicForwarder) ForwardRaw(_ context.Context, _ *http.Request, _ string, _ egress.Dialer, _ egress.Resolver) (asyncjob.UpstreamResponse, error) {
	panic("test panic from ForwardRaw")
}

var _ RawForwarder = (*panicForwarder)(nil)

func makeTestRequest(t testing.TB, zoneID string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/", nil)
	require.NoError(t, err)
	req.Header.Set(internalTunnelIDHeader, zoneID)
	req.Header.Set("X-Vpntunnel-Token", "secret-token")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestZoneRoutingForwarder_Forward(t *testing.T) {
	t.Parallel()

	t.Run("success: routes, strips service headers, calls ForwardRaw, releases", func(t *testing.T) {
		t.Parallel()

		router := &fakeRouter{}
		base := &fakeRawForwarder{
			resp: asyncjob.UpstreamResponse{StatusCode: 200, Body: []byte("ok")},
		}
		fwd := NewZoneRoutingForwarder(router, base)

		req := makeTestRequest(t, "mullvad-se-sto-wg-001")
		resp, err := fwd.Forward(t.Context(), req)

		require.NoError(t, err)
		assert.Equal(t, 200, resp.StatusCode)
		assert.Equal(t, []byte("ok"), resp.Body)
		// release must have been called exactly once.
		assert.Equal(t, 1, router.releaseCount)
		// service headers must be stripped before ForwardRaw sees the request.
		require.NotNil(t, base.capturedReq)
		assert.Empty(t, base.capturedReq.Header.Get(internalTunnelIDHeader), "internal tunnel-id header must be stripped")
		assert.Empty(t, base.capturedReq.Header.Get("X-Vpntunnel-Token"), "X-Vpntunnel-Token must be stripped")
		// non-service headers must survive.
		assert.Equal(t, "application/json", base.capturedReq.Header.Get("Content-Type"))
	})

	t.Run("Proxy-Retry-Tag is stripped before ForwardRaw", func(t *testing.T) {
		t.Parallel()

		router := &fakeRouter{}
		base := &fakeRawForwarder{resp: asyncjob.UpstreamResponse{StatusCode: 200}}
		fwd := NewZoneRoutingForwarder(router, base)

		req := makeTestRequest(t, "some-zone")
		req.Header.Set("Proxy-Retry-Tag", "retag-1234567890")

		_, err := fwd.Forward(t.Context(), req)
		require.NoError(t, err)
		require.NotNil(t, base.capturedReq)
		assert.Empty(t, base.capturedReq.Header.Get("Proxy-Retry-Tag"), "Proxy-Retry-Tag must be stripped before upstream")
	})

	t.Run("missing tunnel id returns 400 with X-Proxy-Error", func(t *testing.T) {
		t.Parallel()

		router := &fakeRouter{}
		base := &fakeRawForwarder{}
		fwd := NewZoneRoutingForwarder(router, base)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/", nil)
		require.NoError(t, err)
		// no internal tunnel-id header set
		resp, err := fwd.Forward(t.Context(), req)

		require.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "tunnel_required", resp.Header.Get("X-Proxy-Error"))
		// router must not have been called.
		assert.Equal(t, 0, router.releaseCount)
		assert.Nil(t, base.capturedReq)
	})

	t.Run("unknown zone returns 400 with X-Proxy-Error unknown_zone", func(t *testing.T) {
		t.Parallel()

		router := &fakeRouter{
			routeErr: publicerror.New("unknown_zone: bad-zone is not in the eligible set"),
		}
		base := &fakeRawForwarder{}
		fwd := NewZoneRoutingForwarder(router, base)

		resp, err := fwd.Forward(t.Context(), makeTestRequest(t, "bad-zone"))

		require.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "unknown_zone", resp.Header.Get("X-Proxy-Error"))
		// base forwarder must not have been called.
		assert.Nil(t, base.capturedReq)
	})

	t.Run("zone bring-up failure returns 502 with X-Proxy-Error", func(t *testing.T) {
		t.Parallel()

		router := &fakeRouter{
			routeErr: publicerror.New("zone_bring_up_failure: some-zone device could not be started"),
		}
		base := &fakeRawForwarder{}
		fwd := NewZoneRoutingForwarder(router, base)

		resp, err := fwd.Forward(t.Context(), makeTestRequest(t, "some-zone"))

		require.NoError(t, err)
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
		assert.Equal(t, "zone_bring_up_failure", resp.Header.Get("X-Proxy-Error"))
		assert.Nil(t, base.capturedReq)
	})

	t.Run("non-publicerror from Route returns 502", func(t *testing.T) {
		t.Parallel()

		router := &fakeRouter{
			routeErr: errors.New("scheduler internal error"),
		}
		base := &fakeRawForwarder{}
		fwd := NewZoneRoutingForwarder(router, base)

		resp, err := fwd.Forward(t.Context(), makeTestRequest(t, "any-zone"))

		require.NoError(t, err)
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
		assert.NotEmpty(t, resp.Header.Get("X-Proxy-Error"))
		assert.Nil(t, base.capturedReq)
	})

	t.Run("ForwardRaw error is propagated", func(t *testing.T) {
		t.Parallel()

		baseErr := errors.New("upstream dial failed")
		router := &fakeRouter{}
		base := &fakeRawForwarder{err: baseErr}
		fwd := NewZoneRoutingForwarder(router, base)

		_, err := fwd.Forward(t.Context(), makeTestRequest(t, "some-zone"))

		require.Error(t, err)
		assert.ErrorIs(t, err, baseErr)
		// release must still have been called.
		assert.Equal(t, 1, router.releaseCount)
	})

	t.Run("release is called even when ForwardRaw panics", func(t *testing.T) {
		t.Parallel()

		router := &fakeRouter{}
		base := &panicForwarder{}
		fwd := NewZoneRoutingForwarder(router, base)

		assert.Panics(t, func() {
			_, _ = fwd.Forward(t.Context(), makeTestRequest(t, "some-zone"))
		})
		// the defer must have run before the panic propagated.
		assert.Equal(t, 1, router.releaseCount)
	})

	t.Run("X-Proxy-Error is persisted in stored response body (JSON)", func(t *testing.T) {
		t.Parallel()

		router := &fakeRouter{
			routeErr: publicerror.New("unknown_zone: missing is not in the eligible set"),
		}
		base := &fakeRawForwarder{}
		fwd := NewZoneRoutingForwarder(router, base)

		resp, err := fwd.Forward(t.Context(), makeTestRequest(t, "missing"))

		require.NoError(t, err)
		// the response is stored in bbolt and replayed verbatim; verify that the
		// X-Proxy-Error header is present in the stored Header map (not just a
		// synchronous response path).
		require.NotNil(t, resp.Header)
		assert.Equal(t, "unknown_zone", resp.Header.Get("X-Proxy-Error"))
		assert.Contains(t, string(resp.Body), "unknown_zone")
	})
}
