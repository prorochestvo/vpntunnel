package handlers

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"vpntunnel/internal/application/asyncjob"
	"vpntunnel/internal/domain"
	"vpntunnel/internal/infrastructure/ipdeny"
)

// NewTunnelForwarder returns a forwarder that routes outbound requests through
// the tunnel dialer and resolver passed to each Forward call. The maxBody
// parameter caps response body reads via the request's http.MaxBytesReader; it
// must match the proxy handler's configured value. log must not be nil. Safe
// for concurrent use; the transport cache uses sync.Map.
func NewTunnelForwarder(maxBody int64, log *slog.Logger) *tunnelForwarder {
	return &tunnelForwarder{
		maxBody: maxBody,
		log:     log,
	}
}

// tunnelForwarder dispatches the validated outbound *http.Request through the
// selected tunnel's dialer, enforcing the IP deny-list and mapping upstream
// failures to the envelope error codes. Safe for concurrent use; the transport
// cache uses sync.Map.
type tunnelForwarder struct {
	maxBody    int64
	log        *slog.Logger
	transports sync.Map // map[string]*http.Transport, keyed by tunnel ID
}

// Forward dispatches r through the tunnel identified by tunnelID. dialer and
// resolver are used to build (or retrieve from cache) a per-tunnel transport
// that enforces the IP deny-list. Forward writes the upstream response — or an
// error envelope — to w and returns nil in all cases except internal panics,
// which it recovers and maps to 500.
func (f *tunnelForwarder) Forward(
	w http.ResponseWriter,
	r *http.Request,
	tunnelID string,
	dialer domain.Dialer,
	resolver domain.Resolver,
) error {
	// recover from any panic; map to 500 internal_error so the request does not hang.
	defer func() {
		if rec := recover(); rec != nil {
			f.log.Error("proxy forwarder panic",
				slog.String("recover", fmt.Sprint(rec)),
				slog.String("target_host", r.URL.Host),
			)
			WriteError(w, "internal", http.StatusInternalServerError, w.Header().Get("X-Request-Id"))
		}
	}()

	transport := f.transportFor(tunnelID, dialer, resolver)

	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	resp, err := client.Do(r)
	if err != nil {
		f.mapDoError(w, r, err)
		return nil
	}
	defer resp.Body.Close()

	copyUpstreamResponse(w, resp, w.Header().Get("X-Request-Id"))
	return nil
}

// ForwardRaw executes req against the upstream tunnel identified by tunnelID
// and returns the materialised response (status code, headers minus hop-by-hop,
// and body bytes) for later replay. The caller owns req.Body; ForwardRaw closes
// it via the http.Client.Do call chain. Unlike Forward, ForwardRaw does not
// write to an http.ResponseWriter — it materialises the response into an
// asyncjob.UpstreamResponse suitable for storage in the async job store.
//
// Response bodies larger than 10 MiB are silently truncated: the first 10 MiB
// are stored and the rest discarded. This is a deliberate design trade-off for
// v2 (bounded storage); callers that need the full body must increase the cap or
// handle the truncation themselves.
//
// Returns a non-nil error on upstream dial failure, context cancellation, or
// body-read failure. The error is never nil together with a non-zero StatusCode.
func (f *tunnelForwarder) ForwardRaw(
	ctx context.Context,
	req *http.Request,
	tunnelID string,
	dialer domain.Dialer,
	resolver domain.Resolver,
) (asyncjob.UpstreamResponse, error) {
	transport := f.transportFor(tunnelID, dialer, resolver)

	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	resp, err := client.Do(req)
	if err != nil {
		return asyncjob.UpstreamResponse{}, f.classifyRawError(err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, forwardRawMaxBody))
	if readErr != nil {
		return asyncjob.UpstreamResponse{}, fmt.Errorf("forwarder: read upstream body: %w", readErr)
	}
	if int64(len(body)) == forwardRawMaxBody {
		f.log.Warn("forwarder: response body truncated at cap",
			"cap_bytes", forwardRawMaxBody,
			"status", resp.StatusCode,
		)
	}

	// strip hop-by-hop headers using the shared hopByHopHeaders map.
	header := make(http.Header, len(resp.Header))
	for key, vals := range resp.Header {
		if isHopByHop(key) {
			continue
		}
		header[key] = vals
	}

	return asyncjob.UpstreamResponse{
		StatusCode: resp.StatusCode,
		Header:     header,
		Body:       body,
	}, nil
}

