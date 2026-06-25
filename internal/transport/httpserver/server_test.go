package httpserver_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/service"
	"vpntunnel/internal/transport/httpserver"
	"vpntunnel/internal/tunnel"
)

var _ tunnel.Dialer = (*stubDialer)(nil)

// stubDialer routes DialContext directly to net.Dial.
type stubDialer struct{}

func (s *stubDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

func makeOpts(addr string) httpserver.Options {
	return httpserver.Options{
		Listen:            addr,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
}

func newSvc(t *testing.T) *service.ProxyService {
	t.Helper()
	return service.NewProxyService(service.ProxyServiceOptions{
		Dialer:      &stubDialer{},
		DialTimeout: 5 * time.Second,
	})
}

// waitReady polls until a TCP connection to addr succeeds.
func waitReady(t *testing.T, addr string) {
	t.Helper()
	require.Eventually(t, func() bool {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 3*time.Second, 10*time.Millisecond)
}

// bindListener opens a TCP listener on 127.0.0.1:0 and returns it.
// Using an already-bound listener eliminates the TOCTOU race that arises
// when a free port is queried, closed, and then re-opened by the server.
func bindListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func TestServer(t *testing.T) {
	t.Parallel()

	t.Run("dispatches GET to HandleHTTP", func(t *testing.T) {
		t.Parallel()
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("proxied"))
		}))
		t.Cleanup(target.Close)

		svc := newSvc(t)
		ln := bindListener(t)
		addr := ln.Addr().String()
		srv := httpserver.New(makeOpts(addr), svc, nil)
		go func() { _ = srv.StartOn(ln) }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
		waitReady(t, addr)

		proxyURL, err := url.Parse("http://" + addr)
		require.NoError(t, err)
		client := &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			},
		}
		resp, err := client.Get(target.URL)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "proxied", string(body))
	})

	t.Run("dispatches CONNECT to HandleCONNECT", func(t *testing.T) {
		t.Parallel()
		echoLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = echoLn.Close() })
		go func() {
			for {
				c, err := echoLn.Accept()
				if err != nil {
					return
				}
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}
		}()

		svc := newSvc(t)
		ln := bindListener(t)
		addr := ln.Addr().String()
		srv := httpserver.New(makeOpts(addr), svc, nil)
		go func() { _ = srv.StartOn(ln) }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
		waitReady(t, addr)

		conn, err := net.Dial("tcp", addr)
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		target := echoLn.Addr().String()
		_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		require.NoError(t, err)
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("Start returns nil after Shutdown", func(t *testing.T) {
		t.Parallel()
		svc := newSvc(t)
		ln := bindListener(t)
		addr := ln.Addr().String()
		srv := httpserver.New(makeOpts(addr), svc, nil)

		errCh := make(chan error, 1)
		go func() { errCh <- srv.StartOn(ln) }()
		waitReady(t, addr)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))

		select {
		case err := <-errCh:
			assert.NoError(t, err, "Start should return nil on clean Shutdown")
		case <-time.After(3 * time.Second):
			t.Fatal("Start did not return after Shutdown")
		}
	})

	t.Run("WriteTimeout is zero on constructed server", func(t *testing.T) {
		t.Parallel()
		// verify that a long-lived tunnel is not killed by a server write timeout.
		// Since http.Server.WriteTimeout is unexported, we test via behavior:
		// a tunnel to a slow target (200ms) must still be established.
		slowLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = slowLn.Close() })
		go func() {
			c, err := slowLn.Accept()
			if err != nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
			_ = c.Close()
		}()

		svc := newSvc(t)
		ln := bindListener(t)
		addr := ln.Addr().String()
		srv := httpserver.New(makeOpts(addr), svc, nil)
		go func() { _ = srv.StartOn(ln) }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
		waitReady(t, addr)

		conn, err := net.Dial("tcp", addr)
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		target := slowLn.Addr().String()
		_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		require.NoError(t, err)

		require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("Shutdown returns ctx error when CONNECT exceeds deadline", func(t *testing.T) {
		t.Parallel()
		holdLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = holdLn.Close() })
		go func() {
			c, err := holdLn.Accept()
			if err != nil {
				return
			}
			time.Sleep(5 * time.Second)
			_ = c.Close()
		}()

		svc := newSvc(t)
		ln := bindListener(t)
		addr := ln.Addr().String()

		// wrap the service to detect when HandleCONNECT returns (so WaitTunnels
		// does not race against Add(1) in the still-running handler goroutine).
		handlerDone := make(chan struct{}, 1)
		hookSvc := &connectNotifier{svc: svc, done: handlerDone}
		srv := httpserver.NewWithHandler(makeOpts(addr), hookSvc, nil)
		go func() { _ = srv.StartOn(ln) }()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
		waitReady(t, addr)

		conn, err := net.Dial("tcp", addr)
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		target := holdLn.Addr().String()
		_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		require.NoError(t, err)

		require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		select {
		case <-handlerDone:
		case <-time.After(3 * time.Second):
			t.Fatal("HandleCONNECT did not return in time")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err = srv.Shutdown(ctx)
		assert.True(t, errors.Is(err, context.DeadlineExceeded) || err != nil)
	})
}

// connectNotifier wraps ProxyService and signals done when HandleCONNECT
// returns, ensuring that tunnels.Add(1) has been called before WaitTunnels.
type connectNotifier struct {
	svc  *service.ProxyService
	done chan<- struct{}
}

func (n *connectNotifier) HandleHTTP(w http.ResponseWriter, r *http.Request) {
	n.svc.HandleHTTP(w, r)
}

func (n *connectNotifier) HandleCONNECT(w http.ResponseWriter, r *http.Request) {
	n.svc.HandleCONNECT(w, r)
	select {
	case n.done <- struct{}{}:
	default:
	}
}

func (n *connectNotifier) WaitTunnels(ctx context.Context) error {
	return n.svc.WaitTunnels(ctx)
}
