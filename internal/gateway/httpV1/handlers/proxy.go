package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
	"time"

	"vpntunnel/internal/application/asyncjob"
	"vpntunnel/internal/domain"
)

// NewProxyHandler returns an http.Handler that validates, normalises, and
// forwards proxy requests through the selected tunnel.
//
// maxBodyBytes caps the request body via http.MaxBytesReader. upstreamTimeout
// is the default per-request deadline; clients may override it per-request via
// X-Proxy-Timeout, up to maxUpstreamTimeout. log must not be nil.
//
// ap is the optional async job pool. When nil the handler operates in
// sync-only mode: requests carrying Proxy-Retry-Tag receive 503 async_disabled.
// When non-nil the handler branches on Proxy-Retry-Tag and dispatches to
// serveAsync.
//
// The returned handler is safe for concurrent use: all state is read-only after
// construction.
//
// PathValue decoding caveat: Go's ServeMux URL-decodes path wildcard values
// before returning them from r.PathValue. Operators upstream of the daemon
// should percent-encode anything that must survive verbatim in the target URL
// (e.g. %20 for a literal space).
func NewProxyHandler(
	checker ZoneChecker,
	router Router,
	fwd Forwarder,
	maxBodyBytes int64,
	upstreamTimeout time.Duration,
	maxUpstreamTimeout time.Duration,
	log *slog.Logger,
	ap asyncPool,
) http.Handler {
	return &proxyHandler{
		checker:            checker,
		router:             router,
		fwd:                fwd,
		maxBodyBytes:       maxBodyBytes,
		upstreamTimeout:    upstreamTimeout,
		maxUpstreamTimeout: maxUpstreamTimeout,
		log:                log,
		asyncPool:          ap,
	}
}

// ZoneChecker validates whether a zone ID is eligible for routing. It is
// satisfied by *lazy.EligibleSet and allows tests to inject a fake without
// constructing a real EligibleSet.
type ZoneChecker interface {
	// IsEligible reports whether zoneID is in the eligible set. Safe for
	// concurrent callers.
	IsEligible(zoneID string) bool
}

// asyncPool is the narrow contract the proxy handler depends on for async job
// dispatch. The concrete *asyncjob.Pool satisfies it by structural typing; using
// an interface here lets tests inject a fake without constructing a real pool
// (which requires a bbolt store and an on-disk file).
type asyncPool interface {
	SubmitOrFetch(ctx context.Context, tag string, req *http.Request) (asyncjob.Outcome, error)
}

// compile-time assertion: *asyncjob.Pool must satisfy asyncPool.
var _ asyncPool = (*asyncjob.Pool)(nil)

// Forwarder dispatches a constructed *http.Request to the upstream and streams
// the response back. The tunnelID, dialer, and resolver are passed so the
// implementation can cache a per-tunnel http.Transport and enforce the IP
// deny-list. The concrete *tunnelForwarder returned by NewTunnelForwarder
// satisfies this interface; tests and integration-test helpers may inject
// alternative implementations (e.g. a plain net/http round-tripper that
// bypasses the WireGuard dialer for localhost upstreams).
type Forwarder interface {
	Forward(w http.ResponseWriter, r *http.Request, tunnelID string, dialer domain.Dialer, resolver domain.Resolver) error
}

// proxyHandler implements http.Handler for the /v1/tunnels/{id}/proxy/{scheme}/{rest...} route.
type proxyHandler struct {
	checker            ZoneChecker
	router             Router
	fwd                Forwarder
	maxBodyBytes       int64
	upstreamTimeout    time.Duration
	maxUpstreamTimeout time.Duration
	log                *slog.Logger
	asyncPool          asyncPool
}

