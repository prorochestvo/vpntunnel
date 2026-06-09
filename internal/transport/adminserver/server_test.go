package adminserver_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"httpproxy/internal/transport/adminserver"
)

// fakeHandler is a minimal http.Handler for testing dispatch.
type fakeHandler struct {
	body   string
	method string
	status int
}

func (f *fakeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.method = r.Method
	code := f.status
	if code == 0 {
		code = http.StatusOK
	}
	w.WriteHeader(code)
	_, _ = fmt.Fprint(w, f.body)
}

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("panics when HealthHandler is nil", func(t *testing.T) {
		t.Parallel()
		assert.Panics(t, func() {
			adminserver.New(adminserver.Options{
				Addr:          "127.0.0.1:0",
				HealthHandler: nil,
			}, nil)
		})
	})

	t.Run("uses slog.Default when opLog is nil", func(t *testing.T) {
		t.Parallel()
		// verifies the nil-guard: should not panic.
		srv := adminserver.New(adminserver.Options{
			Addr:          "127.0.0.1:0",
			HealthHandler: &fakeHandler{body: "ok"},
		}, nil)
		assert.NotNil(t, srv)
	})
}

func TestServer_StartOn(t *testing.T) {
	t.Parallel()

	// startServer binds a free port, starts the server, and returns the base URL
	// and a cancel func that shuts the server down.
	startServer := func(t *testing.T, handler http.Handler) (baseURL string, shutdown func()) {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)

		srv := adminserver.New(adminserver.Options{
			Addr:          ln.Addr().String(),
			HealthHandler: handler,
		}, nil)

		errCh := make(chan error, 1)
		go func() { errCh <- srv.StartOn(ln) }()

		baseURL = "http://" + ln.Addr().String()
		shutdown = func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			require.NoError(t, srv.Shutdown(ctx))
			// wait for StartOn to return
			select {
			case err := <-errCh:
				assert.NoError(t, err)
			case <-time.After(3 * time.Second):
				t.Error("server did not stop within timeout")
			}
		}
		return baseURL, shutdown
	}

	t.Run("GET /healthz dispatches to supplied handler", func(t *testing.T) {
		t.Parallel()
		handler := &fakeHandler{body: `{"status":"ok"}`, status: http.StatusOK}
		baseURL, shutdown := startServer(t, handler)
		defer shutdown()

		resp, err := http.Get(baseURL + "/healthz") //nolint:noctx
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, `{"status":"ok"}`, string(body))
	})

	t.Run("GET /unknown returns 404", func(t *testing.T) {
		t.Parallel()
		handler := &fakeHandler{body: "ok"}
		baseURL, shutdown := startServer(t, handler)
		defer shutdown()

		resp, err := http.Get(baseURL + "/not-a-route") //nolint:noctx
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("POST /healthz forwarded to handler", func(t *testing.T) {
		t.Parallel()
		handler := &fakeHandler{body: ""}
		baseURL, shutdown := startServer(t, handler)
		defer shutdown()

		resp, err := http.Post(baseURL+"/healthz", "application/json", nil) //nolint:noctx
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		// adminserver does not filter methods; the handler receives the request.
		assert.Equal(t, http.MethodPost, handler.method)
	})

	t.Run("Shutdown returns nil and connection refused after", func(t *testing.T) {
		t.Parallel()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := ln.Addr().String()

		srv := adminserver.New(adminserver.Options{
			Addr:          addr,
			HealthHandler: &fakeHandler{body: "ok"},
		}, nil)

		errCh := make(chan error, 1)
		go func() { errCh <- srv.StartOn(ln) }()

		// wait until the server is actually accepting connections before shutting down.
		require.Eventually(t, func() bool {
			c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
			if err != nil {
				return false
			}
			_ = c.Close()
			return true
		}, 3*time.Second, 10*time.Millisecond, "server never started listening")

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))

		// wait for StartOn goroutine to exit
		select {
		case err := <-errCh:
			assert.NoError(t, err)
		case <-time.After(3 * time.Second):
			t.Fatal("server did not stop within timeout")
		}

		// connection should now be refused
		_, dialErr := net.DialTimeout("tcp", addr, time.Second)
		assert.Error(t, dialErr, "expected connection refused after shutdown")
	})
}