// classifyRawError maps client.Do errors for ForwardRaw. It uses classifyDoError
// to produce a controlled-vocabulary string with no host:port tokens.
// %w is intentionally NOT used: the raw error chain would re-introduce host:port
// via err.Error() at any future log site. Callers (the async pool's runWorker)
// treat any non-nil return as a failure; the cause is not load-bearing for matching.
func (f *tunnelForwarder) classifyRawError(err error) error {
	return fmt.Errorf("forwarder: %s", classifyDoError(err))
}

// transportFor returns the cached *http.Transport for tunnelID, building and
// storing one on first access. The transport's DialContext enforces the IP
// deny-list before dialing through the tunnel.
func (f *tunnelForwarder) transportFor(id string, dialer domain.Dialer, resolver domain.Resolver) *http.Transport {
	if existing, ok := f.transports.Load(id); ok {
		return existing.(*http.Transport)
	}
	t := &http.Transport{
		DialContext:           f.denyAwareDial(dialer, resolver),
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    false,
	}
	actual, _ := f.transports.LoadOrStore(id, t)
	return actual.(*http.Transport)
}

// denyAwareDial returns a DialContext function that resolves the target host
// via the tunnel's Resolver, rejects any resolved IP that matches DefaultDeny,
// and dials the first acceptable IP via the tunnel's Dialer.
//
// This runs AFTER hostname resolution to defeat DNS rebinding — an attacker
// who controls a public domain that resolves to a private IP via tunnel DNS
// would otherwise reach into private space. .Unmap() is called on each
// resolved address so IPv4-mapped IPv6 addresses (::ffff:10.x.x.x) correctly
// match their IPv4 entries in DefaultDeny.
func (f *tunnelForwarder) denyAwareDial(dialer domain.Dialer, resolver domain.Resolver) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}

		// if host is already a literal IP, check directly without DNS.
		if ip, parseErr := netip.ParseAddr(host); parseErr == nil {
			if ipdeny.Contains(ipdeny.DefaultDeny(), ip.Unmap()) {
				return nil, errDenyList
			}
			return dialer.DialContext(ctx, network, address)
		}

		addrs, err := resolver.LookupHost(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, &net.DNSError{Err: "no such host", Name: host}
		}
		for _, addr := range addrs {
			if ipdeny.Contains(ipdeny.DefaultDeny(), addr.Unmap()) {
				return nil, errDenyList
			}
		}
		// dial the first address; failover across resolved addresses is out of
		// scope for v1.
		first := addrs[0]
		return dialer.DialContext(ctx, network, net.JoinHostPort(first.String(), port))
	}
}

// mapDoError maps errors from client.Do to the appropriate HTTP error envelope.
func (f *tunnelForwarder) mapDoError(w http.ResponseWriter, r *http.Request, err error) {
	reqID := w.Header().Get("X-Request-Id")

	// body cap: *http.MaxBytesError surfaces from client.Do when the transport
	// reads the request body. Map to 413.
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		WriteError(w, "body_too_large", http.StatusRequestEntityTooLarge, reqID)
		return
	}

	// deny-list rejection — wrapped error chain may include errDenyList.
	if errors.Is(err, errDenyList) {
		WriteError(w, "private_address_denied", http.StatusForbidden, reqID)
		return
	}

	// deadline exceeded → 504 upstream_timeout. context.Canceled from a client
	// disconnect is a different class — do not map it to upstream_timeout.
	if errors.Is(err, context.DeadlineExceeded) {
		WriteError(w, "upstream_timeout", http.StatusGatewayTimeout, reqID)
		return
	}
	if errors.Is(err, context.Canceled) {
		// client disconnected; nothing useful to write back.
		f.log.Debug("proxy forwarder: client disconnected", slog.String("request_id", reqID))
		return
	}

	// DNS resolution failure → 502 upstream_dns_failed. May be nested inside
	// *url.Error; errors.As walks the chain.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		WriteError(w, "upstream_dns_failed", http.StatusBadGateway, reqID)
		return
	}

	// any other dial / connect / reset → 502 upstream_connect_failed.
	// classifyDoError produces a short controlled message; never log raw .Error()
	// strings which may carry resolved IPs, hostnames, or query strings.
	opMsg := classifyDoError(err)
	f.log.Debug("proxy forwarder error",
		slog.String("request_id", reqID),
		slog.String("target_scheme", r.URL.Scheme),
		slog.String("target_host", r.URL.Host),
		slog.String("op_msg", opMsg),
	)
	WriteError(w, "upstream_connect_failed", http.StatusBadGateway, reqID)
}

