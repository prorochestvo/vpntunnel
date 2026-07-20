package notify

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prorochestvo/dsninjector"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSendClientNoProxy(t *testing.T) {
	t.Parallel()
	// the HTTP client handed to go-telegram/bot must never route through a
	// process-wide proxy.
	transport, ok := sendClient.Transport.(*http.Transport)
	require.True(t, ok)
	u, err := transport.Proxy(&http.Request{})
	require.NoError(t, err)
	assert.Nil(t, u)
}

func TestTelegramNotifier_Notify(t *testing.T) {
	t.Parallel()

	t.Run("streaming events are never deduped", func(t *testing.T) {
		t.Parallel()
		srv, calls := newRecordingTelegramServer(t)
		defer srv.Close()

		tn := mustNotifier(t, 1, validToken, testTag, srv.URL, "", fixedClock(time.Now()), discardLogger())
		defer tn.Close()

		tn.Notify(t.Context(), Event{Source: SourceStreaming, Title: "started", Filename: "se-sto-wg-001.conf"})
		tn.Notify(t.Context(), Event{Source: SourceStreaming, Title: "switched tunnel", Filename: "se-sto-wg-001.conf"})

		awaitCalls(t, calls, 2, time.Second)
	})

	t.Run("on-demand same filename within the dedup window is suppressed", func(t *testing.T) {
		t.Parallel()
		srv, calls := newRecordingTelegramServer(t)
		defer srv.Close()

		now := time.Now()
		clk := &manualClock{t: now}
		tn := mustNotifier(t, 1, validToken, testTag, srv.URL, "", clk.now, discardLogger())
		defer tn.Close()

		tn.Notify(t.Context(), Event{Source: SourceOnDemand, Title: "on-demand: se", Filename: "se-sto-wg-001.conf"})
		clk.advance(time.Minute)
		tn.Notify(t.Context(), Event{Source: SourceOnDemand, Title: "on-demand: se", Filename: "se-sto-wg-001.conf"})

		awaitCalls(t, calls, 1, time.Second)
		time.Sleep(50 * time.Millisecond) // ensure a second send does not sneak in
		assert.Equal(t, int32(1), calls.Load())
	})

	t.Run("on-demand different filename within the floor is suppressed", func(t *testing.T) {
		t.Parallel()
		srv, calls := newRecordingTelegramServer(t)
		defer srv.Close()

		now := time.Now()
		clk := &manualClock{t: now}
		tn := mustNotifier(t, 1, validToken, testTag, srv.URL, "", clk.now, discardLogger())
		defer tn.Close()

		tn.Notify(t.Context(), Event{Source: SourceOnDemand, Title: "on-demand: se", Filename: "se-sto-wg-001.conf"})
		clk.advance(5 * time.Second) // within onDemandMinInterval (20s)
		tn.Notify(t.Context(), Event{Source: SourceOnDemand, Title: "on-demand: de", Filename: "de-ber-wg-001.conf"})

		awaitCalls(t, calls, 1, time.Second)

		clk.advance(onDemandMinInterval + time.Second) // past the floor
		tn.Notify(t.Context(), Event{Source: SourceOnDemand, Title: "on-demand: de", Filename: "de-ber-wg-001.conf"})

		awaitCalls(t, calls, 2, time.Second)
	})

	t.Run("probe failure still sends without the exit line", func(t *testing.T) {
		t.Parallel()
		var gotText string
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			gotText = r.FormValue("text")
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
		}))
		defer srv.Close()

		tn := mustNotifier(t, 1, validToken, testTag, srv.URL, "", fixedClock(time.Now()), discardLogger())
		defer tn.Close()

		tn.Notify(t.Context(), Event{
			Source:   SourceStreaming,
			Title:    "started",
			Country:  "se",
			Filename: "se-sto-wg-001.conf",
			Dialer:   &fakeDialer{err: errors.New("tunnel torn down")},
		})

		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return gotText != ""
		}, time.Second, 5*time.Millisecond)

		mu.Lock()
		defer mu.Unlock()
		assert.NotContains(t, gotText, "exit")
		assert.Contains(t, gotText, "se-sto-wg-001.conf")
		assert.Contains(t, gotText, testTag, "the injected app tag must reach the sent message")
	})

	t.Run("telegram 500 does not panic and the sender survives", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		tn := mustNotifier(t, 1, validToken, testTag, srv.URL, "", fixedClock(time.Now()), discardLogger())
		defer tn.Close()

		assert.NotPanics(t, func() {
			tn.Notify(t.Context(), Event{Source: SourceStreaming, Title: "started"})
		})

		// the sender goroutine must still be alive to process a follow-up event.
		time.Sleep(20 * time.Millisecond)
		assert.NotPanics(t, func() {
			tn.Notify(t.Context(), Event{Source: SourceStreaming, Title: "still alive"})
		})
	})
}

