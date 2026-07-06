package handlers_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/gateway/httpV1/handlers"
	"vpntunnel/internal/infrastructure/observability"
	"vpntunnel/internal/tunnel"
)

// captureRecord holds a single captured slog log record.
type captureRecord struct {
	Level   slog.Level
	Message string
	Attrs   map[string]string
}

// captureHandler is a slog.Handler that accumulates records for inspection.
type captureHandler struct {
	mu      sync.Mutex
	records []captureRecord
}

var _ slog.Handler = (*captureHandler)(nil)

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := captureRecord{
		Level:   r.Level,
		Message: r.Message,
		Attrs:   make(map[string]string),
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Value.Kind() == slog.KindString {
			rec.Attrs[a.Key] = a.Value.String()
		}
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	return h
}

func (h *captureHandler) captured() []captureRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]captureRecord, len(h.records))
	copy(out, h.records)
	return out
}

// reHostPort matches a host:port token (IPv4, IPv6, or hostname with port).
var reHostPort = regexp.MustCompile(`(?:\d{1,3}\.){3}\d{1,3}:\d+|\[[\da-fA-F:]+\]:\d+|[a-zA-Z0-9._-]+:\d+`)

// dialFunc is a tunnel.Dialer backed by a plain function.
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

var _ tunnel.Dialer = (dialFunc)(nil)

func (f dialFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

// countingDialer wraps a tunnel.Dialer and counts DialContext invocations.
type countingDialer struct {
	inner tunnel.Dialer
	count atomic.Int64
}

var _ tunnel.Dialer = (*countingDialer)(nil)

func (c *countingDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c.count.Add(1)
	return c.inner.DialContext(ctx, network, address)
}

// resolverFunc is a tunnel.Resolver backed by a plain function.
type resolverFunc func(ctx context.Context, host string) ([]netip.Addr, error)

var _ tunnel.Resolver = (resolverFunc)(nil)

func (f resolverFunc) LookupHost(ctx context.Context, host string) ([]netip.Addr, error) {
	return f(ctx, host)
}

// publicResolver returns a resolver that always resolves to a public IP,
// bypassing the deny-list.
func publicResolver() resolverFunc {
	return resolverFunc(func(_ context.Context, _ string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("203.0.113.1")}, nil
	})
}

// forwardToServer returns a dialer + resolver pair that routes all connections
// to srv, regardless of the address in the request. The resolver returns a
// public IP so the deny-list passes; the dialer connects to the real server.
func forwardToServer(t *testing.T, srv *httptest.Server) (tunnel.Dialer, tunnel.Resolver) {
	t.Helper()
	srvURL, err := url.Parse(srv.URL)
	require.NoError(t, err)
	dialer := dialFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srvURL.Host)
	})
	return dialer, publicResolver()
}

// buildOutboundRequest constructs an outbound *http.Request aimed at targetURL.
func buildOutboundRequest(t *testing.T, method, targetURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, targetURL, http.NoBody)
	require.NoError(t, err)
	return req
}

// osSyscallErr wraps a syscall.Errno to expose the Timeout/Temporary surface
// that *net.OpError expects from its inner error.
type osSyscallErr struct{ err syscall.Errno }

func (e *osSyscallErr) Error() string   { return e.err.Error() }
func (e *osSyscallErr) Timeout() bool   { return e.err.Timeout() }
func (e *osSyscallErr) Temporary() bool { return e.err.Temporary() } //nolint:staticcheck
func (e *osSyscallErr) Unwrap() error   { return e.err }

// connRefusedErr returns a *net.OpError wrapping ECONNREFUSED.
func connRefusedErr() error {
	return &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: &osSyscallErr{err: syscall.ECONNREFUSED},
	}
}

