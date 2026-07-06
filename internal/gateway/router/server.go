package router

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"vpntunnel/internal/application/asyncjob"
	"vpntunnel/internal/domain"
	"vpntunnel/internal/gateway/httpV1/handlers"
	"vpntunnel/internal/infrastructure/observability"
)

// Options carries everything Server needs to start.
//
// Server holds references to LiveHealth, TunnelCatalog, ZoneChecker, ZoneRouter,
// and Access but does NOT own their lifecycle — the caller (main.go) is responsible
// for stopping them on shutdown.
//
// ShutdownTimeout is also owned by the caller; Server.Shutdown accepts an
// already-scoped context, so Server itself does not read this field.
type Options struct {
	// Addr is the bind address, in "host:port" form.
	Addr string
	// ShutdownTimeout is the grace period for graceful shutdown. The caller
	// (main.go) uses it to derive the context passed to Shutdown; Server
	// itself does not use this field.
	ShutdownTimeout time.Duration
	// Cert is the TLS certificate for the API listener. When nil, the server runs in
	// plain-HTTP mode (no TLS); when non-nil it serves HTTPS (TLS 1.3 only). The
	// nil-ness of this field is the HTTP-vs-HTTPS mode discriminator, fixed at
	// construction.
	Cert *tls.Certificate
	// Tokens is the loaded role-token map. Required.
	Tokens *Tokens
	// LiveHealth is the aggregated live health source. Required. It feeds only
	// the /v1/admin/health handler; tunnel catalog data comes from TunnelCatalog.
	LiveHealth *handlers.LiveHealthModel
	// TunnelCatalog is the full discovered tunnel set, keyed by HMAC id.
	// Required. *lazy.EligibleSet (NewFullSet) satisfies handlers.TunnelCatalog
	// via Entries(). It feeds the /v1/tunnels handler.
	TunnelCatalog handlers.TunnelCatalog
	// ZoneChecker validates zone eligibility at admission. Required.
	// *lazy.EligibleSet satisfies handlers.ZoneChecker.
	ZoneChecker handlers.ZoneChecker
	// ZoneRouter routes on-demand proxy requests. Required.
	// *lazy.OnDemandScheduler satisfies handlers.Router.
	ZoneRouter handlers.Router
	// MaxRequestBodyBytes caps the request body size. Applied inside the
	// proxy handler; stored here for handler construction.
	MaxRequestBodyBytes int64
	// UpstreamTimeout is the per-request dial timeout for proxy requests.
	UpstreamTimeout time.Duration
	// MaxUpstreamTimeout is the ceiling callers may request via a header.
	MaxUpstreamTimeout time.Duration
	// HealthMaxAge is the staleness threshold for the health handler.
	HealthMaxAge time.Duration
	// JobCounter supplies async job counts for the health endpoint. Optional —
	// when nil the health response reports zero for all three count fields.
	JobCounter handlers.AsyncJobCounter
	// JobPool is the async job pool. Optional — when nil, the proxy handler
	// operates in sync-only mode (no Proxy-Retry-Tag support).
	JobPool *asyncjob.Pool
	// Access is the optional shared access logger. May be nil; when non-nil, a
	// withAccessLog middleware records one JSONL line per /v1/tunnels/{id}/proxy/ request.
	Access *observability.AccessLogger
	// ProxyForwarder is an optional override for the sync HTTP forwarder.
	// SECURITY: when non-nil, the WireGuard dialer and IP deny-list are
	// bypassed entirely. Must only be set in test binaries. Production wiring
	// in cmd/vpntunnel/main.go must NEVER set this field. A WARN log fires at
	// startup when this is non-nil so the misconfiguration is loud.
	ProxyForwarder handlers.Forwarder
	// Rotator triggers a graceful streaming-tunnel rotation for
	// POST /v1/admin/rotate. Optional — when nil, a noopRotator is used so
	// the endpoint answers with a clean 503 "unavailable" instead of a
	// nil-pointer panic.
	Rotator Rotator
}

