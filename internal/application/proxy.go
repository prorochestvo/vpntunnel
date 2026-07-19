// Package application implements the core proxy logic: HandleHTTP for
// plain-HTTP forward requests and HandleCONNECT for HTTPS tunnels.
//
// Error contract: methods call publicerror.New for failures whose cause is
// useful to the client — bad input, concurrency limit, and upstream
// unreachable. The fallback constant ErrFallbackMessage is reserved for
// genuinely unexpected impl failures (request build error, hijacker missing)
// where leaking detail would obscure intent. The HTTP transport layer does
// NOT need to inspect errors; the service writes the response itself.
package application

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vpntunnel/internal/domain"
	"vpntunnel/internal/egress"
	"vpntunnel/internal/infrastructure/auth"
	"vpntunnel/internal/infrastructure/observability"
	"vpntunnel/internal/publicerror"
)

// ErrFallbackMessage is the generic error body sent to clients when an
// infrastructure failure occurs that must not leak internal details.
const ErrFallbackMessage = "Something went wrong. Try again later."

// NewProxyService constructs a ProxyService from opts. It panics if opts.Dialer
// is nil, as the dialer is the service's core dependency.
func NewProxyService(opts ProxyServiceOptions) *ProxyService {
	if opts.Dialer == nil {
		panic("application: NewProxyService requires a non-nil Dialer")
	}

	transport := &http.Transport{
		DialContext: opts.Dialer.DialContext,
		// ResponseHeaderTimeout bounds the wait for the first response byte from
		// upstream; it does not affect long-lived CONNECT tunnel body streaming.
		ResponseHeaderTimeout: opts.DialTimeout,
	}
	client := &http.Client{
		Transport: transport,
		// the proxy must not follow redirects — the client's job, not ours.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	svc := &ProxyService{
		dialer:       opts.Dialer,
		verifier:     opts.Verifier,
		httpClient:   client,
		access:       opts.Access,
		opLog:        opts.OpLog,
		dialTimeout:  opts.DialTimeout,
		tunnelNotify: make(chan struct{}),
	}
	return svc
}

// ProxyServiceOptions holds all dependencies for ProxyService. Pass by value;
// all pointers inside must be non-nil except OpLog (defaults to slog.Default)
// and Verifier (nil disables auth).
type ProxyServiceOptions struct {
	// Dialer routes outbound TCP connections. Required.
	Dialer egress.Dialer
	// Access is the rotating access log writer.
	Access *observability.AccessLogger
	// OpLog is the operational slog logger. If nil, slog.Default() is used.
	OpLog *slog.Logger
	// DialTimeout bounds each upstream dial attempt and the wait for the first
	// response byte from upstream (ResponseHeaderTimeout on the transport).
	DialTimeout time.Duration
	// Verifier authenticates incoming proxy requests via Proxy-Authorization.
	// Optional — when nil, auth is disabled and all requests pass through.
	Verifier auth.Verifier
}

// ProxyService implements forward HTTP proxying (HandleHTTP) and HTTPS
// tunnelling (HandleCONNECT). Methods are safe for concurrent use.
type ProxyService struct {
	dialer      egress.Dialer
	verifier    auth.Verifier
	httpClient  *http.Client
	access      *observability.AccessLogger
	opLog       *slog.Logger
	dialTimeout time.Duration
	// tunnelCount tracks active CONNECT tunnels. It is incremented atomically
	// inside HandleCONNECT before the goroutine is launched and decremented
	// when the goroutine exits. tunnelNotify is closed and replaced each time
	// the count reaches zero, allowing WaitTunnels to poll without a race.
	tunnelCount  atomic.Int64
	tunnelMu     sync.Mutex // guards tunnelNotify replacement
	tunnelNotify chan struct{}

	// activeSessions is a live gauge of in-flight streaming sessions (CONNECT
	// tunnels plus in-flight plain-HTTP forwards), independent of tunnelCount
	// and unaffected by WaitTunnels' CONNECT-only shutdown drain. See
	// ActiveSessions for the increment/decrement contract.
	activeSessions atomic.Int64
}