func TestTunnelForwarder_Forward(t *testing.T) {
	t.Parallel()

	t.Run("upstream_dns_failure_returns_502_upstream_dns_failed", func(t *testing.T) {
		t.Parallel()
		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))

		dnsErr := &net.DNSError{Err: "no such host", Name: "nxdomain.example", IsNotFound: true}
		resolver := resolverFunc(func(_ context.Context, _ string) ([]netip.Addr, error) {
			return nil, dnsErr
		})
		dialer := dialFunc(func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, errors.New("should not be called")
		})

		req := buildOutboundRequest(t, http.MethodGet, "http://nxdomain.example/path")
		w := httptest.NewRecorder()
		err := fwdr.Forward(w, req, "se-sto-wg-001", dialer, resolver)
		require.NoError(t, err, "Forward must not return an error; it writes the response itself")
		assert.Equal(t, http.StatusBadGateway, w.Code)
		assert.Equal(t, "upstream_dns_failed", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("upstream_connect_refused_returns_502_upstream_connect_failed", func(t *testing.T) {
		t.Parallel()
		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))

		dialer := dialFunc(func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, connRefusedErr()
		})

		req := buildOutboundRequest(t, http.MethodGet, "http://refused.example/path")
		w := httptest.NewRecorder()
		err := fwdr.Forward(w, req, "se-sto-wg-001", dialer, publicResolver())
		require.NoError(t, err)
		assert.Equal(t, http.StatusBadGateway, w.Code)
		assert.Equal(t, "upstream_connect_failed", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("upstream_timeout_returns_504_upstream_timeout", func(t *testing.T) {
		t.Parallel()
		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))

		dialer := dialFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})

		// short timeout so the test does not take long.
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, "http://slow.example/path", http.NoBody)
		require.NoError(t, reqErr)

		w := httptest.NewRecorder()
		fwdErr := fwdr.Forward(w, req, "se-sto-wg-001", dialer, publicResolver())
		require.NoError(t, fwdErr)
		assert.Equal(t, http.StatusGatewayTimeout, w.Code)
		assert.Equal(t, "upstream_timeout", w.Header().Get("X-Proxy-Error"))
	})

	t.Run("target_resolves_to_private_ip_returns_403_private_address_denied", func(t *testing.T) {
		t.Parallel()

		neverDialed := dialFunc(func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, errors.New("dialer must not be reached")
		})

		t.Run("via_dns_hostname_resolving_to_private_ip", func(t *testing.T) {
			t.Parallel()
			fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))

			resolver := resolverFunc(func(_ context.Context, _ string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
			})

			req := buildOutboundRequest(t, http.MethodGet, "http://internal.evil.example/secret")
			w := httptest.NewRecorder()
			err := fwdr.Forward(w, req, "se-sto-wg-001", neverDialed, resolver)
			require.NoError(t, err)
			assert.Equal(t, http.StatusForbidden, w.Code)
			assert.Equal(t, "private_address_denied", w.Header().Get("X-Proxy-Error"))
		})

		t.Run("via_literal_private_ip_in_host", func(t *testing.T) {
			t.Parallel()
			fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))

			// resolver must never be called when host is a literal IP.
			resolver := resolverFunc(func(_ context.Context, _ string) ([]netip.Addr, error) {
				return nil, errors.New("resolver must not be called for literal IPs")
			})

			req := buildOutboundRequest(t, http.MethodGet, "http://127.0.0.1/admin")
			w := httptest.NewRecorder()
			err := fwdr.Forward(w, req, "se-sto-wg-001", neverDialed, resolver)
			require.NoError(t, err)
			assert.Equal(t, http.StatusForbidden, w.Code)
			assert.Equal(t, "private_address_denied", w.Header().Get("X-Proxy-Error"))
		})

		t.Run("ipv4_mapped_ipv6_private_via_dns_blocked", func(t *testing.T) {
			t.Parallel()
			fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))

			// ::ffff:10.0.0.1 unmaps to 10.0.0.1 — must be blocked.
			resolver := resolverFunc(func(_ context.Context, _ string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("::ffff:10.0.0.1")}, nil
			})

			req := buildOutboundRequest(t, http.MethodGet, "http://mapped.evil.example/")
			w := httptest.NewRecorder()
			err := fwdr.Forward(w, req, "se-sto-wg-001", neverDialed, resolver)
			require.NoError(t, err)
			assert.Equal(t, http.StatusForbidden, w.Code)
			assert.Equal(t, "private_address_denied", w.Header().Get("X-Proxy-Error"))
		})

		t.Run("via_literal_zero_address_blocked", func(t *testing.T) {
			t.Parallel()
			fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))

			// resolver must never be called when host is a literal IP.
			resolver := resolverFunc(func(_ context.Context, _ string) ([]netip.Addr, error) {
				return nil, errors.New("resolver must not be called for literal IPs")
			})

			// 0.0.0.0 routes to loopback on Linux; the deny-list must catch it.
			req := buildOutboundRequest(t, http.MethodGet, "http://0.0.0.0/secret")
			w := httptest.NewRecorder()
			err := fwdr.Forward(w, req, "se-sto-wg-001", neverDialed, resolver)
			require.NoError(t, err)
			assert.Equal(t, http.StatusForbidden, w.Code)
			assert.Equal(t, "private_address_denied", w.Header().Get("X-Proxy-Error"))
		})
	})

	t.Run("remote_500_passes_through_without_X_Proxy_Error", func(t *testing.T) {
		t.Parallel()

		// real upstream server that returns 500 and sets X-Proxy-Error itself
		// (to verify the forwarder strips it).
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Upstream-Header", "present")
			w.Header().Set("X-Proxy-Error", "upstream-set-this")
			http.Error(w, "server error from upstream", http.StatusInternalServerError)
		}))
		t.Cleanup(upstream.Close)

		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))
		dialer, resolver := forwardToServer(t, upstream)

		req := buildOutboundRequest(t, http.MethodGet, "http://example.com/path")
		w := httptest.NewRecorder()
		err := fwdr.Forward(w, req, "se-sto-wg-001", dialer, resolver)
		require.NoError(t, err)

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		// X-Proxy-Error from upstream must be stripped; it is a daemon-only header.
		assert.Empty(t, w.Header().Get("X-Proxy-Error"),
			"X-Proxy-Error must never be set on upstream-originated responses")
		// the upstream's own custom header passes through.
		assert.Equal(t, "present", w.Header().Get("X-Upstream-Header"))
		assert.Contains(t, w.Body.String(), "server error from upstream")
	})

	t.Run("remote_200_streams_body_verbatim", func(t *testing.T) {
		t.Parallel()

		const body = "hello from upstream — this body must be streamed verbatim to the client"

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(upstream.Close)

		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))
		dialer, resolver := forwardToServer(t, upstream)

		req := buildOutboundRequest(t, http.MethodGet, "http://example.com/body")
		w := httptest.NewRecorder()
		err := fwdr.Forward(w, req, "se-sto-wg-001", dialer, resolver)
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, body, w.Body.String())
		assert.Equal(t, "text/plain; charset=utf-8", w.Header().Get("Content-Type"))
	})

	t.Run("hop_by_hop_headers_stripped_from_upstream_response", func(t *testing.T) {
		t.Parallel()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Custom-Ok", "keep-this")
			w.Header().Set("Keep-Alive", "timeout=5")
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)

		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))
		dialer, resolver := forwardToServer(t, upstream)

		req := buildOutboundRequest(t, http.MethodGet, "http://example.com/")
		w := httptest.NewRecorder()
		err := fwdr.Forward(w, req, "se-sto-wg-001", dialer, resolver)
		require.NoError(t, err)

		assert.Equal(t, "keep-this", w.Header().Get("X-Custom-Ok"))
		assert.Empty(t, w.Header().Get("Keep-Alive"), "hop-by-hop Keep-Alive must be stripped")
	})

	t.Run("transport_cache_reuses_transport_across_requests", func(t *testing.T) {
		t.Parallel()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)

		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))

		upstreamURL, _ := url.Parse(upstream.URL)
		counter := &countingDialer{
			inner: dialFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, upstreamURL.Host)
			}),
		}
		resolver := publicResolver()

		// first request.
		req1 := buildOutboundRequest(t, http.MethodGet, "http://example.com/first")
		w1 := httptest.NewRecorder()
		require.NoError(t, fwdr.Forward(w1, req1, "se-sto-wg-001", counter, resolver))
		assert.Equal(t, http.StatusOK, w1.Code)

		// second request — transport should reuse the idle connection.
		req2 := buildOutboundRequest(t, http.MethodGet, "http://example.com/second")
		w2 := httptest.NewRecorder()
		require.NoError(t, fwdr.Forward(w2, req2, "se-sto-wg-001", counter, resolver))
		assert.Equal(t, http.StatusOK, w2.Code)

		// the transport keeps the TCP connection alive between sequential requests
		// to the same host, so DialContext is called exactly once.
		assert.Equal(t, int64(1), counter.count.Load(),
			"transport must reuse the connection across requests to the same host")
	})

	t.Run("client_disconnect_cancels_upstream", func(t *testing.T) {
		// deliberately not marked parallel — uses a blocking upstream handler
		// and time.Sleep; running concurrently with other long-sleepers would slow CI.

		// upstream that blocks until its request context is cancelled.
		serverCancelled := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
				close(serverCancelled)
			case <-time.After(5 * time.Second):
			}
		}))
		t.Cleanup(upstream.Close)

		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))
		dialer, resolver := forwardToServer(t, upstream)

		ctx, cancel := context.WithCancel(t.Context())

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/slow", http.NoBody)
		require.NoError(t, err)

		forwardDone := make(chan struct{})
		go func() {
			defer close(forwardDone)
			w := httptest.NewRecorder()
			_ = fwdr.Forward(w, req, "se-sto-wg-001", dialer, resolver)
		}()

		// allow the upstream handler to start before disconnecting.
		time.Sleep(50 * time.Millisecond)
		cancel()

		select {
		case <-serverCancelled:
		case <-time.After(3 * time.Second):
			t.Fatal("upstream context was not cancelled within 3s after client disconnect")
		}

		select {
		case <-forwardDone:
		case <-time.After(3 * time.Second):
			t.Fatal("Forward did not return within 3s after client disconnect")
		}
	})
}