// ServeHTTP executes the full validation pipeline and, on success, hands the
// constructed outbound *http.Request to the forwarder.
//
// Validation order (fail-fast on first error):
//  1. scheme must be "http" or "https" → 400 invalid_scheme.
//  2. {id} path segment non-empty → 400 tunnel_required.
//  3. checker.IsEligible verifies the zone is known → 400 unknown_tunnel.
//  4. async branch: if Proxy-Retry-Tag is present, validate and dispatch; CONNECT
//     is rejected here with 405 (raw TCP tunnels cannot be materialised).
//  5. X-Proxy-Timeout (if present) parses and fits within maxUpstreamTimeout → 400.
//  6. X-Proxy-Forward-Headers (if present) split, trimmed, canonicalised.
//  7. Target URL parses without error → 400 bad_request.
//  8. Outbound *http.Request constructed with deadline context, MaxBytesReader body,
//     and filtered headers.
//  9. router.Route acquires a live dialer and resolver → forward via fwd.
//
// 10. forwarder.Forward called; body read errors map to 413 body_too_large.
func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// read the request ID from the response header; the withRequestID middleware
	// sets it on w.Header() before calling the handler so it is already present.
	reqID := w.Header().Get("X-Request-Id")

	// invariant: the upstream target scheme is sourced EXCLUSIVELY from the {scheme}
	// path segment. It MUST NOT be derived from the inbound connection (r.TLS) or the
	// inbound request URL (r.URL.Scheme). The :8888 API listener may run as plain HTTP
	// (dev) or HTTPS (prod); that listener protocol is orthogonal to this target scheme.

	// 1. scheme validation.
	scheme := r.PathValue("scheme")
	if scheme != "http" && scheme != "https" {
		WriteError(w, "invalid_scheme", http.StatusBadRequest, reqID)
		return
	}

	// 2. {id} path segment must be non-empty.
	tunnelID := r.PathValue("id")
	if tunnelID == "" {
		WriteError(w, "tunnel_required", http.StatusBadRequest, reqID)
		return
	}

	// 3. eligibility check: zone must be in the eligible set.
	if !h.checker.IsEligible(tunnelID) {
		WriteError(w, "unknown_tunnel", http.StatusBadRequest, reqID)
		return
	}

	// 4. async branch: only entered when Proxy-Retry-Tag is present. Scheme
	// and eligibility are already validated above; a tagged request with an
	// invalid scheme or unknown tunnel is correctly rejected before here.
	if tag := r.Header.Get("Proxy-Retry-Tag"); tag != "" {
		// CONNECT semantics (raw TCP tunnel) cannot be materialised as a stored
		// response — reject before any async work is attempted.
		if r.Method == http.MethodConnect {
			WriteError(w, "method_not_allowed", http.StatusMethodNotAllowed, reqID)
			return
		}
		if !retryTagRegexp.MatchString(tag) {
			WriteError(w, "tag_invalid", http.StatusBadRequest, reqID)
			return
		}
		if h.asyncPool == nil {
			WriteError(w, "async_disabled", http.StatusServiceUnavailable, reqID)
			return
		}
		h.serveAsync(w, r, tag, tunnelID, reqID)
		return
	}

	// 5. X-Proxy-Timeout parsing and bounds check.
	effectiveTimeout := h.upstreamTimeout
	if raw := r.Header.Get("X-Proxy-Timeout"); raw != "" {
		d, parseErr := time.ParseDuration(raw)
		if parseErr != nil {
			WriteError(w, "bad_request", http.StatusBadRequest, reqID)
			return
		}
		if d <= 0 || d > h.maxUpstreamTimeout {
			WriteError(w, "timeout_too_large", http.StatusBadRequest, reqID)
			return
		}
		effectiveTimeout = d
	}

	// 6. X-Proxy-Forward-Headers: comma-split, trim, drop empties, canonicalise.
	var extraHeaders []string
	if raw := r.Header.Get("X-Proxy-Forward-Headers"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			extraHeaders = append(extraHeaders, textproto.CanonicalMIMEHeaderKey(trimmed))
		}
	}

	// 7. build and validate target URL.
	rest := r.PathValue("rest")
	targetStr := scheme + "://" + rest
	if r.URL.RawQuery != "" {
		targetStr += "?" + r.URL.RawQuery
	}
	targetURL, parseErr := url.Parse(targetStr)
	if parseErr != nil || targetURL.Host == "" {
		WriteError(w, "bad_request", http.StatusBadRequest, reqID)
		return
	}

	// 8. construct outbound *http.Request.
	ctx, cancel := context.WithTimeout(r.Context(), effectiveTimeout)
	defer cancel()

	// MaxBytesReader panics on a nil reader; substitute http.NoBody so the body
	// limit is still enforced on the rare path where the client sent no body.
	rawBody := r.Body
	if rawBody == nil {
		rawBody = http.NoBody
	}
	body := http.MaxBytesReader(w, rawBody, h.maxBodyBytes)

	outReq, newErr := http.NewRequestWithContext(ctx, r.Method, targetURL.String(), body)
	if newErr != nil {
		WriteError(w, "bad_request", http.StatusBadRequest, reqID)
		return
	}

	// copy filtered headers into the outbound request.
	copyProxyHeaders(outReq, r, extraHeaders)

	// 9. acquire a live dialer and resolver via the router, then forward.
	dialer, resolver, release, routeErr := h.router.Route(ctx, tunnelID)
	if routeErr != nil {
		h.log.Error("proxy handler: route error",
			slog.String("request_id", reqID),
			slog.String("tunnel_id", tunnelID),
			slog.String("error", routeErr.Error()),
		)
		WriteError(w, "tunnel_unavailable", http.StatusServiceUnavailable, reqID)
		return
	}
	defer release()

	// 10. forward; map body-size errors to 413.
	if fwdErr := h.fwd.Forward(w, outReq, tunnelID, dialer, resolver); fwdErr != nil {
		if isBodyTooLarge(fwdErr) {
			WriteError(w, "body_too_large", http.StatusRequestEntityTooLarge, reqID)
			return
		}
		h.log.Error("proxy forwarder error",
			slog.String("request_id", reqID),
			slog.String("target_scheme", outReq.URL.Scheme),
			slog.String("target_host", outReq.URL.Host),
			slog.String("error", fwdErr.Error()),
		)
	}
}