func TestTelegramNotifier_SendErrorNeverLogsToken(t *testing.T) {
	t.Parallel()
	// point the bot at a dead address so the send fails with a transport error
	// whose URL embeds the token; go-telegram/bot must redact it before the
	// notifier logs the failure (R15 regression guard).
	lb := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(lb, nil))
	tn := mustNotifier(t, 1, validToken, testTag, "http://127.0.0.1:1", "", fixedClock(time.Now()), logger)
	defer tn.Close()

	tn.Notify(t.Context(), Event{Source: SourceStreaming, Title: "started", Filename: "se-sto-wg-001.conf"})

	require.Eventually(t, func() bool {
		return strings.Contains(lb.String(), "telegram send failed")
	}, 2*time.Second, 10*time.Millisecond)
	assert.NotContains(t, lb.String(), validToken)
	assert.NotContains(t, lb.String(), "AAAAaaaaBBBBbbbbCCCCccccDDDDdddd123")
}

// lockedBuffer is a mutex-guarded write+read buffer for capturing async log
// output from the sender goroutine without racing the test's reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestTelegramNotifier_Close(t *testing.T) {
	t.Parallel()

	t.Run("sender goroutine exits after close", func(t *testing.T) {
		t.Parallel()
		srv, _ := newRecordingTelegramServer(t)
		defer srv.Close()

		tn := mustNotifier(t, 1, validToken, testTag, srv.URL, "", fixedClock(time.Now()), discardLogger())
		tn.Close()

		select {
		case <-tn.done:
		default:
			t.Fatal("sender goroutine did not exit after Close")
		}
	})

	t.Run("second close is a no-op", func(t *testing.T) {
		t.Parallel()
		srv, _ := newRecordingTelegramServer(t)
		defer srv.Close()

		tn := mustNotifier(t, 1, validToken, testTag, srv.URL, "", fixedClock(time.Now()), discardLogger())
		tn.Close()
		assert.NotPanics(t, func() { tn.Close() })
	})

	t.Run("notify after close does not panic", func(t *testing.T) {
		t.Parallel()
		srv, _ := newRecordingTelegramServer(t)
		defer srv.Close()

		tn := mustNotifier(t, 1, validToken, testTag, srv.URL, "", fixedClock(time.Now()), discardLogger())
		tn.Close()

		assert.NotPanics(t, func() {
			tn.Notify(t.Context(), Event{Source: SourceStreaming, Title: "after close"})
		})
	})
}

func TestNewTelegram(t *testing.T) {
	t.Parallel()

	t.Run("enabled log line carries token_len only, never the secret", func(t *testing.T) {
		t.Parallel()
		var buf strings.Builder
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		ds, err := dsninjector.Parse("tbot://987654321:@" + validToken + "/")
		require.NoError(t, err)
		tn, err := NewTelegram(ds, testTag, logger)
		require.NoError(t, err)
		defer tn.Close()

		out := buf.String()
		assert.Contains(t, out, "token_len")
		assert.NotContains(t, out, validToken)
		assert.NotContains(t, out, "987654321")
	})

	t.Run("data source without a valid token returns a safe error", func(t *testing.T) {
		t.Parallel()
		ds, err := dsninjector.Parse("tbot://987654321:@not-a-token/")
		require.NoError(t, err)

		_, err = NewTelegram(ds, testTag, discardLogger())
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "not-a-token")
	})
}

const validToken = "123456789:AAAAaaaaBBBBbbbbCCCCccccDDDDdddd123"

// testTag is a stand-in app identity used by the notifier tests. It differs
// from the production "#VPNTUNNEL" so a message asserting on it proves the tag
// was threaded from the constructor rather than a leftover hardcoded prefix.
const testTag = "#TESTAPP"

// discardLogger returns a slog.Logger that drops everything, for tests that
// don't assert on log output.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// fixedClock returns a now func that always returns t.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// manualClock is a controllable clock seam for dedup-window tests, advanced
// explicitly instead of sleeping.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// mustNotifier builds a TelegramNotifier via the internal constructor and fails
// the test on the (practically unreachable) bot-init error.
func mustNotifier(t *testing.T, adminChatID int64, token, tag, apiBase, probeURL string, now func() time.Time, opLog *slog.Logger) *TelegramNotifier {
	t.Helper()
	tn, err := newTelegramNotifier(adminChatID, token, tag, apiBase, probeURL, now, opLog)
	require.NoError(t, err)
	return tn
}

// newRecordingTelegramServer returns an httptest server that always answers
// with the Bot API success envelope and an atomic counter of how many requests
// it received.
func newRecordingTelegramServer(t *testing.T) (*httptest.Server, *atomicCounter) {
	t.Helper()
	calls := &atomicCounter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	return srv, calls
}

// atomicCounter is a tiny mutex-guarded counter, avoiding an import of
// sync/atomic purely for int32 arithmetic in test helpers.
type atomicCounter struct {
	mu sync.Mutex
	n  int32
}

func (c *atomicCounter) Add(delta int32) {
	c.mu.Lock()
	c.n += delta
	c.mu.Unlock()
}

func (c *atomicCounter) Load() int32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// awaitCalls blocks until calls.Load() >= n or timeout elapses.
func awaitCalls(t *testing.T, calls *atomicCounter, n int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if calls.Load() >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d call(s); got %d", n, calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
