// Package httpserver wires an *http.Server that dispatches to ProxyService.
// It owns graceful shutdown: first draining active HTTP handlers via
// http.Server.Shutdown, then waiting for open CONNECT tunnels via
// proxyHandler.WaitTunnels.
package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"vpntunnel/internal/application"
)

// Options configures the HTTP proxy server. It carries only the three scalars
// httpserver needs; it does not depend on the full config.Config.
type Options struct {
	// Listen is the address to bind to, in "host:port" form.
	Listen string
	// ReadHeaderTimeout bounds the time to read request headers.
	// Fed by the proxy's DialTimeout.
	ReadHeaderTimeout time.Duration
	// IdleTimeout is the maximum time to wait for the next request.
	IdleTimeout time.Duration
}

// New constructs a Server from opts, svc, and opLog. opLog may be nil; if nil
// slog.Default() is used. WriteTimeout is intentionally zero — CONNECT tunnels
// must outlive any per-write deadline. The risk of slow-write attacks against
// the forward-HTTP path is mitigated by ReadHeaderTimeout and upstream
// http.Client timeouts; accepted for v1.
func New(opts Options, svc *application.ProxyService, opLog *slog.Logger) *Server {
	return NewWithHandler(opts, svc, opLog)
}

// NewWithHandler constructs a Server that dispatches to handler instead of
// a concrete *application.ProxyService. Intended for testing with stub handlers.
func NewWithHandler(opts Options, handler proxyHandler, opLog *slog.Logger) *Server {
	logger := opLog
	if logger == nil {
		logger = slog.Default()
	}

	// bridge slog into the stdlib log.Logger expected by http.Server.ErrorLog.
	errLogger := slog.NewLogLogger(logger.Handler(), slog.LevelWarn)

	httpSrv := &http.Server{
		Addr:              opts.Listen,
		Handler:           rootHandler(handler),
		ReadHeaderTimeout: opts.ReadHeaderTimeout,
		IdleTimeout:       opts.IdleTimeout,
		// WriteTimeout intentionally unset; CONNECT tunnels must outlive any
		// per-write deadline. The hijacked conn manages its own lifecycle.
		WriteTimeout: 0,
		ErrorLog:     errLogger,
	}

	return &Server{http: httpSrv, handler: handler, log: logger}
}

// Server wraps *http.Server and a proxyHandler to provide Start and Shutdown.
type Server struct {
	http    *http.Server
	handler proxyHandler
	log     *slog.Logger
}

// Start begins listening and serving. It returns nil when Shutdown is called
// (converting http.ErrServerClosed to nil) and the actual error otherwise.
// Callers can treat a non-nil return as a fatal startup failure.
func (s *Server) Start() error {
	s.log.Info("listening", slog.String("addr", s.http.Addr))
	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the server. It first drains active HTTP handlers
// via http.Server.Shutdown (which does NOT drain hijacked CONNECT tunnels),
// then calls WaitTunnels to drain open tunnels. Returns the first non-nil
// error from either call.
//
// Both http.Shutdown and WaitTunnels share the same ctx budget. Tunnel drain
// receives whatever budget remains after http.Shutdown returns; size the
// caller's ShutdownTimeout accordingly (the default 15s is generous for
// typical request completion times).
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.http.Shutdown(ctx); err != nil {
		return err
	}
	return s.handler.WaitTunnels(ctx)
}

// proxyHandler is the test seam that lets transport tests stub the service
// without importing the real implementation. It is unexported intentionally —
// it is a transport-package concern, not a domain contract.
type proxyHandler interface {
	HandleHTTP(w http.ResponseWriter, r *http.Request)
	HandleCONNECT(w http.ResponseWriter, r *http.Request)
	WaitTunnels(ctx context.Context) error
}

// compile-time assertion: *application.ProxyService satisfies proxyHandler.
var _ proxyHandler = (*application.ProxyService)(nil)

func rootHandler(h proxyHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			h.HandleCONNECT(w, r)
			return
		}
		h.HandleHTTP(w, r)
	})
}