// serveAsync dispatches the request to the async job pool and writes the
// appropriate response based on the outcome. tag has already been validated
// against retryTagRegexp by the caller.
//
// The inbound *http.Request r carries the proxy-path URL (/v1/tunnels/{id}/proxy/{scheme}/…)
// as set by the HTTP server, not the upstream absolute URL. SubmitOrFetch
// drains and stores the body, then the worker's Forwarder calls client.Do,
// which requires an absolute URL with a non-empty Host. serveAsync therefore
// reconstructs the absolute upstream URL from the path values before handing
// r to the pool so the worker can dial the correct target. tunnelID is stamped
// on the clone via internalTunnelIDHeader before SubmitOrFetch so the async
// worker can recover the id after the URL is rewritten.
func (h *proxyHandler) serveAsync(w http.ResponseWriter, r *http.Request, tag, tunnelID, reqID string) {
	// same path-only scheme invariant as ServeHTTP — see the comment there.

	// rebuild the absolute upstream URL from the validated path components.
	scheme := r.PathValue("scheme")
	rest := r.PathValue("rest")
	targetStr := scheme + "://" + rest
	if r.URL.RawQuery != "" {
		targetStr += "?" + r.URL.RawQuery
	}
	targetURL, parseErr := url.Parse(targetStr)
	if parseErr != nil || targetURL.Host == "" {
		WriteError(w, "bad_request", http.StatusBadRequest, reqID)
		return
	}
	// clone r so we can override URL without mutating the inbound request; the
	// clone shares everything else (headers, method, body) with r.
	asyncReq := r.Clone(r.Context())
	asyncReq.URL = targetURL
	asyncReq.Host = targetURL.Host // ensure Host header reaches the upstream
	// clear RequestURI: the HTTP server sets it from the inbound request line
	// (/v1/tunnels/{id}/proxy/…), but http.Client.Do rejects a request that has
	// RequestURI set — it is only valid for incoming server requests, not outgoing
	// client requests. The worker's Forwarder uses asyncReq as an outgoing client
	// request, so RequestURI must be empty.
	asyncReq.RequestURI = ""
	// stamp the validated tunnel id on the clone so the async worker can recover
	// it from internalTunnelIDHeader after the URL has been rewritten to the
	// upstream absolute URL (the {id} path segment is no longer present at that
	// point). Stamp on the CLONE, not on r, to avoid mutating the inbound request.
	asyncReq.Header.Set(internalTunnelIDHeader, tunnelID)

	outcome, err := h.asyncPool.SubmitOrFetch(r.Context(), tag, asyncReq)
	if err != nil {
		switch {
		case errors.Is(err, asyncjob.ErrBodyTooLarge):
			WriteError(w, "body_too_large", http.StatusRequestEntityTooLarge, reqID)
		case errors.Is(err, asyncjob.ErrPoolClosed):
			WriteError(w, "shutting_down", http.StatusServiceUnavailable, reqID)
		default:
			echo := tag
			if len(echo) > asyncTagEchoLen {
				echo = echo[:asyncTagEchoLen]
			}
			h.log.Error("async proxy: pool error",
				slog.String("request_id", reqID),
				slog.String("tag_echo", echo),
			)
			WriteError(w, "internal", http.StatusInternalServerError, reqID)
		}
		return
	}

	switch oc := outcome.(type) {
	case asyncjob.OutcomePending:
		// set Proxy-Async-Status BEFORE WriteHeader; once WriteHeader is called
		// the header map is frozen by the http package.
		w.Header().Set("Proxy-Async-Status", "pending")
		w.Header().Set("Content-Type", "application/json")
		if reqID != "" {
			w.Header().Set("X-Request-Id", reqID)
		}
		w.WriteHeader(http.StatusAccepted)
		// unrecoverable once WriteHeader sent.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":    "pending",
			"retry_tag": tag,
		})

	case asyncjob.OutcomeCompleted:
		// replay the stored upstream response verbatim. hop-by-hop headers are
		// stripped using the same isHopByHop helper as ForwardRaw/copyUpstreamResponse
		// in forwarder.go (same package) — single source of truth for the strip list.
		resp := oc.Response
		for key, vals := range resp.Header {
			if isHopByHop(key) {
				continue
			}
			for _, v := range vals {
				w.Header().Add(key, v)
			}
		}
		if reqID != "" {
			w.Header().Set("X-Request-Id", reqID)
		}
		// absence of Proxy-Async-Status is the discriminator that tells the client
		// this is a final upstream response, not a pending envelope.
		w.Header().Del("Proxy-Async-Status")
		w.WriteHeader(resp.StatusCode)
		if len(resp.Body) > 0 {
			_, _ = w.Write(resp.Body)
		}

	case asyncjob.OutcomeTombstoned:
		WriteError(w, "tag_evicted", http.StatusGone, reqID)

	case asyncjob.OutcomeQueueFull:
		h.log.Warn("async proxy: job queue full, rejecting request",
			slog.String("request_id", reqID),
		)
		WriteError(w, "queue_full", http.StatusServiceUnavailable, reqID)

	default:
		// sealed sum — unreachable in practice; log and 500 for safety.
		h.log.Error("async proxy: unknown outcome type",
			slog.String("request_id", reqID),
		)
		WriteError(w, "internal", http.StatusInternalServerError, reqID)
	}
}

