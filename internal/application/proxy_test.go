package application_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/application"
	"vpntunnel/internal/infrastructure/config"
	"vpntunnel/internal/infrastructure/observability"
	"vpntunnel/internal/tools/bearerauth"
)

var _ dialer = (*mockDialer)(nil)
var _ application.Verifier = (*mockVerifier)(nil)
var _ application.Verifier = (*bearerauth.BearerVerifier)(nil)
var _ net.Conn = fakeConn{}
var _ net.Addr = fakeAddr{}
var _ http.ResponseWriter = (*fakeHijackWriter)(nil)
var _ http.Hijacker = (*fakeHijackWriter)(nil)

// mockVerifier is a test double for application.Verifier.
type mockVerifier struct {
	verifyFn func(string) bool
}

func (m *mockVerifier) Verify(header string) bool {
	if m.verifyFn != nil {
		return m.verifyFn(header)
	}
	return false
}

// mockDialer is a test double for the dialer port.
type mockDialer struct {
	dialFn func(ctx context.Context, network, address string) (net.Conn, error)
}

func (m *mockDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if m.dialFn != nil {
		return m.dialFn(ctx, network, address)
	}
	return net.Dial(network, address)
}

// lockedBuffer is a bytes.Buffer guarded by a mutex. It satisfies io.Writer
// and is safe for concurrent use, meeting the contract required by
// slog.NewJSONHandler when the underlying writer is shared across goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (lb *lockedBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.buf.Write(p)
}

func (lb *lockedBuffer) String() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.buf.String()
}

// directDialer routes DialContext directly to net.Dial.
func directDialer() *mockDialer {
	return &mockDialer{dialFn: func(ctx context.Context, network, address string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, address)
	}}
}

func newTestService(t *testing.T, d dialer, opts ...func(*application.ProxyServiceOptions)) *application.ProxyService {
	t.Helper()
	o := application.ProxyServiceOptions{
		Dialer:      d,
		DialTimeout: 5 * time.Second,
	}
	for _, fn := range opts {
		fn(&o)
	}
	return application.NewProxyService(o)
}

