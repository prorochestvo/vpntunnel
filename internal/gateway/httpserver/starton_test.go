package httpserver

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
)

// StartOn serves on an already-bound listener. It returns nil on clean
// Shutdown and the actual error otherwise. Intended for tests that need to
// bind the listener before passing the address to other components, avoiding
// the TOCTOU race inherent in grabbing a free port and then listening later.
func (s *Server) StartOn(ln net.Listener) error {
	s.log.Info("listening", slog.String("addr", ln.Addr().String()))
	if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
