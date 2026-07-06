package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"vpntunnel/internal/application/asyncjob"
	"vpntunnel/internal/application/lazy"
	"vpntunnel/internal/domain"
	"vpntunnel/internal/publicerror"
)

// NewZoneRoutingForwarder returns a ZoneRoutingForwarder that reads the tunnel
// id from the internal tunnel-id header (internalTunnelIDHeader), routes it
// through router to obtain a live dialer and resolver, and delegates to
// base.ForwardRaw. It satisfies asyncjob.Forwarder and is intended to replace
// the single-activeID closure in main.go (Task 8 wiring).
//
// Token-leak safety: serveAsync clones the inbound *http.Request, which copies
// all headers (including the internal tunnel-id header and X-Vpntunnel-Token).
// ForwardRaw passes the request directly to http.Client.Do without calling
// copyProxyHeaders, so those service headers would reach the upstream.
// ZoneRoutingForwarder strips all serviceHeaders from the request before
// calling ForwardRaw.
func NewZoneRoutingForwarder(router Router, base RawForwarder) *ZoneRoutingForwarder {
	return &ZoneRoutingForwarder{router: router, base: base}
}

// Router is the minimal zone-routing contract. *lazy.OnDemandScheduler
// satisfies it. The local interface keeps the structural dependency narrow
// (the scheduler is injected, not constructed here); the lazy import in this
// file is scoped to the PrefixUnknownZone constant, which is the single
// source of truth shared with the scheduler's Route implementation.
type Router interface {
	// Route acquires a dialer and resolver for the given zone. The returned
	// release function MUST be deferred by the caller; it signals the scheduler
	// that the job is done so grace and idle timers remain accurate. Returns a
	// *publicerror.Error for unknown zones (message prefix "unknown_zone:") or
	// device bring-up failures (message prefix "zone_bring_up_failure:").
	Route(ctx context.Context, zoneID string) (dialer domain.Dialer, resolver domain.Resolver, release func(), err error)
}

// RawForwarder is the minimal async-forwarding contract. *tunnelForwarder
// satisfies it; a local interface keeps the dependency surface narrow and
// allows tests to inject a fake without constructing a real tunnelForwarder.
type RawForwarder interface {
	ForwardRaw(ctx context.Context, req *http.Request, tunnelID string, dialer domain.Dialer, resolver domain.Resolver) (asyncjob.UpstreamResponse, error)
}

// ZoneRoutingForwarder implements asyncjob.Forwarder by reading the tunnel id
// from the internal tunnel-id header, routing through the scheduler, stripping
// service headers, and delegating to a RawForwarder. Safe for concurrent use;
// all state is read-only after construction.
type ZoneRoutingForwarder struct {
	router Router
	base   RawForwarder
}

// Forward implements asyncjob.Forwarder. It extracts the tunnel id from the
// internal tunnel-id header, routes through the scheduler, and forwards via
// base. On routing errors the returned UpstreamResponse already carries the
// correct status code and X-Proxy-Error header so the stored async job result
// is distinguishable from a genuine upstream failure.
//
// Error classification (RESOLVED #7):
//   - unknown zone (publicerror prefix "unknown_zone:") → 400 + X-Proxy-Error: unknown_zone
//   - device bring-up failure (publicerror prefix "zone_bring_up_failure:") → 502 + X-Proxy-Error set
//   - non-publicerror from Route → 502 (treated as proxy fault)
//   - ForwardRaw error → returned as-is (pool.runWorker writes its own 502 for non-nil forwarder errors)
func (z *ZoneRoutingForwarder) Forward(ctx context.Context, req *http.Request) (asyncjob.UpstreamResponse, error) {
	zoneID := req.Header.Get(internalTunnelIDHeader)
	if zoneID == "" {
		return zoneErrorResponse(http.StatusBadRequest, "tunnel_required"), nil
	}

	dialer, resolver, release, err := z.router.Route(ctx, zoneID)
	if err != nil {
		return classifyRouteError(err), nil
	}
	// release is deferred so it is called even if base.ForwardRaw panics.
	defer release()

	// strip service headers before the upstream sees them. serveAsync clones
	// the full inbound request (including the internal tunnel-id header,
	// X-Vpntunnel-Token, etc.) and ForwardRaw calls client.Do directly without
	// running copyProxyHeaders, so the strip must happen here.
	stripServiceHeaders(req)

	return z.base.ForwardRaw(ctx, req, zoneID, dialer, resolver)
}

// compile-time assertions: the concrete types must satisfy their interfaces.
var _ asyncjob.Forwarder = (*ZoneRoutingForwarder)(nil)
var _ RawForwarder = (*tunnelForwarder)(nil)

// classifyRouteError maps a Route error to the stored UpstreamResponse the
// async client will see when polling the job result.
func classifyRouteError(err error) asyncjob.UpstreamResponse {
	pe, ok := publicerror.Is(err)
	if !ok {
		// unexpected non-public error from Route (scheduler bug or ctx cancel):
		// treat as proxy fault → 502.
		return zoneErrorResponse(http.StatusBadGateway, "zone_error")
	}

	// classify against the prefix constants the scheduler exports (single source
	// of truth shared with Route), not an inline literal that could drift.
	msg := pe.Details()
	if strings.HasPrefix(msg, lazy.PrefixUnknownZone) {
		return zoneErrorResponse(http.StatusBadRequest, "unknown_zone")
	}
	// zone_bring_up_failure or any other publicerror from the scheduler is our
	// fault → 502.
	return zoneErrorResponse(http.StatusBadGateway, "zone_bring_up_failure")
}

// zoneErrorResponse builds an UpstreamResponse with the given HTTP status and
// an X-Proxy-Error header set to code. The presence of X-Proxy-Error is the
// operator contract for "this error originated from the daemon, not upstream"
// (RESOLVED #7; mirrors handlers/envelope.go and forwarder.go line 315).
func zoneErrorResponse(status int, code string) asyncjob.UpstreamResponse {
	h := make(http.Header)
	h.Set("X-Proxy-Error", code)
	h.Set("Content-Type", "application/json")
	// encode the body rather than concatenate so a future caller passing a
	// non-literal code cannot produce malformed or injectable JSON.
	body, err := json.Marshal(map[string]string{"error": code})
	if err != nil {
		// unreachable: marshaling a map[string]string cannot fail. Guard rather
		// than discard the error, per the no-skipped-errors rule.
		body = []byte(`{"error":"internal"}`)
	}
	return asyncjob.UpstreamResponse{
		StatusCode: status,
		Header:     h,
		Body:       body,
	}
}

// stripServiceHeaders deletes all serviceHeaders entries from req.Header in
// place. Called before passing the async-pool request to ForwardRaw so that
// internal vpntunnel headers never reach the upstream.
func stripServiceHeaders(req *http.Request) {
	for k := range serviceHeaders {
		req.Header.Del(k)
	}
}