// New constructs a Server from opts and log. Server is safe for concurrent
// use after New returns — all fields are written once and then read-only.
// The caller must call Start or StartOn exactly once; both are safe to call
// from any goroutine. Shutdown may be called concurrently with the server
// accepting connections.
func New(opts Options, log *slog.Logger) *Server {
	var jobCounter handlers.AsyncJobCounter
	if opts.JobCounter != nil {
		jobCounter = opts.JobCounter
	} else {
		jobCounter = noopCounter{}
	}
	healthHandler := handlers.NewHealthHandler(opts.LiveHealth, opts.HealthMaxAge, jobCounter, log)
	tunnelsHandler := handlers.NewTunnelsHandler(opts.TunnelCatalog, log)

	// use the injected forwarder when provided (e.g. integration tests that run a
	// loopback upstream and cannot use the WireGuard dialer); fall back to the
	// production TunnelForwarder that enforces the IP deny-list.
	fwd := opts.ProxyForwarder
	if fwd == nil {
		fwd = handlers.NewTunnelForwarder(opts.MaxRequestBodyBytes, log)
	} else {
		log.Warn("SECURITY: router.Options.ProxyForwarder override active — WireGuard dialer and IP deny-list bypassed; must only fire in test binaries")
	}
	proxyHandler := handlers.NewProxyHandler(
		opts.ZoneChecker,
		opts.ZoneRouter,
		fwd,
		opts.MaxRequestBodyBytes,
		opts.UpstreamTimeout,
		opts.MaxUpstreamTimeout,
		log,
		opts.JobPool,
	)
	s := &Server{
		opts:           opts,
		log:            log,
		healthHandler:  healthHandler,
		tunnelsHandler: tunnelsHandler,
		proxyHandler:   proxyHandler,
	}
	s.http = s.buildHTTPServer()
	return s
}

// Server wraps *http.Server with TLS and the v1 API routing + middleware.
// All methods are safe for concurrent use. The caller owns the lifecycle of
// LiveHealth, ZoneChecker, ZoneRouter, and AccessLogger passed via Options;
// Server does not close them.
type Server struct {
	opts           Options
	log            *slog.Logger
	http           *http.Server
	healthHandler  http.Handler
	tunnelsHandler http.Handler
	proxyHandler   http.Handler
}

// Start creates a TLS listener on opts.Addr and begins serving. It returns
// nil on graceful Shutdown and the first network error otherwise.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("apiserver: listen %s: %w", s.opts.Addr, err)
	}
	return s.StartOn(ln)
}