// asyncTagEchoLen caps the number of bytes echoed from a Proxy-Retry-Tag in log
// messages to prevent a large client-supplied tag from flooding the log.
const asyncTagEchoLen = 64

// internalTunnelIDHeader is stamped on the async request clone by serveAsync so
// that the async worker (ZoneRoutingForwarder.Forward) can recover the validated
// tunnel id after the URL has been rewritten to the upstream absolute URL. It is
// added to serviceHeaders so that both the sync copyProxyHeaders path and the
// async stripServiceHeaders path drop a client-supplied value of this header.
const internalTunnelIDHeader = "X-Vpntunnel-Tunnel-Id"

// retryTagRegexp validates the Proxy-Retry-Tag header value. Tags must be
// 8–128 characters long and contain only alphanumerics, underscores, and hyphens.
var retryTagRegexp = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,128}$`)

// alwaysForward is the set of headers that are unconditionally copied from the
// client request to the outbound request, unless they appear in serviceHeaders.
var alwaysForward = map[string]struct{}{
	"Content-Type":  {},
	"Accept":        {},
	"Authorization": {},
	"User-Agent":    {},
}

// serviceHeaders lists headers internal to the vpntunnel API. They are stripped
// before forwarding regardless of what the client or allow-list requests.
var serviceHeaders = map[string]struct{}{
	"X-Vpntunnel-Token":       {},
	internalTunnelIDHeader:    {}, // strip client-supplied fake on both sync and async paths
	"X-Proxy-Timeout":         {},
	"X-Proxy-Forward-Headers": {},
	"Proxy-Retry-Tag":         {}, // async idempotency tag; internal, must not reach upstream
}

// copyProxyHeaders copies headers from src into dst according to the allow policy:
// the always-forwarded set plus extra, excluding any service header.
//
// User-Agent suppression: if the client did not send User-Agent we explicitly
// set it to "" so Go's http.Transport does not invent "Go-http-client/…".
func copyProxyHeaders(dst *http.Request, src *http.Request, extra []string) {
	allowed := make(map[string]struct{}, len(alwaysForward)+len(extra))
	for k := range alwaysForward {
		allowed[k] = struct{}{}
	}
	for _, k := range extra {
		allowed[k] = struct{}{}
	}

	for k, vals := range src.Header {
		canonical := textproto.CanonicalMIMEHeaderKey(k)
		if _, isService := serviceHeaders[canonical]; isService {
			continue
		}
		if _, isAllowed := allowed[canonical]; !isAllowed {
			continue
		}
		for _, v := range vals {
			dst.Header.Add(canonical, v)
		}
	}

	// suppress Go's default User-Agent when the client omitted it.
	if src.Header.Get("User-Agent") == "" {
		dst.Header["User-Agent"] = []string{""}
	}
}

// isBodyTooLarge reports whether err is an http.MaxBytesError (body limit exceeded).
func isBodyTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}