// ActiveSessions returns the current count of in-flight streaming sessions:
// CONNECT tunnels plus in-flight plain-HTTP forwards. It is a live gauge, not
// a cumulative counter — it returns to 0 once every in-flight session has
// finished.
//
// Concurrency contract: on every handler path, activeSessions.Add(1) strictly
// precedes the DialContext/httpClient.Do call that follows it. The matching
// Add(-1) runs on handler return for HandleHTTP; for HandleCONNECT it runs
// only after BOTH the client and upstream connections have been Closed —
// inside the existing tunnel goroutine's defer, guarded by a sessionHandedOff
// flag so every pre-handoff early return decrements exactly once and the
// happy path never double-decrements on handler return. This gauge is
// intentionally separate from tunnelCount/WaitTunnels, which retain their
// CONNECT-only shutdown-drain semantics unchanged.
func (s *ProxyService) ActiveSessions() int64 {
	return s.activeSessions.Load()
}

// HandleHTTP processes a plain forward-HTTP request (GET, POST, etc.).
// It strips hop-by-hop headers, re-issues the request through the configured
// dialer, streams the response, and emits one access log record.
//
// Writes a *publicerror.Error body for bad-input (400, non-absolute URI) and
// for upstream-unreachable (502). Unexpected impl failures get 500 with
// ErrFallbackMessage.
func (s *ProxyService) HandleHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(w, r) {
		return
	}

	start := time.Now()
	summary := domain.RequestSummary{
		Method:     r.Method,
		Target:     logTarget(r.URL),
		ClientAddr: r.RemoteAddr,
	}

	if !r.URL.IsAbs() {
		err := publicerror.New("Proxy expects absolute-URI request form.")
		http.Error(w, err.Details(), http.StatusBadRequest)
		summary.StatusCode = http.StatusBadRequest
		summary.UpstreamError = err.Error()
		summary.DurationMS = time.Since(start).Milliseconds()
		s.logAccess(summary)
		return
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
	if err != nil {
		s.logger().Error("build outbound request", slog.String("err", err.Error()))
		http.Error(w, ErrFallbackMessage, http.StatusInternalServerError)
		summary.StatusCode = http.StatusInternalServerError
		summary.UpstreamError = err.Error()
		summary.DurationMS = time.Since(start).Milliseconds()
		s.logAccess(summary)
		return
	}
	// safety: NewRequestWithContext leaves RequestURI empty; assert here so a
	// future switch to r.Clone(ctx) doesn't silently break upstream routing.
	outReq.RequestURI = ""

	// copy and sanitise headers
	copyHeaders(outReq.Header, r.Header)
	stripHopByHop(outReq.Header, r.Header)
	outReq.Host = r.URL.Host

	s.activeSessions.Add(1)
	defer s.activeSessions.Add(-1)
	resp, err := s.httpClient.Do(outReq)
	if err != nil {
		s.logger().Error("upstream request failed",
			slog.String("target", logTarget(r.URL)),
			slog.String("err", err.Error()))
		pe := publicerror.New("Upstream unreachable.")
		http.Error(w, pe.Details(), http.StatusBadGateway)
		summary.StatusCode = http.StatusBadGateway
		summary.UpstreamError = err.Error()
		summary.DurationMS = time.Since(start).Milliseconds()
		s.logAccess(summary)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	stripHopByHop(resp.Header, resp.Header)
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	summary.StatusCode = resp.StatusCode

	n, _ := io.Copy(w, resp.Body)
	summary.BytesOut = n
	summary.DurationMS = time.Since(start).Milliseconds()
	s.logAccess(summary)
}

// HandleCONNECT processes an HTTP CONNECT request, establishing a bidirectional
// tunnel between the client and the upstream target through the configured dialer.
//
// Order of operations: validate → dial → hijack → write 200 →
// clear deadlines → copy in background goroutine. Dial failure is written
// before hijacking, preserving proper HTTP status semantics.
func (s *ProxyService) HandleCONNECT(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(w, r) {
		return
	}

	start := time.Now()
	target := r.Host
	summary := domain.RequestSummary{
		Method:     r.Method,
		Target:     target,
		ClientAddr: r.RemoteAddr,
	}

	if _, _, err := net.SplitHostPort(target); err != nil {
		pe := publicerror.New("Invalid CONNECT target. Expected host:port.")
		http.Error(w, pe.Details(), http.StatusBadRequest)
		summary.StatusCode = http.StatusBadRequest
		summary.UpstreamError = pe.Error()
		summary.DurationMS = time.Since(start).Milliseconds()
		s.logAccess(summary)
		return
	}

	dialCtx, dialCancel := context.WithTimeout(r.Context(), s.dialTimeout)
	defer dialCancel()

	s.activeSessions.Add(1)
	sessionHandedOff := false
	defer func() {
		if !sessionHandedOff {
			s.activeSessions.Add(-1) // every pre-handoff early return lands here
		}
	}()

	upstreamConn, err := s.dialer.DialContext(dialCtx, "tcp", target)
	if err != nil {
		s.logger().Error("CONNECT dial failed",
			slog.String("target", target),
			slog.String("err", err.Error()))
		pe := publicerror.New("Upstream unreachable.")
		http.Error(w, pe.Details(), http.StatusBadGateway)
		summary.StatusCode = http.StatusBadGateway
		summary.UpstreamError = err.Error()
		summary.DurationMS = time.Since(start).Milliseconds()
		s.logAccess(summary)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		s.logger().Error("ResponseWriter does not support hijack; this is a server config bug")
		http.Error(w, ErrFallbackMessage, http.StatusInternalServerError)
		_ = upstreamConn.Close()
		summary.StatusCode = http.StatusInternalServerError
		summary.DurationMS = time.Since(start).Milliseconds()
		s.logAccess(summary)
		return
	}

	clientConn, bufrw, err := hj.Hijack()
	if err != nil {
		s.logger().Error("hijack failed", slog.String("err", err.Error()))
		_ = upstreamConn.Close()
		summary.StatusCode = http.StatusInternalServerError
		summary.DurationMS = time.Since(start).Milliseconds()
		s.logAccess(summary)
		return
	}

	// clear any server-set deadlines; CONNECT tunnels must outlive them.
	// WriteTimeout intentionally unset on the Server; CONNECT tunnels must
	// outlive any per-write deadline. The hijacked conn manages its own lifecycle.
	if err := clientConn.SetDeadline(time.Time{}); err != nil {
		s.logger().Warn("could not clear deadline on hijacked conn", slog.String("err", err.Error()))
	}

	// write the 200 response directly — w is dead after Hijack.
	// EXACTLY "HTTP/1.1 200 Connection established\r\n\r\n" — no Date, no Server.
	_, err = fmt.Fprint(bufrw, "HTTP/1.1 200 Connection established\r\n\r\n")
	if err != nil {
		_ = clientConn.Close()
		_ = upstreamConn.Close()
		summary.StatusCode = http.StatusInternalServerError
		summary.DurationMS = time.Since(start).Milliseconds()
		s.logAccess(summary)
		return
	}
	if err := bufrw.Flush(); err != nil {
		_ = clientConn.Close()
		_ = upstreamConn.Close()
		summary.StatusCode = http.StatusInternalServerError
		summary.DurationMS = time.Since(start).Milliseconds()
		s.logAccess(summary)
		return
	}
	// capture fields for the tunnel goroutine before the handler returns.
	tunnelMethod := summary.Method
	tunnelTarget := summary.Target
	tunnelClient := summary.ClientAddr
	tunnelStart := start

	sessionHandedOff = true
	s.tunnelCount.Add(1)
	go func() {
		defer func() {
			s.activeSessions.Add(-1) // only after both conns are Closed, below
			if s.tunnelCount.Add(-1) == 0 {
				s.tunnelMu.Lock()
				close(s.tunnelNotify)
				s.tunnelNotify = make(chan struct{})
				s.tunnelMu.Unlock()
			}
		}()

		inCh := make(chan int64, 1)
		outCh := make(chan int64, 1)

		// read from bufrw.Reader (not clientConn directly) so any data the
		// Hijack buffered reader already consumed is not lost or raced against.
		go func() {
			n, _ := io.Copy(upstreamConn, bufrw)
			inCh <- n
			// half-close the write direction so the remote end sees EOF and
			// can send its final bytes before the connection is fully closed.
			halfClose(upstreamConn)
		}()
		go func() {
			n, _ := io.Copy(clientConn, upstreamConn)
			outCh <- n
			halfClose(clientConn)
		}()

		// Wait for both directions to finish. halfClose in each goroutine
		// propagates EOF naturally: when the client stops sending, upstream
		// sees EOF and can send its final bytes; when upstream closes, the
		// client sees EOF. For pathological cases where one side never closes,
		// the WaitTunnels context timeout bounds the total wait at shutdown.
		bytesIn := <-inCh
		bytesOut := <-outCh
		_ = clientConn.Close()
		_ = upstreamConn.Close()

		closeSummary := domain.RequestSummary{
			Method:     tunnelMethod,
			Target:     tunnelTarget,
			ClientAddr: tunnelClient,
			StatusCode: http.StatusOK,
			BytesIn:    bytesIn,
			BytesOut:   bytesOut,
			DurationMS: time.Since(tunnelStart).Milliseconds(),
		}
		s.logAccess(closeSummary)
	}()

	// emit a setup summary immediately; the goroutine emits the close summary with byte counts.
	summary.StatusCode = http.StatusOK
	summary.DurationMS = time.Since(start).Milliseconds()
	// do not call s.logAccess(summary) here; the goroutine logs when the tunnel closes.
}

// WaitTunnels blocks until all active CONNECT tunnels have closed or ctx
// expires. It returns ctx.Err() if the context deadline fires first.
//
// WaitTunnels is safe to call concurrently with HandleCONNECT. It uses an
// atomic counter and a notification channel rather than sync.WaitGroup to
// avoid the WaitGroup Add/Wait race that occurs when HandleCONNECT is still
// executing while WaitTunnels enters its wait.
func (s *ProxyService) WaitTunnels(ctx context.Context) error {
	for {
		if s.tunnelCount.Load() == 0 {
			return nil
		}
		s.tunnelMu.Lock()
		notify := s.tunnelNotify
		s.tunnelMu.Unlock()

		// re-check after acquiring the channel reference
		if s.tunnelCount.Load() == 0 {
			return nil
		}

		select {
		case <-notify:
			// count hit zero; loop and check again
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *ProxyService) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.verifier == nil {
		return true
	}
	if isLoopbackRemote(r.RemoteAddr) {
		s.logger().Info("auth bypassed",
			slog.String("reason", "loopback"),
			slog.String("client_addr", r.RemoteAddr),
		)
		return true
	}
	header := r.Header.Get("Proxy-Authorization")
	if header != "" && s.verifier.Verify(header) {
		return true
	}

	reason := classifyAuthFailure(header)
	target := authLogTarget(r)
	w.Header().Set("Proxy-Authenticate", `Bearer realm="vpntunnel"`)
	http.Error(w, "Proxy authentication required.", http.StatusProxyAuthRequired)
	s.logger().Info("auth failed",
		slog.String("client_addr", r.RemoteAddr),
		slog.String("method", r.Method),
		slog.String("target", target),
		slog.String("reason", reason),
	)
	s.logAccess(domain.RequestSummary{
		Method:     r.Method,
		Target:     target,
		ClientAddr: r.RemoteAddr,
		StatusCode: http.StatusProxyAuthRequired,
	})
	return false
}

// classifyAuthFailure derives the operational-log reason enum for a failed
// Proxy-Authorization header. It does not look at the token value, only at
// the structural shape of the header.
//
// Classification rules:
//   - empty header → "missing"
//   - single field starting with "bearer" (no space between scheme and token,
//     e.g. "Bearertoken", or "Bearer" alone / "Bearer " with trailing whitespace) → "malformed"
//   - first field is not "bearer" (case-insensitive) → "wrong_scheme"
//   - "Bearer <something>" where something doesn't match → "wrong_token"
func classifyAuthFailure(header string) string {
	if header == "" {
		return "missing"
	}
	parts := strings.Fields(header)
	if len(parts) == 0 {
		return "malformed"
	}
	// single-field headers that begin with "bearer": covers "Bearer " (trailing
	// whitespace, Fields collapses to one token) and "Bearertoken" (no space).
	// Both are Bearer misformats — distinguish them from a completely wrong scheme.
	if len(parts) == 1 && strings.HasPrefix(strings.ToLower(parts[0]), "bearer") {
		return "malformed"
	}
	if !strings.EqualFold(parts[0], "Bearer") {
		return "wrong_scheme"
	}
	return "wrong_token"
}

func (s *ProxyService) logAccess(summary domain.RequestSummary) {
	if s.access != nil {
		s.access.Log(summary)
	}
}

func (s *ProxyService) logger() *slog.Logger {
	if s.opLog != nil {
		return s.opLog
	}
	return slog.Default()
}

// authLogTarget picks the appropriate sanitised target for auth failure logging.
// For CONNECT requests, the target is r.Host (host:port). For HTTP requests,
// the target is derived from the URL via logTarget when the URL is absolute.
func authLogTarget(r *http.Request) string {
	if r.Method == http.MethodConnect {
		return r.Host
	}
	if r.URL != nil && r.URL.IsAbs() {
		return logTarget(r.URL)
	}
	return r.RequestURI
}

// halfClose attempts to signal EOF in the write direction without closing the
// connection entirely. This allows the remote end to drain and send its final
// bytes before the conn is fully closed. Falls back to a full Close when the
// conn is not a *net.TCPConn (e.g. a wrapped connection type).
func halfClose(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
		return
	}
	_ = conn.Close()
}

// isLoopbackRemote reports whether addr (an "ip:port" string as set by net/http
// in r.RemoteAddr) belongs to a client connecting over the loopback interface.
// It covers 127.0.0.0/8, ::1, and IPv4-mapped IPv6 loopback (::ffff:127.0.0.1).
// Empty or unparseable addresses return false (fail closed: an abnormal RemoteAddr
// must not grant an auth bypass — failing open on malformed input is the shape of
// a classic auth bypass bug).
//
// Trust boundary: this function reads r.RemoteAddr, which net/http sets to the TCP
// peer address of the accepted connection. Any middleware that rewrites RemoteAddr
// (a PROXY-protocol parser, a reverse-proxy hop) would silently widen the bypass to
// whatever address that middleware injects. In the current deploy model the daemon
// binds to 127.0.0.1 by default, so this branch is the active gate for local-client
// auth bypass; remote clients reach the daemon only when listen is changed to a
// non-loopback address, at which point this branch is inactive and the Bearer token
// is enforced.
func isLoopbackRemote(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// hopByHopHeaders lists the standard hop-by-hop header names defined in
// RFC 7230 §6.1. These are stripped from both inbound and outbound headers.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

// copyHeaders copies all headers from src to dst without modifying either.
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// stripHopByHop removes hop-by-hop headers from h. It strips the standard
// RFC 7230 §6.1 set plus any header names listed in the incoming Connection
// header value.
//
// Passing the same map for h and incoming is safe: the Connection field-name
// set is snapshotted before any deletion loop so the self-mutation bug
// (deleting Connection before reading it) cannot occur.
func stripHopByHop(h http.Header, incoming http.Header) {
	// snapshot Connection-listed field names before any deletion
	var connFields []string
	for _, v := range incoming["Connection"] {
		for _, f := range strings.Split(v, ",") {
			if name := strings.TrimSpace(f); name != "" {
				connFields = append(connFields, name)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
	for _, name := range connFields {
		h.Del(name)
	}
}

// logTarget returns a sanitized URL string safe to log: it drops userinfo,
// query string, and fragment so client-supplied secrets (tokens, passwords)
// do not reach disk. The scheme, host, and path are preserved for
// operability.
func logTarget(u *url.URL) string {
	safe := *u
	safe.User = nil
	safe.RawQuery = ""
	safe.Fragment = ""
	return safe.String()
}