func TestProxyService_HandleHTTP(t *testing.T) {
	t.Parallel()

	t.Run("forwards GET and copies body", func(t *testing.T) {
		t.Parallel()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, "upstream body")
		}))
		t.Cleanup(upstream.Close)

		svc := newTestService(t, directDialer())
		req := httptest.NewRequest(http.MethodGet, upstream.URL+"/path", nil)
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "upstream body", rr.Body.String())
	})

	t.Run("strips hop-by-hop headers from request", func(t *testing.T) {
		t.Parallel()
		var receivedHeaders http.Header
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedHeaders = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)

		svc := newTestService(t, directDialer())
		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
		for _, h := range []string{
			"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
			"TE", "Trailers", "Transfer-Encoding", "Upgrade",
		} {
			req.Header.Set(h, "value")
		}
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		for _, h := range []string{
			"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
			"TE", "Trailers", "Transfer-Encoding", "Upgrade",
		} {
			assert.Empty(t, receivedHeaders.Get(h), "hop-by-hop header %s should be stripped", h)
		}
	})

	t.Run("strips hop-by-hop headers listed in Connection header", func(t *testing.T) {
		t.Parallel()
		var receivedHeaders http.Header
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedHeaders = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)

		svc := newTestService(t, directDialer())
		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
		req.Header.Set("Connection", "X-Custom")
		req.Header.Set("X-Custom", "some-value")
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Empty(t, receivedHeaders.Get("X-Custom"), "X-Custom should be stripped via Connection header")
	})

	t.Run("rewrites Host header to target", func(t *testing.T) {
		t.Parallel()
		var receivedHost string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedHost = r.Host
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)

		svc := newTestService(t, directDialer())
		req := httptest.NewRequest(http.MethodGet, upstream.URL+"/", nil)
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, upstream.Listener.Addr().String(), receivedHost)
	})

	t.Run("returns 400 PublicError on non-absolute URI", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t, directDialer())
		req := httptest.NewRequest(http.MethodGet, "/relative/path", nil)
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code)
		assert.Contains(t, rr.Body.String(), "Proxy expects absolute-URI request form.")
	})

	t.Run("returns 502 with upstream-unreachable public message when dialer fails", func(t *testing.T) {
		t.Parallel()
		failDialer := &mockDialer{
			dialFn: func(_ context.Context, _, _ string) (net.Conn, error) {
				return nil, errors.New("nope")
			},
		}
		svc := newTestService(t, failDialer)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		t.Cleanup(upstream.Close)

		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusBadGateway, rr.Code)
		assert.Equal(t, "Upstream unreachable.\n", rr.Body.String())
		assert.NotContains(t, rr.Body.String(), application.ErrFallbackMessage, "dial fail must surface as public, not fallback")
	})

	t.Run("propagates client cancellation to upstream", func(t *testing.T) {
		t.Parallel()
		// upstream blocks until ctx is done
		blocking := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-blocking
		}))
		t.Cleanup(func() {
			close(blocking)
			upstream.Close()
		})

		svc := newTestService(t, directDialer())
		ctx, cancel := context.WithCancel(t.Context())
		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil).WithContext(ctx)
		rr := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			defer close(done)
			svc.HandleHTTP(rr, req)
		}()

		time.Sleep(30 * time.Millisecond)
		cancel()
		<-done
		// after ctx cancel, either a 502 or connection error is expected
		assert.NotEqual(t, http.StatusOK, rr.Code)
	})

	t.Run("emits access log line with bytes and status", func(t *testing.T) {
		t.Parallel()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, "data")
		}))
		t.Cleanup(upstream.Close)

		logPath := filepath.Join(t.TempDir(), "access.log")
		al, err := observability.NewAccessLogger(config.AccessLog{
			Path:       logPath,
			MaxSizeMB:  100,
			MaxAgeDays: 14,
			MaxBackups: 7,
			Compress:   false,
		}, nil, nil)
		require.NoError(t, err)

		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Access = al
		})

		req := httptest.NewRequest(http.MethodGet, upstream.URL+"/path", nil)
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)

		// close the logger to flush before reading the file
		require.NoError(t, al.Close())

		raw, err := os.ReadFile(logPath)
		require.NoError(t, err)
		require.NotEmpty(t, raw, "access log file must not be empty")

		var rec map[string]any
		require.NoError(t, json.Unmarshal(raw, &rec), "access log line must be valid JSON")
		assert.Equal(t, "GET", rec["method"])
		assert.Equal(t, float64(http.StatusOK), rec["status_code"])
		assert.NotZero(t, rec["bytes_out"], "bytes_out must be non-zero for a response with body")
		assert.Contains(t, rec["target"], upstream.Listener.Addr().String())
	})

	t.Run("strips Connection-listed headers from upstream response", func(t *testing.T) {
		t.Parallel()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Connection", "X-Internal")
			w.Header().Set("X-Internal", "secret")
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)

		svc := newTestService(t, directDialer())
		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Empty(t, rr.Header().Get("X-Internal"), "custom hop-by-hop from response Connection must be stripped")
		assert.Empty(t, rr.Header().Get("Connection"), "Connection header itself must be stripped")
	})

	t.Run("logTarget sanitises credentials and query from access log", func(t *testing.T) {
		t.Parallel()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)

		logPath := filepath.Join(t.TempDir(), "access.log")
		al, err := observability.NewAccessLogger(config.AccessLog{
			Path:       logPath,
			MaxSizeMB:  100,
			MaxAgeDays: 14,
			MaxBackups: 7,
			Compress:   false,
		}, nil, nil)
		require.NoError(t, err)

		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Access = al
		})

		// build a request whose URL has userinfo and a query string with a secret token
		rawURL := "http://user:pw@" + upstream.Listener.Addr().String() + "/path?api_key=abc"
		req := httptest.NewRequest(http.MethodGet, rawURL, nil)
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		require.NoError(t, al.Close())

		raw, err := os.ReadFile(logPath)
		require.NoError(t, err)
		require.NotEmpty(t, raw)

		var rec map[string]any
		require.NoError(t, json.Unmarshal(raw, &rec))
		target, _ := rec["target"].(string)
		assert.NotContains(t, target, "user:pw", "userinfo must not appear in access log")
		assert.NotContains(t, target, "api_key", "query string must not appear in access log")
		assert.Contains(t, target, upstream.Listener.Addr().String(), "host must still be present")
	})

	t.Run("auth disabled passes through without header", func(t *testing.T) {
		t.Parallel()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, "ok")
		}))
		t.Cleanup(upstream.Close)

		// no Verifier set → auth disabled
		svc := newTestService(t, directDialer())
		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("auth enabled with valid Bearer passes through", func(t *testing.T) {
		t.Parallel()
		const secretToken = "test-secret-valid-xk3m9v"
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, "ok")
		}))
		t.Cleanup(upstream.Close)

		v := bearerauth.NewBearerVerifier(secretToken)
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = v
		})
		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
		req.Header.Set("Proxy-Authorization", "Bearer "+secretToken)
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("auth enabled missing header returns 407 with Proxy-Authenticate", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = &mockVerifier{verifyFn: func(string) bool { return false }}
		})
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusProxyAuthRequired, rr.Code)
		assert.Equal(t, "Proxy authentication required.\n", rr.Body.String())
		assert.Equal(t, `Bearer realm="vpntunnel"`, rr.Header().Get("Proxy-Authenticate"))
	})

	t.Run("auth enabled with Basic scheme returns 407", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = &mockVerifier{verifyFn: func(string) bool { return false }}
		})
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		req.Header.Set("Proxy-Authorization", "Basic dXNlcjpwYXNz")
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusProxyAuthRequired, rr.Code)
		assert.Equal(t, "Proxy authentication required.\n", rr.Body.String())
		assert.Equal(t, `Bearer realm="vpntunnel"`, rr.Header().Get("Proxy-Authenticate"))
	})

	t.Run("auth enabled with wrong token returns 407", func(t *testing.T) {
		t.Parallel()
		const secretToken = "test-secret-wrong-xk3m9v"
		v := bearerauth.NewBearerVerifier(secretToken)
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = v
		})
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		req.Header.Set("Proxy-Authorization", "Bearer wrongtoken")
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusProxyAuthRequired, rr.Code)
		assert.Equal(t, "Proxy authentication required.\n", rr.Body.String())
		assert.Equal(t, `Bearer realm="vpntunnel"`, rr.Header().Get("Proxy-Authenticate"))
	})

	t.Run("auth enabled with malformed header returns 407", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = &mockVerifier{verifyFn: func(string) bool { return false }}
		})
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		req.Header.Set("Proxy-Authorization", "not-bearer-at-all")
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusProxyAuthRequired, rr.Code)
		assert.Equal(t, "Proxy authentication required.\n", rr.Body.String())
		assert.Equal(t, `Bearer realm="vpntunnel"`, rr.Header().Get("Proxy-Authenticate"))
	})

	t.Run("auth success strips Proxy-Authorization before forwarding", func(t *testing.T) {
		t.Parallel()
		const secretToken = "test-secret-strip-xk3m9v"

		var capturedHeaders http.Header
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedHeaders = r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(upstream.Close)

		v := bearerauth.NewBearerVerifier(secretToken)
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = v
		})
		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
		req.Header.Set("Proxy-Authorization", "Bearer "+secretToken)
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Empty(t, capturedHeaders.Get("Proxy-Authorization"),
			"Proxy-Authorization must be stripped from the forwarded request")
	})

	t.Run("auth failure op log never contains token value", func(t *testing.T) {
		t.Parallel()
		const secretToken = "DO-NOT-LOG-THIS-TOKEN-xk3m9v"

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		v := bearerauth.NewBearerVerifier(secretToken)
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = v
			o.OpLog = logger
		})
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		req.Header.Set("Proxy-Authorization", "Bearer wrongattempt")
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusProxyAuthRequired, rr.Code)
		assert.NotContains(t, buf.String(), secretToken,
			"the configured token must never appear in any log output")
		assert.NotContains(t, buf.String(), "wrongattempt",
			"the attempted token must never appear in any log output")
	})

	t.Run("auth failure with no-space Bearer header logs reason=malformed", func(t *testing.T) {
		t.Parallel()
		// "Bearertoken" is one field — no scheme/token split → malformed, not wrong_scheme.
		const secretToken = "auth-malformed-nospace-xk3m9v"

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		v := bearerauth.NewBearerVerifier(secretToken)
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = v
			o.OpLog = logger
		})
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		req.Header.Set("Proxy-Authorization", "Bearertoken")
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusProxyAuthRequired, rr.Code)
		assert.Contains(t, buf.String(), `reason=malformed`,
			"single-field header with no space must be classified as malformed, not wrong_scheme")
	})

	t.Run("auth failure with Bearer trailing-space header logs reason=malformed", func(t *testing.T) {
		t.Parallel()
		// "Bearer " has no token after the scheme — the token field is empty → malformed, not wrong_token.
		const secretToken = "auth-malformed-trailspace-xk3m9v"

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		v := bearerauth.NewBearerVerifier(secretToken)
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = v
			o.OpLog = logger
		})
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		req.Header.Set("Proxy-Authorization", "Bearer ")
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusProxyAuthRequired, rr.Code)
		assert.Contains(t, buf.String(), `reason=malformed`,
			"Bearer with trailing space and no token must be classified as malformed, not wrong_token")
	})

	t.Run("auth enabled bypasses loopback IPv4", func(t *testing.T) {
		t.Parallel()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, "ok")
		}))
		t.Cleanup(upstream.Close)

		var lb lockedBuffer
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = &mockVerifier{verifyFn: func(string) bool {
				t.Fatal("verifier must not be called for loopback client")
				return false
			}}
			o.OpLog = slog.New(slog.NewJSONHandler(&lb, nil))
		})
		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code)
		assert.Contains(t, lb.String(), `"reason":"loopback"`)
	})

	t.Run("auth enabled bypasses loopback IPv6", func(t *testing.T) {
		t.Parallel()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, "ok")
		}))
		t.Cleanup(upstream.Close)

		var lb lockedBuffer
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = &mockVerifier{verifyFn: func(string) bool {
				t.Fatal("verifier must not be called for loopback client")
				return false
			}}
			o.OpLog = slog.New(slog.NewJSONHandler(&lb, nil))
		})
		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
		req.RemoteAddr = "[::1]:12345"
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code)
		assert.Contains(t, lb.String(), `"reason":"loopback"`)
	})

	t.Run("auth enabled rejects non-loopback", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = &mockVerifier{verifyFn: func(string) bool { return false }}
		})
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		req.RemoteAddr = "192.0.2.4:12345"
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusProxyAuthRequired, rr.Code)
		assert.Equal(t, `Bearer realm="vpntunnel"`, rr.Header().Get("Proxy-Authenticate"))
	})

	t.Run("auth enabled fails closed on empty RemoteAddr", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = &mockVerifier{verifyFn: func(string) bool { return false }}
		})
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		req.RemoteAddr = ""
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		assert.Equal(t, http.StatusProxyAuthRequired, rr.Code)
		assert.Equal(t, `Bearer realm="vpntunnel"`, rr.Header().Get("Proxy-Authenticate"))
	})
}