// StartOn serves on an already-bound listener. When Options.Cert is non-nil the
// listener is wrapped with TLS 1.3; when nil it serves plain HTTP. Intended for
// tests that create the listener before calling StartOn to avoid the TOCTOU race
// of grabbing a free port and then binding later. Returns nil on clean Shutdown
// and the first error otherwise.
func (s *Server) StartOn(ln net.Listener) error {
	serveLn := ln
	if s.opts.Cert != nil {
		serveLn = tls.NewListener(ln, &tls.Config{
			Certificates: []tls.Certificate{*s.opts.Cert},
			MinVersion:   tls.VersionTLS13,
		})
	}
	s.log.Info("api server listening",
		slog.String("addr", ln.Addr().String()),
		slog.Bool("tls", s.opts.Cert != nil),
	)
	if err := s.http.Serve(serveLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully drains in-flight requests. The caller's ctx bounds
// the wait; once it expires, Shutdown returns immediately with a deadline
// error. The caller is responsible for stopping LiveHealth sources and
// closing AccessLogger after Shutdown returns.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// buildHTTPServer constructs the *http.Server with the full mux + middleware chain.
func (s *Server) buildHTTPServer() *http.Server {
	errLogger := slog.NewLogLogger(s.log.Handler(), slog.LevelWarn)
	return &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.buildMux(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		// WriteTimeout intentionally unset: streaming proxy responses
		// must outlive any per-write deadline.
		WriteTimeout: 0,
		ErrorLog:     errLogger,
	}
}

// buildMux registers all v1 routes and wraps them with the outer middleware chain.
// Middleware ordering (outermost → innermost):
//  1. withRequestID — generates UUIDv7, sets X-Request-Id header + context key.
//  2. withAuth — validates X-Vpntunnel-Token, attaches role to context; 401 on mismatch.
//  3. requireRole (per route) — rejects with 403 if role not in the per-route allow-list.
//  4. withAccessLog (proxy route only, when opts.Access is non-nil) — records one
//     JSONL line per /v1/tunnels/{id}/proxy/ request after the handler returns.
func (s *Server) buildMux() http.Handler {
	mux := http.NewServeMux()

	// GET /v1/tunnels — user or admin. This exact-path route does NOT match
	// /v1/tunnels/{id}/... (covered by the more-specific proxy pattern below);
	// a future /v1/tunnels/{id} detail route must be added carefully to avoid
	// shadowing this listing route or the proxy wildcard.
	mux.HandleFunc("GET /v1/tunnels",
		s.requireRole(RoleProxy, RoleAdmin)(s.tunnelsHandler.ServeHTTP))

	// GET /v1/admin/health — admin only.
	mux.HandleFunc("GET /v1/admin/health",
		s.requireRole(RoleAdmin)(s.handleHealth))

	// /v1/admin/rotate — admin only. Registered methodless (not
	// "POST /v1/admin/rotate") because the "/" catch-all defeats Go's
	// automatic 405 for method-scoped patterns (a non-POST would otherwise
	// fall through to the catch-all's 404); handleRotate checks the method
	// itself and replies 405 with Allow: POST.
	mux.HandleFunc("/v1/admin/rotate",
		s.requireRole(RoleAdmin)(s.handleRotate))

	// /v1/tunnels/{id}/proxy/{scheme}/{rest...} — user or admin. Access logging wraps
	// the route when opts.Access is configured; the sanitiser patterns are applied
	// inside AccessLogger.Log before the JSONL line is written.
	// Body cap (http.MaxBytesReader) is applied inside the proxy handler.
	proxyInner := s.requireRole(RoleProxy, RoleAdmin)(s.proxyHandler.ServeHTTP)
	if s.opts.Access != nil {
		mux.Handle("/v1/tunnels/{id}/proxy/{scheme}/{rest...}", s.withAccessLog(http.HandlerFunc(proxyInner)))
	} else {
		mux.HandleFunc("/v1/tunnels/{id}/proxy/{scheme}/{rest...}", proxyInner)
	}

	// catch-all 404 with JSON envelope (overrides the plain-text ServeMux default).
	mux.Handle("/", s.notFoundHandler())

	return s.withRequestID(s.withAuth(mux))
}

// handleHealth delegates to the healthHandler constructed in New.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.healthHandler.ServeHTTP(w, r)
}

// notFoundHandler returns an http.Handler that emits a 404 JSON envelope with
// X-Proxy-Error: bad_request. The ServeMux default emits plain text; this
// override ensures all error responses share the same JSON envelope shape.
func (s *Server) notFoundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.WriteError(w, "bad_request", http.StatusNotFound, requestIDFromContext(r.Context()))
	})
}

// withRequestID generates a UUIDv7 request ID, sets it on the response via
// X-Request-Id, and stores it in the request context for downstream handlers.
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withAuth reads the X-Vpntunnel-Token header, calls tokens.Match, and attaches
// the resolved Role to the request context. Responds 401 when no token matches.
//
// Security invariants:
//   - The token value is NEVER logged; only role (on success) or reason=unauthorized
//     (on failure) appear in log records.
//   - tokens.Match hashes the header internally and zeroes the copy; withAuth does
//     not inspect or copy the raw header value beyond the single Match call.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("X-Vpntunnel-Token")
		role, ok := s.opts.Tokens.Match(header)
		if !ok {
			s.log.Debug("auth rejected",
				slog.String("reason", "unauthorized"),
				slog.String("request_id", requestIDFromContext(r.Context())),
			)
			handlers.WriteError(w, "unauthorized", http.StatusUnauthorized, requestIDFromContext(r.Context()))
			return
		}
		s.log.Debug("auth accepted",
			slog.String("role", string(role)),
			slog.String("request_id", requestIDFromContext(r.Context())),
		)
		ctx := context.WithValue(r.Context(), ctxKeyRole, role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireRole returns a middleware factory that enforces the per-route role
// allow-list. The outer function pre-builds the set for O(1) membership test.
// Responds 403 when the authenticated role is not in allowed. Responds 500 if
// the role was never set in context (indicates a middleware wiring bug).
//
// Panics if allowed is empty — this is a programmer error caught at wiring
// time, not runtime.
func (s *Server) requireRole(allowed ...Role) func(http.HandlerFunc) http.HandlerFunc {
	if len(allowed) == 0 {
		panic("requireRole: at least one role must be specified")
	}
	set := make(map[Role]struct{}, len(allowed))
	for _, r := range allowed {
		set[r] = struct{}{}
	}
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			role, ok := roleFromContext(r.Context())
			if !ok {
				// auth middleware did not run or did not set the role — bug in wiring.
				s.log.Error("requireRole: role not set in context — middleware wiring bug")
				handlers.WriteError(w, "internal_error", http.StatusInternalServerError, requestIDFromContext(r.Context()))
				return
			}
			if _, ok := set[role]; !ok {
				handlers.WriteError(w, "forbidden", http.StatusForbidden, requestIDFromContext(r.Context()))
				return
			}
			next(w, r)
		}
	}
}

