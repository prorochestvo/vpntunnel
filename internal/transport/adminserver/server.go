// Package adminserver wraps a small *http.Server bound to the proxy's
// admin address (default 127.0.0.1:8081). It serves the liveness
// probe at /healthz and nothing else; unknown routes return 404 via
// http.ServeMux's default behaviour.
//
// The admin listener is intentionally separate from the proxy
// listener: the proxy listener serves untrusted forward-proxy traffic
// (and uses hijack for CONNECT), while the admin listener serves
// operator/probe traffic with no hijack and a small surface. Running
// them on the same *http.Server would force a shared middleware story
// for two divergent use cases.
package adminserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// New constructs an admin server bound to opts.Addr that serves
// opts.HealthHandler at /healthz. opLog may be nil; if nil
// slog.Default() is used. opts.HealthHandler is required; New panics
// if nil (implementation bug, not a config error).
func New(opts Options, opLog *slog.Logger) *Server {
	logger := opLog
	if logger == nil {
		logger = slog.Default()
	}
	if opts.HealthHandler == nil {
		panic("adminserver: New requires a non-nil HealthHandler")
	}

	mux := http.NewServeMux()
	mux.Handle("/healthz", opts.HealthHandler)

	errLogger := slog.NewLogLogger(logger.Handler(), slog.LevelWarn)

	httpSrv := &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		// WriteTimeout is fine here (no hijack, no streaming).
		WriteTimeout: 5 * time.Second,
		ErrorLog:     errLogger,
	}
	return &Server{http: httpSrv, log: logger}
}

// Options configures the admin server.
type Options struct {
	// Addr is the bind address, "host:port" form. Required (config
	// applies the 127.0.0.1:8081 default before construction).
	Addr string
	// HealthHandler serves GET /healthz. Required; New panics if nil.
	HealthHandler http.Handler
}

// Server wraps *http.Server and exposes Start/StartOn/Shutdown.
type Server struct {
	http *http.Server
	log  *slog.Logger
}

// Start begins listening and serving. Returns nil on graceful
// Shutdown, the actual error otherwise.
func (s *Server) Start() error {
	// log "starting" before bind — "listening" would be premature here.
	// httpserver.Start() has the same asymmetry; left as-is (predates this package).
	s.log.Info("starting admin server", slog.String("addr", s.http.Addr))
	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// StartOn serves on an already-bound listener. Same return semantics
// as Start. Intended for tests that bind the listener before passing
// the address downstream (avoids the TOCTOU race of "grab a free
// port then listen").
func (s *Server) StartOn(ln net.Listener) error {
	s.log.Info("admin server listening", slog.String("addr", ln.Addr().String()))
	if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the server. The caller's ctx bounds the
// wait; if it's already done, Shutdown returns immediately.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}