func TestProxyService_HandleCONNECT(t *testing.T) {
	t.Parallel()

	t.Run("establishes tunnel and copies bytes both ways", func(t *testing.T) {
		t.Parallel()
		// echo server on the "target"
		echoLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = echoLn.Close() })
		go func() {
			for {
				conn, err := echoLn.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer func() { _ = c.Close() }()
					_, _ = io.Copy(c, c)
				}(conn)
			}
		}()

		svc := newTestService(t, directDialer())

		// build a real HTTP server wrapping the service
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				svc.HandleCONNECT(w, r)
				return
			}
			svc.HandleHTTP(w, r)
		}))
		t.Cleanup(proxyServer.Close)

		// manually build a CONNECT request over a raw TCP conn
		conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		target := echoLn.Addr().String()
		_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		require.NoError(t, err)

		// read the 200 response
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		_ = resp.Body.Close()

		// send data and expect echo
		_, err = fmt.Fprint(conn, "hello tunnel")
		require.NoError(t, err)
		require.NoError(t, conn.(*net.TCPConn).CloseWrite())

		got, err := io.ReadAll(conn)
		require.NoError(t, err)
		assert.Equal(t, "hello tunnel", string(got))
	})

	t.Run("writes exact CONNECT response line with no extra headers", func(t *testing.T) {
		t.Parallel()
		echoLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = echoLn.Close() })
		go func() {
			conn, err := echoLn.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = io.Copy(conn, conn)
		}()

		svc := newTestService(t, directDialer())
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc.HandleCONNECT(w, r)
		}))
		t.Cleanup(proxyServer.Close)

		conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		target := echoLn.Addr().String()
		_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		require.NoError(t, err)

		// read exactly the expected bytes — no Date, no Server, just the line
		buf := make([]byte, len("HTTP/1.1 200 Connection established\r\n\r\n"))
		_, err = io.ReadFull(conn, buf)
		require.NoError(t, err)
		assert.Equal(t, []byte("HTTP/1.1 200 Connection established\r\n\r\n"), buf)
	})

	t.Run("rejects bad host:port with 400 PublicError", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t, directDialer())
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc.HandleCONNECT(w, r)
		}))
		t.Cleanup(proxyServer.Close)

		req, err := http.NewRequest(http.MethodConnect, proxyServer.URL, nil)
		require.NoError(t, err)
		req.Host = "notahostport"

		resp, err := proxyServer.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Contains(t, string(body), "Invalid CONNECT target")
	})

	t.Run("returns 502 with upstream-unreachable public message when dial fails", func(t *testing.T) {
		t.Parallel()
		failDialer := &mockDialer{
			dialFn: func(_ context.Context, _, _ string) (net.Conn, error) {
				return nil, errors.New("dial refused")
			},
		}
		svc := newTestService(t, failDialer)
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc.HandleCONNECT(w, r)
		}))
		t.Cleanup(proxyServer.Close)

		req, err := http.NewRequest(http.MethodConnect, proxyServer.URL, nil)
		require.NoError(t, err)
		req.Host = "example.com:443"

		resp, err := proxyServer.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "Upstream unreachable.\n", string(body))
	})

	t.Run("WaitTunnels returns when all tunnels close", func(t *testing.T) {
		t.Parallel()
		echoLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = echoLn.Close() })
		go func() {
			for {
				conn, err := echoLn.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer func() { _ = c.Close() }()
					_, _ = io.Copy(c, c)
				}(conn)
			}
		}()

		svc := newTestService(t, directDialer())
		// handlerDone signals when HandleCONNECT has returned (including Add(1)).
		handlerDone := make(chan struct{}, 1)
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc.HandleCONNECT(w, r)
			handlerDone <- struct{}{}
		}))

		conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
		require.NoError(t, err)
		target := echoLn.Addr().String()
		_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		require.NoError(t, err)
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		_ = resp.Body.Close()

		// wait for HandleCONNECT to return (and thus Add(1) to be called)
		// before calling WaitTunnels so there is no Add/Wait race.
		select {
		case <-handlerDone:
		case <-time.After(3 * time.Second):
			t.Fatal("HandleCONNECT did not return in time")
		}
		proxyServer.Close()

		// close client side so tunnel drains
		_ = conn.(*net.TCPConn).CloseWrite()
		// give goroutine time to finish
		time.Sleep(50 * time.Millisecond)
		_ = conn.Close()

		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		assert.NoError(t, svc.WaitTunnels(ctx))
	})

	t.Run("WaitTunnels returns ctx error on timeout", func(t *testing.T) {
		t.Parallel()
		// target that never closes
		holdLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = holdLn.Close() })

		var tunnelConn net.Conn
		var mu sync.Mutex
		go func() {
			c, err := holdLn.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			tunnelConn = c
			mu.Unlock()
			time.Sleep(10 * time.Second)
			_ = c.Close()
		}()

		svc := newTestService(t, directDialer())
		handlerDone := make(chan struct{}, 1)
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc.HandleCONNECT(w, r)
			handlerDone <- struct{}{}
		}))

		conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		target := holdLn.Addr().String()
		_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		require.NoError(t, err)
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		_ = resp.Body.Close()

		// wait for HandleCONNECT to return (and thus Add(1) to be called)
		// before calling WaitTunnels so there is no Add/Wait race.
		select {
		case <-handlerDone:
		case <-time.After(3 * time.Second):
			t.Fatal("HandleCONNECT did not return in time")
		}
		proxyServer.Close()

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		err = svc.WaitTunnels(ctx)
		assert.ErrorIs(t, err, context.DeadlineExceeded)

		// cleanup: close the hold conn so the goroutine exits
		mu.Lock()
		if tunnelConn != nil {
			_ = tunnelConn.Close()
		}
		mu.Unlock()
	})

	t.Run("clears deadlines on hijacked conn", func(t *testing.T) {
		t.Parallel()
		// set a very short ReadHeaderTimeout on the server and verify the
		// tunnel survives beyond it — proving deadlines were cleared.
		echoLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = echoLn.Close() })
		go func() {
			for {
				conn, err := echoLn.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer func() { _ = c.Close() }()
					_, _ = io.Copy(c, c)
				}(conn)
			}
		}()

		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.DialTimeout = 5 * time.Second
		})

		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				svc.HandleCONNECT(w, r)
			}),
			ReadHeaderTimeout: 50 * time.Millisecond,
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = srv.Close() })
		go func() { _ = srv.Serve(ln) }()

		conn, err := net.Dial("tcp", ln.Addr().String())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		target := echoLn.Addr().String()
		_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		require.NoError(t, err)

		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		_ = resp.Body.Close()

		// wait longer than ReadHeaderTimeout, then verify the tunnel still works
		time.Sleep(100 * time.Millisecond)

		_, err = fmt.Fprint(conn, "ping")
		require.NoError(t, err, "tunnel should survive past ReadHeaderTimeout")

		buf := make([]byte, 4)
		require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
		_, err = io.ReadFull(conn, buf)
		require.NoError(t, err)
		assert.Equal(t, "ping", string(buf))
	})

	t.Run("auth disabled passes through without header", func(t *testing.T) {
		t.Parallel()
		echoLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = echoLn.Close() })
		go func() {
			conn, err := echoLn.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = io.Copy(conn, conn)
		}()

		// no Verifier → auth disabled, CONNECT should succeed with no header
		svc := newTestService(t, directDialer())
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc.HandleCONNECT(w, r)
		}))
		t.Cleanup(proxyServer.Close)

		target := echoLn.Addr().String()
		conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		require.NoError(t, err)

		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("auth enabled with valid Bearer establishes tunnel", func(t *testing.T) {
		t.Parallel()
		const secretToken = "connect-secret-xk3m9v"

		echoLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = echoLn.Close() })
		go func() {
			conn, err := echoLn.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = io.Copy(conn, conn)
		}()

		v := bearerauth.NewBearerVerifier(secretToken)
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = v
		})
		// force non-loopback so the loopback bypass does not fire and the token
		// check is the actual code path under test.
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = "192.0.2.4:12345"
			svc.HandleCONNECT(w, r)
		}))
		t.Cleanup(proxyServer.Close)

		target := echoLn.Addr().String()
		conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		_, err = fmt.Fprintf(conn,
			"CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Bearer %s\r\n\r\n",
			target, target, secretToken)
		require.NoError(t, err)

		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("auth enabled missing header returns 407 without hijacking", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = &mockVerifier{verifyFn: func(string) bool { return false }}
		})
		// override RemoteAddr to non-loopback so the bypass does not fire
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = "192.0.2.4:12345"
			svc.HandleCONNECT(w, r)
		}))
		t.Cleanup(proxyServer.Close)

		req, err := http.NewRequest(http.MethodConnect, proxyServer.URL, nil)
		require.NoError(t, err)
		req.Host = "example.com:443"

		resp, err := proxyServer.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusProxyAuthRequired, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "Proxy authentication required.\n", string(body))
		assert.Equal(t, `Bearer realm="vpntunnel"`, resp.Header.Get("Proxy-Authenticate"))
	})

	t.Run("auth enabled with wrong token returns 407 without hijacking", func(t *testing.T) {
		t.Parallel()
		const secretToken = "connect-wrong-xk3m9v"
		v := bearerauth.NewBearerVerifier(secretToken)
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = v
		})
		// override RemoteAddr to non-loopback so the bypass does not fire
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = "192.0.2.4:12345"
			svc.HandleCONNECT(w, r)
		}))
		t.Cleanup(proxyServer.Close)

		req, err := http.NewRequest(http.MethodConnect, proxyServer.URL, nil)
		require.NoError(t, err)
		req.Host = "example.com:443"
		req.Header.Set("Proxy-Authorization", "Bearer wrongtoken")

		resp, err := proxyServer.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusProxyAuthRequired, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "Proxy authentication required.\n", string(body))
		assert.Equal(t, `Bearer realm="vpntunnel"`, resp.Header.Get("Proxy-Authenticate"))
	})

	t.Run("auth enabled with Basic scheme returns 407 without hijacking", func(t *testing.T) {
		t.Parallel()
		const secretToken = "connect-basic-scheme-xk3m9v"
		v := bearerauth.NewBearerVerifier(secretToken)
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = v
		})
		// override RemoteAddr to non-loopback so the bypass does not fire
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = "192.0.2.4:12345"
			svc.HandleCONNECT(w, r)
		}))
		t.Cleanup(proxyServer.Close)

		req, err := http.NewRequest(http.MethodConnect, proxyServer.URL, nil)
		require.NoError(t, err)
		req.Host = "example.com:443"
		req.Header.Set("Proxy-Authorization", "Basic dXNlcjpwYXNz")

		resp, err := proxyServer.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusProxyAuthRequired, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "Proxy authentication required.\n", string(body))
		assert.Equal(t, `Bearer realm="vpntunnel"`, resp.Header.Get("Proxy-Authenticate"))
	})

	t.Run("auth failure op log never contains token value for CONNECT", func(t *testing.T) {
		t.Parallel()
		const secretToken = "DO-NOT-LOG-THIS-CONNECT-TOKEN-xk3m9v"

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		v := bearerauth.NewBearerVerifier(secretToken)
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = v
			o.OpLog = logger
		})
		// override RemoteAddr to non-loopback so the bypass does not fire
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = "192.0.2.4:12345"
			svc.HandleCONNECT(w, r)
		}))
		t.Cleanup(proxyServer.Close)

		req, err := http.NewRequest(http.MethodConnect, proxyServer.URL, nil)
		require.NoError(t, err)
		req.Host = "example.com:443"
		req.Header.Set("Proxy-Authorization", "Bearer connectwrongattempt")

		resp, err := proxyServer.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusProxyAuthRequired, resp.StatusCode)
		assert.NotContains(t, buf.String(), secretToken,
			"the configured token must never appear in any log output")
		assert.NotContains(t, buf.String(), "connectwrongattempt",
			"the attempted token must never appear in any log output")
	})

	t.Run("auth enabled bypasses loopback", func(t *testing.T) {
		t.Parallel()
		echoLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = echoLn.Close() })
		go func() {
			conn, err := echoLn.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = io.Copy(conn, conn)
		}()

		var lb lockedBuffer
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = &mockVerifier{verifyFn: func(string) bool {
				t.Fatal("verifier must not be called for loopback client")
				return false
			}}
			o.OpLog = slog.New(slog.NewJSONHandler(&lb, nil))
		})
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc.HandleCONNECT(w, r)
		}))
		t.Cleanup(proxyServer.Close)

		target := echoLn.Addr().String()
		conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		// the proxy server loopback address means RemoteAddr will be 127.0.0.1:port
		_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		require.NoError(t, err)

		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, lb.String(), `"reason":"loopback"`)
	})

	t.Run("auth enabled rejects non-loopback", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t, directDialer(), func(o *application.ProxyServiceOptions) {
			o.Verifier = &mockVerifier{verifyFn: func(string) bool { return false }}
		})
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// override RemoteAddr before the handler sees it
			r.RemoteAddr = "192.0.2.4:12345"
			svc.HandleCONNECT(w, r)
		}))
		t.Cleanup(proxyServer.Close)

		req, err := http.NewRequest(http.MethodConnect, proxyServer.URL, nil)
		require.NoError(t, err)
		req.Host = "example.com:443"

		resp, err := proxyServer.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()

		assert.Equal(t, http.StatusProxyAuthRequired, resp.StatusCode)
		assert.Equal(t, `Bearer realm="vpntunnel"`, resp.Header.Get("Proxy-Authenticate"))
	})
}