// withAccessLog wraps next so that one JSONL line is appended to opts.Access
// after the handler returns. The middleware captures the response status code
// and the number of bytes written via a thin responseCapture wrapper; the
// original request URL path (not sanitised) is passed to AccessLogger.Log, which
// applies the PathSanitizePattern slice before writing — routing is never affected.
// Callers must ensure opts.Access is non-nil before calling this method.
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rc := &responseCapture{ResponseWriter: w, status: http.StatusOK}
		var bodyIn int64
		if r.Body != nil {
			r.Body = countReader(r.Body, &bodyIn)
		}
		next.ServeHTTP(rc, r)
		s.opts.Access.Log(domain.RequestSummary{
			Method:     r.Method,
			Target:     r.URL.String(),
			ClientAddr: r.RemoteAddr,
			StatusCode: rc.status,
			BytesIn:    bodyIn,
			BytesOut:   rc.written,
			DurationMS: time.Since(start).Milliseconds(),
		})
	})
}

// responseCapture is a thin http.ResponseWriter wrapper that records the status
// code and the number of bytes written so withAccessLog can emit them after the
// handler returns.
type responseCapture struct {
	http.ResponseWriter
	status  int
	written int64
}

func (rc *responseCapture) WriteHeader(code int) {
	rc.status = code
	rc.ResponseWriter.WriteHeader(code)
}

func (rc *responseCapture) Write(b []byte) (int, error) {
	n, err := rc.ResponseWriter.Write(b)
	rc.written += int64(n)
	return n, err
}

func (rc *responseCapture) Flush() {
	if f, ok := rc.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// countingReader wraps an io.ReadCloser and increments n for every byte read.
type countingReader struct {
	io.ReadCloser
	n *int64
}

func countReader(r io.ReadCloser, n *int64) io.ReadCloser {
	return &countingReader{ReadCloser: r, n: n}
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	*c.n += int64(n)
	return n, err
}

// ctxKey is an unexported integer type for request-scoped context keys.
// Using int (not string) ensures keys are positionally distinct and immune
// to value-collisions with keys declared in other packages.
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyRole
)

// requestIDFromContext returns the UUIDv7 request ID stored by withRequestID,
// or empty string if none is present.
func requestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID).(string)
	return v
}

// roleFromContext returns the Role stored by withAuth and a boolean indicating
// whether the key was present. ok is false when the auth middleware did not run.
func roleFromContext(ctx context.Context) (Role, bool) {
	v, ok := ctx.Value(ctxKeyRole).(Role)
	return v, ok
}

// compile-time assertion: noopCounter must satisfy handlers.AsyncJobCounter.
var _ handlers.AsyncJobCounter = noopCounter{}

// noopCounter is a zero-allocation stand-in used when Options.JobCounter is nil.
// It always returns zero counts.
type noopCounter struct{}

func (noopCounter) Counts() (asyncjob.JobCounts, error) { return asyncjob.JobCounts{}, nil }