// assertNoHostPort checks that no captured log record contains a host:port
// pattern in any string attribute value or in the message itself.
func assertNoHostPort(t *testing.T, records []captureRecord) {
	t.Helper()
	for _, rec := range records {
		if reHostPort.MatchString(rec.Message) {
			t.Errorf("log message contains host:port: %q", rec.Message)
		}
		for k, v := range rec.Attrs {
			if reHostPort.MatchString(v) {
				t.Errorf("log attr %q contains host:port: %q", k, v)
			}
		}
	}
}

// TestTunnelForwarder_MapDoError verifies that mapDoError never leaks
// resolved IPs or host:port tokens into log output, and that it emits
// a controlled operational message for each error shape.
func TestTunnelForwarder_MapDoError(t *testing.T) {
	t.Parallel()

	t.Run("dial timeout emits no host:port in logs", func(t *testing.T) {
		t.Parallel()
		cap := &captureHandler{}
		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(cap))

		dialer := dialFunc(func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, &net.OpError{
				Op:   "dial",
				Net:  "tcp",
				Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080},
				Err:  &osSyscallErr{err: syscall.ETIMEDOUT},
			}
		})

		req := buildOutboundRequest(t, http.MethodGet, "http://example.com/path")
		w := httptest.NewRecorder()
		require.NoError(t, fwdr.Forward(w, req, "t1", dialer, publicResolver()))
		assert.Equal(t, http.StatusBadGateway, w.Code)
		assertNoHostPort(t, cap.captured())
	})

	t.Run("connection refused emits no host:port in logs", func(t *testing.T) {
		t.Parallel()
		cap := &captureHandler{}
		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(cap))

		dialer := dialFunc(func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, &net.OpError{
				Op:   "dial",
				Net:  "tcp",
				Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.1"), Port: 443},
				Err:  &osSyscallErr{err: syscall.ECONNREFUSED},
			}
		})

		req := buildOutboundRequest(t, http.MethodGet, "http://refused.example/path")
		w := httptest.NewRecorder()
		require.NoError(t, fwdr.Forward(w, req, "t1", dialer, publicResolver()))
		assert.Equal(t, http.StatusBadGateway, w.Code)
		assertNoHostPort(t, cap.captured())
	})

	t.Run("tls handshake failure emits no host:port in logs", func(t *testing.T) {
		t.Parallel()
		// spin up a raw TCP listener that sends 5 garbage bytes, causing the TLS
		// client to surface a tls.RecordHeaderError (not the net/http sentinel for
		// "http: server gave HTTP response to HTTPS client").
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })

		go func() {
			conn, acErr := ln.Accept()
			if acErr != nil {
				return
			}
			defer conn.Close()
			// 5 raw garbage bytes produce a tls.RecordHeaderError on the client side.
			_, _ = conn.Write([]byte{0x01, 0x02, 0x03, 0x04, 0x05})
		}()

		lnAddr := ln.Addr().String()
		cap := &captureHandler{}
		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(cap))

		dialer := dialFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, lnAddr)
		})

		req := buildOutboundRequest(t, http.MethodGet, "https://tls.example/path")
		w := httptest.NewRecorder()
		require.NoError(t, fwdr.Forward(w, req, "t1", dialer, publicResolver()))
		assert.Equal(t, http.StatusBadGateway, w.Code)

		records := cap.captured()
		assertNoHostPort(t, records)
		require.Len(t, records, 1)
		assert.Equal(t, "upstream tls handshake failed", records[0].Attrs["op_msg"])
	})

	t.Run("unknown error shape emits no host:port in logs", func(t *testing.T) {
		t.Parallel()
		cap := &captureHandler{}
		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(cap))

		// error message intentionally contains a host:port — must not appear in logs.
		dialer := dialFunc(func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, errors.New("mystery failure at 10.0.0.1:9999")
		})

		req := buildOutboundRequest(t, http.MethodGet, "http://unknown.example/path")
		w := httptest.NewRecorder()
		require.NoError(t, fwdr.Forward(w, req, "t1", dialer, publicResolver()))
		assert.Equal(t, http.StatusBadGateway, w.Code)
		assertNoHostPort(t, cap.captured())
	})

	t.Run("read error (connection closed early) emits no host:port in logs", func(t *testing.T) {
		t.Parallel()
		// server immediately closes the connection after accepting; the HTTP
		// client reads EOF on the response, producing *net.OpError{Op:"read"}.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })

		go func() {
			conn, acErr := ln.Accept()
			if acErr != nil {
				return
			}
			_ = conn.Close()
		}()

		lnAddr := ln.Addr().String()
		cap := &captureHandler{}
		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(cap))

		dialer := dialFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, lnAddr)
		})

		req := buildOutboundRequest(t, http.MethodGet, "http://closedearly.example/path")
		w := httptest.NewRecorder()
		require.NoError(t, fwdr.Forward(w, req, "t1", dialer, publicResolver()))
		assert.Equal(t, http.StatusBadGateway, w.Code)
		assertNoHostPort(t, cap.captured())
	})

	t.Run("client-supplied host:port in URL is scrubbed by layer 2", func(t *testing.T) {
		t.Parallel()
		// layer-1 logs target_host verbatim (required for operational visibility);
		// layer-2 (scrubHandler) must redact it before it reaches the sink.
		// This test wires scrubHandler around captureHandler to prove the contract.
		cap := &captureHandler{}
		scrubbed := observability.NewScrubHandler(cap)
		log := slog.New(scrubbed)

		dialer := dialFunc(func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, errors.New("connection refused")
		})

		// hostname target with explicit port so r.URL.Host == "scrub.example:8443";
		// the deny-list passes (public resolver returns 203.0.113.1), and the
		// dial error triggers the mapDoError log line that carries target_host.
		fwdr := handlers.NewTunnelForwarder(1<<20, log)
		req := buildOutboundRequest(t, http.MethodGet, "http://scrub.example:8443/path")
		w := httptest.NewRecorder()
		require.NoError(t, fwdr.Forward(w, req, "t1", dialer, publicResolver()))

		records := cap.captured()
		require.NotEmpty(t, records, "expected at least one log record")
		for _, rec := range records {
			v, ok := rec.Attrs["target_host"]
			if !ok {
				continue
			}
			assert.Equal(t, "<HOST:PORT>", v,
				"scrubHandler must replace host:port in target_host with <HOST:PORT>")
		}
	})
}