// forwardRawMaxBody is the cap on response body bytes materialised by ForwardRaw.
// Responses larger than 10 MiB have their body silently truncated to this limit;
// the first 10 MiB are stored and the remainder is dropped. This matches the
// inbound body cap enforced by the pool (asyncjob.maxBodyBytes) so the stored
// round-trip is symmetrically bounded.
const forwardRawMaxBody = int64(10 * 1024 * 1024) // 10 MiB

// errDenyList is the sentinel returned by denyAwareDial when the target IP
// matches ipdeny.DefaultDeny(). It is matched via errors.Is in mapDoError.
var errDenyList = errors.New("ipdeny: target resolves to a denied address")

// hopByHopHeaders are stripped from upstream responses per RFC 7230 §6.1.
// Duplicated here (not extracted from internal/application/proxy.go) because the
// two sites must be free to diverge in v1: the forward proxy on 7788 strips
// these on inbound requests; the API on 8888 strips them on outbound responses.
// The list is small and stable.
var hopByHopHeaders = map[string]struct{}{
	"Connection":         {},
	"Proxy-Connection":   {},
	"Keep-Alive":         {},
	"Te":                 {},
	"Trailer":            {},
	"Transfer-Encoding":  {},
	"Upgrade":            {},
	"Proxy-Authenticate": {},
}

// copyUpstreamResponse writes the upstream response status + headers + body to
// w. Hop-by-hop response headers (RFC 7230 §6.1) are stripped. The X-Request-Id
// header from our side overrides any value the upstream may have sent.
// X-Proxy-Error is NOT set — its presence is the operator contract for
// "this came from the daemon, not the upstream".
func copyUpstreamResponse(w http.ResponseWriter, resp *http.Response, requestID string) {
	for key, vals := range resp.Header {
		if isHopByHop(key) {
			continue
		}
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	// override X-Request-Id with our value.
	if requestID != "" {
		w.Header().Set("X-Request-Id", requestID)
	}
	// drop any X-Proxy-Error upstream might have set.
	w.Header().Del("X-Proxy-Error")

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// classifyDoError inspects the error chain from http.Client.Do and returns a
// short, controlled operational message. It never returns raw stdlib error
// strings, resolved IPs, or host:port tokens.
//
// Classification order:
//  1. *url.Error → unwrap to *net.OpError, classify by Op ("dial", "read", "write").
//  2. *url.Error → unwrap to tls.RecordHeaderError (malformed TLS record).
//  3. Generic *url.Error fall-through.
//  4. Unknown error shape → generic "upstream error".
func classifyDoError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		var opErr *net.OpError
		if errors.As(urlErr.Err, &opErr) {
			switch opErr.Op {
			case "dial":
				if opErr.Timeout() {
					return "upstream dial timeout"
				}
				return "upstream connection refused"
			case "read":
				if opErr.Timeout() {
					return "upstream read timeout"
				}
				return "upstream read failed"
			case "write":
				return "upstream write failed"
			}
		}
		var tlsErr tls.RecordHeaderError
		if errors.As(urlErr.Err, &tlsErr) {
			return "upstream tls handshake failed"
		}
		return "upstream error"
	}
	return "upstream error"
}

// isHopByHop reports whether key (in any casing) is a hop-by-hop header.
func isHopByHop(key string) bool {
	_, ok := hopByHopHeaders[http.CanonicalHeaderKey(key)]
	return ok
}
