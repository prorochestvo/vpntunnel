package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vpntunnel/internal/egress"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ egress.Dialer = (*fakeDialer)(nil)

// compile-time contract assertions: both Notifier implementations satisfy it.
var _ Notifier = Nop{}
var _ Notifier = (*TelegramNotifier)(nil)

// fakeDialer is an egress.Dialer test double. When err is set, DialContext
// always fails; otherwise it dials address for real over loopback, letting
// tests point it at an httptest.Server without a real WireGuard device.
type fakeDialer struct {
	err error
}

func (f *fakeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if f.err != nil {
		return nil, f.err
	}
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

func TestProbeExitIP(t *testing.T) {
	t.Parallel()

	t.Run("happy path decodes ip, country, and city", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(map[string]string{
				"ip":      "185.213.155.10",
				"country": "Sweden",
				"city":    "Stockholm",
			}))
		}))
		defer srv.Close()

		info, err := probeExitIP(t.Context(), &fakeDialer{}, srv.URL)

		require.NoError(t, err)
		assert.Equal(t, exitInfo{IP: "185.213.155.10", Country: "Sweden", City: "Stockholm"}, info)
	})

	t.Run("dialer error yields non-nil error and zero exitInfo", func(t *testing.T) {
		t.Parallel()
		info, err := probeExitIP(t.Context(), &fakeDialer{err: errors.New("tunnel down")}, "https://am.i.mullvad.net/json")

		require.Error(t, err)
		assert.Equal(t, exitInfo{}, info)
	})

	t.Run("non-200 status yields an error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		info, err := probeExitIP(t.Context(), &fakeDialer{}, srv.URL)

		require.Error(t, err)
		assert.Equal(t, exitInfo{}, info)
	})

	t.Run("server hangs past the deadline returns an error", func(t *testing.T) {
		t.Parallel()
		release := make(chan struct{})
		defer close(release)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()

		// a short-deadline parent ctx caps probeExitIP's internal timeout at
		// this value (context.WithTimeout takes the earlier of the two
		// deadlines), so the test does not wait out the real 5s probeTimeout.
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()

		_, err := probeExitIP(ctx, &fakeDialer{}, srv.URL)

		require.Error(t, err)
	})

	t.Run("body past the limit is truncated and fails to decode", func(t *testing.T) {
		t.Parallel()
		// a body far larger than probeBodyLimit whose JSON only closes past the
		// cap: io.LimitReader stops the decoder at 64 KiB, mid-string, so the
		// decode fails instead of reading the unbounded body. This proves the
		// cap is load-bearing, not decorative.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ip":"1.2.3.4","city":"` + strings.Repeat("a", 200<<10) + `"}`))
		}))
		defer srv.Close()

		info, err := probeExitIP(t.Context(), &fakeDialer{}, srv.URL)

		require.Error(t, err)
		assert.Equal(t, exitInfo{}, info)
	})

	t.Run("nil dialer returns an error without panicking", func(t *testing.T) {
		t.Parallel()
		assert.NotPanics(t, func() {
			_, err := probeExitIP(t.Context(), nil, "https://am.i.mullvad.net/json")
			require.Error(t, err)
		})
	})
}