// TestTunnelForwarder_ForwardRaw exercises the materialising path that captures
// the upstream response into an asyncjob.UpstreamResponse. It shares the same
// transport cache, IP deny-list, and hop-by-hop stripping logic as Forward —
// these tests focus on the ForwardRaw-specific behaviour (response materialization
// and error return) rather than re-testing the shared dial paths.
func TestTunnelForwarder_ForwardRaw(t *testing.T) {
	t.Parallel()

	t.Run("200 response is materialised with status, headers, and body", func(t *testing.T) {
		t.Parallel()

		const respBody = "async upstream response body"
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("X-Custom-Header", "keep-me")
			_, _ = io.WriteString(w, respBody)
		}))
		t.Cleanup(upstream.Close)

		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))
		dialer, resolver := forwardToServer(t, upstream)

		req := buildOutboundRequest(t, http.MethodGet, "http://example.com/async")
		resp, err := fwdr.ForwardRaw(t.Context(), req, "t1", dialer, resolver)
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, respBody, string(resp.Body))
		assert.Equal(t, "text/plain", resp.Header.Get("Content-Type"))
		assert.Equal(t, "keep-me", resp.Header.Get("X-Custom-Header"))
	})

	t.Run("hop-by-hop headers are stripped from materialised response", func(t *testing.T) {
		t.Parallel()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Safe", "present")
			w.Header().Set("Keep-Alive", "timeout=5")
			w.Header().Set("Transfer-Encoding", "chunked")
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)

		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))
		dialer, resolver := forwardToServer(t, upstream)

		req := buildOutboundRequest(t, http.MethodGet, "http://example.com/hop")
		resp, err := fwdr.ForwardRaw(t.Context(), req, "t1", dialer, resolver)
		require.NoError(t, err)

		assert.Equal(t, "present", resp.Header.Get("X-Safe"))
		assert.Empty(t, resp.Header.Get("Keep-Alive"), "hop-by-hop Keep-Alive must be stripped")
		assert.Empty(t, resp.Header.Get("Transfer-Encoding"), "hop-by-hop Transfer-Encoding must be stripped")
	})

	t.Run("non-200 status is materialised correctly", func(t *testing.T) {
		t.Parallel()

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		}))
		t.Cleanup(upstream.Close)

		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))
		dialer, resolver := forwardToServer(t, upstream)

		req := buildOutboundRequest(t, http.MethodGet, "http://example.com/missing")
		resp, err := fwdr.ForwardRaw(t.Context(), req, "t1", dialer, resolver)
		require.NoError(t, err)

		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		assert.Contains(t, string(resp.Body), "not found")
	})

	t.Run("dial error returns non-nil error", func(t *testing.T) {
		t.Parallel()

		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))
		dialer := dialFunc(func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, connRefusedErr()
		})

		req := buildOutboundRequest(t, http.MethodGet, "http://refused.example/path")
		resp, err := fwdr.ForwardRaw(t.Context(), req, "t1", dialer, publicResolver())
		require.Error(t, err, "ForwardRaw must return an error on dial failure")
		assert.Zero(t, resp.StatusCode, "StatusCode must be zero on error")
		assert.Nil(t, resp.Body)
	})

	t.Run("private address is denied and returns error", func(t *testing.T) {
		t.Parallel()

		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))
		neverDialed := dialFunc(func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, errors.New("must not be reached")
		})
		privateResolver := resolverFunc(func(_ context.Context, _ string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
		})

		req := buildOutboundRequest(t, http.MethodGet, "http://internal.example/secret")
		resp, err := fwdr.ForwardRaw(t.Context(), req, "t1", neverDialed, privateResolver)
		require.Error(t, err, "ForwardRaw must return error when target resolves to private IP")
		assert.Zero(t, resp.StatusCode)
	})

	t.Run("context cancellation returns error", func(t *testing.T) {
		t.Parallel()

		fwdr := handlers.NewTunnelForwarder(1<<20, slog.New(slog.DiscardHandler))
		dialer := dialFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})

		ctx, cancel := context.WithCancel(t.Context())
		cancel() // cancel immediately

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://slow.example/", http.NoBody)
		require.NoError(t, err)

		resp, fwdErr := fwdr.ForwardRaw(ctx, req, "t1", dialer, publicResolver())
		require.Error(t, fwdErr, "ForwardRaw must return error on context cancellation")
		assert.Zero(t, resp.StatusCode)
	})
}