// fakeConn is a minimal net.Conn double used by the CONNECT error-path
// ActiveSessions tests. Read always returns io.EOF; Write returns writeErr
// (nil unless configured). It serves two roles: as an upstream dial result
// (writeErr unset — only Close is ever called on it) and as the hijacked
// client conn for the post-hijack write/flush failure branches (writeErr set).
type fakeConn struct {
	writeErr error
}

func (fakeConn) Read([]byte) (int, error) { return 0, io.EOF }

func (c fakeConn) Write(b []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return len(b), nil
}

func (fakeConn) Close() error                     { return nil }
func (fakeConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (fakeConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

// fakeAddr is a minimal net.Addr double for fakeConn.
type fakeAddr struct{}

func (fakeAddr) Network() string { return "tcp" }
func (fakeAddr) String() string  { return "fake:0" }

// fakeHijackWriter is a minimal http.ResponseWriter + http.Hijacker double
// that drives the CONNECT hijack/write/flush failure branches deterministically,
// without a real TCP listener. Header/Write/WriteHeader satisfy
// http.ResponseWriter but are never exercised once Hijack is called — the
// production code never touches w again after a successful hijack.
type fakeHijackWriter struct {
	header   http.Header
	hijackFn func() (net.Conn, *bufio.ReadWriter, error)
}

func newFakeHijackWriter(hijackFn func() (net.Conn, *bufio.ReadWriter, error)) *fakeHijackWriter {
	return &fakeHijackWriter{header: make(http.Header), hijackFn: hijackFn}
}

func (f *fakeHijackWriter) Header() http.Header         { return f.header }
func (f *fakeHijackWriter) Write(b []byte) (int, error) { return len(b), nil }
func (f *fakeHijackWriter) WriteHeader(int)             {}

func (f *fakeHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return f.hijackFn()
}

// dialToFakeConn returns a mockDialer whose DialContext always succeeds with
// a fakeConn, for CONNECT branches that need a successful dial before failing
// later (no hijacker, hijack failure, write/flush failure).
func dialToFakeConn() *mockDialer {
	return &mockDialer{dialFn: func(_ context.Context, _, _ string) (net.Conn, error) {
		return fakeConn{}, nil
	}}
}

// TestProxyService_ActiveSessions drives every HandleHTTP/HandleCONNECT path
// (success, each error branch, and a mid-flight snapshot) and asserts the
// gauge contract documented on ActiveSessions: it is 1 while a session is in
// flight and returns to exactly 0 on every path, including every CONNECT
// error branch between the increment and the tunnel-goroutine handoff.
func TestProxyService_ActiveSessions(t *testing.T) {
	t.Parallel()

	t.Run("HTTP success returns gauge to 0", func(t *testing.T) {
		t.Parallel()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, "ok")
		}))
		t.Cleanup(upstream.Close)

		svc := newTestService(t, directDialer())
		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil)
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		require.Equal(t, http.StatusOK, rr.Code)
		assert.Zero(t, svc.ActiveSessions())
	})

	t.Run("HTTP non-absolute-URI early return never moves the gauge", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t, directDialer())
		req := httptest.NewRequest(http.MethodGet, "/relative/path", nil)
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		require.Equal(t, http.StatusBadRequest, rr.Code)
		assert.Zero(t, svc.ActiveSessions())
	})

	t.Run("HTTP upstream error returns gauge to 0", func(t *testing.T) {
		t.Parallel()
		failDialer := &mockDialer{dialFn: func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, errors.New("nope")
		}}
		svc := newTestService(t, failDialer)
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		rr := httptest.NewRecorder()
		svc.HandleHTTP(rr, req)

		require.Equal(t, http.StatusBadGateway, rr.Code)
		assert.Zero(t, svc.ActiveSessions())
	})

	t.Run("HTTP mid-flight gauge reads 1 while dial is blocked", func(t *testing.T) {
		t.Parallel()
		release := make(chan struct{})
		blockDialer := &mockDialer{dialFn: func(ctx context.Context, _, _ string) (net.Conn, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, errors.New("released")
		}}
		svc := newTestService(t, blockDialer)
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		rr := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			defer close(done)
			svc.HandleHTTP(rr, req)
		}()

		require.Eventually(t, func() bool { return svc.ActiveSessions() == 1 }, time.Second, time.Millisecond,
			"gauge must read 1 while the dial is blocked")
		close(release)
		<-done
		assert.Zero(t, svc.ActiveSessions())
	})

	t.Run("CONNECT happy path returns gauge to 0 only after the tunnel goroutine finishes", func(t *testing.T) {
		t.Parallel()
		echoLn, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = echoLn.Close() })
		go func() {
			for {
				conn, err := echoLn.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer func() { _ = c.Close() }()
					_, _ = io.Copy(c, c)
				}(conn)
			}
		}()

		svc := newTestService(t, directDialer())
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc.HandleCONNECT(w, r)
		}))
		t.Cleanup(proxyServer.Close)

		conn, err := net.Dial("tcp", proxyServer.Listener.Addr().String())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		target := echoLn.Addr().String()
		_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		require.NoError(t, err)

		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		_ = resp.Body.Close()

		// the handler has already returned (200 written) but the tunnel
		// goroutine still owns the session — the gauge must still read 1.
		assert.Equal(t, int64(1), svc.ActiveSessions(),
			"gauge must stay 1 while the tunnel goroutine is still copying")

		require.NoError(t, conn.(*net.TCPConn).CloseWrite())
		_, _ = io.ReadAll(conn)
		_ = conn.Close()

		require.Eventually(t, func() bool { return svc.ActiveSessions() == 0 }, 2*time.Second, 5*time.Millisecond,
			"gauge must return to 0 once both conns are closed by the tunnel goroutine")
	})

	t.Run("CONNECT dial failure returns gauge to 0", func(t *testing.T) {
		t.Parallel()
		failDialer := &mockDialer{dialFn: func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, errors.New("dial refused")
		}}
		svc := newTestService(t, failDialer)
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			svc.HandleCONNECT(w, r)
		}))
		t.Cleanup(proxyServer.Close)

		req, err := http.NewRequest(http.MethodConnect, proxyServer.URL, nil)
		require.NoError(t, err)
		req.Host = "example.com:443"

		resp, err := proxyServer.Client().Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
		assert.Zero(t, svc.ActiveSessions())
	})

	t.Run("CONNECT no hijacker support returns gauge to 0", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t, dialToFakeConn())
		req := httptest.NewRequest(http.MethodConnect, "http://example.com/", nil)
		req.Host = "example.com:443"
		rr := httptest.NewRecorder() // *httptest.ResponseRecorder does not implement http.Hijacker

		svc.HandleCONNECT(rr, req)

		assert.Equal(t, http.StatusInternalServerError, rr.Code)
		assert.Zero(t, svc.ActiveSessions())
	})

	t.Run("CONNECT hijack failure returns gauge to 0", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t, dialToFakeConn())
		req := httptest.NewRequest(http.MethodConnect, "http://example.com/", nil)
		req.Host = "example.com:443"
		w := newFakeHijackWriter(func() (net.Conn, *bufio.ReadWriter, error) {
			return nil, nil, errors.New("hijack refused")
		})

		svc.HandleCONNECT(w, req)

		assert.Zero(t, svc.ActiveSessions())
	})

	t.Run("CONNECT post-hijack write failure returns gauge to 0", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t, dialToFakeConn())
		req := httptest.NewRequest(http.MethodConnect, "http://example.com/", nil)
		req.Host = "example.com:443"
		client := fakeConn{writeErr: errors.New("write refused")}
		w := newFakeHijackWriter(func() (net.Conn, *bufio.ReadWriter, error) {
			// a 1-byte write buffer forces the 200-response write to reach the
			// underlying (failing) conn immediately, inside fmt.Fprint itself.
			bw := bufio.NewWriterSize(client, 1)
			return client, bufio.NewReadWriter(bufio.NewReader(client), bw), nil
		})

		svc.HandleCONNECT(w, req)

		assert.Zero(t, svc.ActiveSessions())
	})

	t.Run("CONNECT post-hijack flush failure returns gauge to 0", func(t *testing.T) {
		t.Parallel()
		svc := newTestService(t, dialToFakeConn())
		req := httptest.NewRequest(http.MethodConnect, "http://example.com/", nil)
		req.Host = "example.com:443"
		client := fakeConn{writeErr: errors.New("write refused")}
		w := newFakeHijackWriter(func() (net.Conn, *bufio.ReadWriter, error) {
			// the default-size write buffer holds the whole 200 response, so
			// fmt.Fprint succeeds (buffered) and only the explicit Flush call
			// reaches the underlying (failing) conn.
			return client, bufio.NewReadWriter(bufio.NewReader(client), bufio.NewWriter(client)), nil
		})

		svc.HandleCONNECT(w, req)

		assert.Zero(t, svc.ActiveSessions())
	})
}

func TestIsLoopbackRemote(t *testing.T) {
	t.Parallel()

	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:12345", true},
		{"127.0.0.1:0", true},
		{"[::1]:12345", true},
		{"[::ffff:127.0.0.1]:80", true},
		{"192.0.2.4:12345", false},
		{"", false},
		{"garbage", false},
		{"127.0.0.1", false},       // no port — SplitHostPort error
		{"127.0.0.1.:1234", false}, // trailing dot — ParseIP returns nil
		{"notanip:1234", false},    // not an IP address
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.addr, func(t *testing.T) {
			t.Parallel()
			got := application.IsLoopbackRemote(tc.addr)
			assert.Equal(t, tc.want, got, "addr=%q", tc.addr)
		})
	}
}

// dialer mirrors the unexported outbound-connection port application declares
// for ProxyServiceOptions.Dialer. It is a test-local copy because the
// production contract is unexported and this is an external test package;
// assignment into the field is structural, so the two only need matching
// method sets.
type dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}
