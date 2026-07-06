package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/observability"
)

// decodeRecord decodes a single JSON log line emitted by slog.JSONHandler into
// a flat map. The buf must contain exactly one JSON object.
func decodeRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	require.NotEmpty(t, line, "no log output was written")
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &m), "invalid JSON in log output")
	return m
}

// newJSONLogger builds a *slog.Logger that writes JSON to buf through a
// scrubHandler. Returns the logger and the buffer.
func newJSONLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := observability.NewScrubHandler(inner)
	return slog.New(h), &buf
}

// peerAddr is a test helper that implements slog.LogValuer, resolving to a
// host:port string. Used to verify that scrubAttr resolves LogValuer before
// inspecting kind.
type peerAddr struct{ addr string }

func (p peerAddr) LogValue() slog.Value { return slog.StringValue(p.addr) }

func TestScrubHandler_Handle(t *testing.T) {
	t.Parallel()

	t.Run("IPv4:port in message is redacted", func(t *testing.T) {
		t.Parallel()
		logger, buf := newJSONLogger(t)
		logger.Info("dial 192.168.1.100:8443 failed")
		rec := decodeRecord(t, buf)
		assert.Equal(t, "dial <HOST:PORT> failed", rec["msg"])
	})

	t.Run("[IPv6]:port in message is redacted", func(t *testing.T) {
		t.Parallel()
		logger, buf := newJSONLogger(t)
		logger.Info("dial [fe80::1]:443 failed")
		rec := decodeRecord(t, buf)
		assert.Equal(t, "dial <HOST:PORT> failed", rec["msg"])
	})

	t.Run("[IPv6]:port in string attribute is redacted", func(t *testing.T) {
		t.Parallel()
		logger, buf := newJSONLogger(t)
		logger.Info("connection", slog.String("peer", "[2001:db8::1]:8443"))
		rec := decodeRecord(t, buf)
		assert.Equal(t, "<HOST:PORT>", rec["peer"])
	})

	t.Run("hostname.tld:port in message is redacted", func(t *testing.T) {
		t.Parallel()
		logger, buf := newJSONLogger(t)
		logger.Info("dial example.com:443 failed")
		rec := decodeRecord(t, buf)
		assert.Equal(t, "dial <HOST:PORT> failed", rec["msg"])
	})

	t.Run("IPv4:port in string attribute is redacted", func(t *testing.T) {
		t.Parallel()
		logger, buf := newJSONLogger(t)
		logger.Info("connection", slog.String("host", "10.0.0.1:80"))
		rec := decodeRecord(t, buf)
		assert.Equal(t, "<HOST:PORT>", rec["host"])
	})

	t.Run("URL path component with ext:line not redacted", func(t *testing.T) {
		t.Parallel()
		logger, buf := newJSONLogger(t)
		logger.Info("error at /v1/api/users.json:5")
		rec := decodeRecord(t, buf)
		assert.Equal(t, "error at /v1/api/users.json:5", rec["msg"])
	})

	t.Run("localhost:port in message is redacted", func(t *testing.T) {
		t.Parallel()
		logger, buf := newJSONLogger(t)
		logger.Info("listen tcp localhost:8888 failed")
		rec := decodeRecord(t, buf)
		assert.Equal(t, "listen tcp <HOST:PORT> failed", rec["msg"])
	})

	t.Run("stdlib dial error shape is fully redacted", func(t *testing.T) {
		t.Parallel()
		logger, buf := newJSONLogger(t)
		logger.Error("upstream error", slog.String("error", "dial tcp 203.0.113.1:443: i/o timeout"))
		rec := decodeRecord(t, buf)
		assert.Equal(t, "dial tcp <HOST:PORT>: i/o timeout", rec["error"])
	})

	t.Run("LogValuer attribute resolved before scrubbing", func(t *testing.T) {
		t.Parallel()
		logger, buf := newJSONLogger(t)
		logger.Info("connected", slog.Any("peer", peerAddr{"10.0.0.1:443"}))
		rec := decodeRecord(t, buf)
		assert.Equal(t, "<HOST:PORT>", rec["peer"])
	})

	t.Run("nested group attribute scrubbed recursively", func(t *testing.T) {
		t.Parallel()
		logger, buf := newJSONLogger(t)
		logger.Info("upstream dial",
			slog.Group("upstream",
				slog.String("host", "example.com:443"),
			),
		)
		rec := decodeRecord(t, buf)
		// slog JSON handler encodes groups as nested JSON objects, not "group.key".
		upstreamRaw, ok := rec["upstream"]
		require.True(t, ok, "expected 'upstream' key in JSON output")
		upstream, ok := upstreamRaw.(map[string]any)
		require.True(t, ok, "expected 'upstream' to be a JSON object")
		assert.Equal(t, "<HOST:PORT>", upstream["host"], "group attribute must be scrubbed")
	})

	t.Run("non-string attribute pass-through unchanged", func(t *testing.T) {
		t.Parallel()
		logger, buf := newJSONLogger(t)
		dur := 42 * time.Millisecond
		logger.Info("metrics",
			slog.Int("count", 7),
			slog.Duration("latency", dur),
			slog.Bool("ok", true),
		)
		rec := decodeRecord(t, buf)
		// JSON numbers: int and float64 are valid; assert the values survive.
		assert.EqualValues(t, 7, rec["count"])
		assert.True(t, rec["ok"].(bool))
		// duration is emitted as nanoseconds (int64) by the JSON handler.
		assert.EqualValues(t, dur.Nanoseconds(), rec["latency"])
	})

	t.Run("non-matching string left untouched", func(t *testing.T) {
		t.Parallel()
		logger, buf := newJSONLogger(t)
		logger.Info("hello world", slog.String("note", "ok"))
		rec := decodeRecord(t, buf)
		assert.Equal(t, "hello world", rec["msg"])
		assert.Equal(t, "ok", rec["note"])
	})

	t.Run("hostname without port left untouched", func(t *testing.T) {
		t.Parallel()
		logger, buf := newJSONLogger(t)
		logger.Info("resolved", slog.String("host", "example.com"))
		rec := decodeRecord(t, buf)
		assert.Equal(t, "example.com", rec["host"])
	})
}

func TestScrubHandler_WithAttrs(t *testing.T) {
	t.Parallel()

	t.Run("bound attrs scrubbed at WithAttrs time", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
		h := observability.NewScrubHandler(inner)

		// Bind a host:port attribute via WithAttrs before creating the logger.
		bound := h.WithAttrs([]slog.Attr{slog.String("host", "1.2.3.4:80")})
		logger := slog.New(bound)
		logger.Info("connected")

		rec := decodeRecord(t, &buf)
		assert.Equal(t, "<HOST:PORT>", rec["host"], "WithAttrs must pre-scrub bound attributes")
	})
}

func TestScrubHandler_WithGroup(t *testing.T) {
	t.Parallel()

	t.Run("WithGroup preserves chaining", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
		h := observability.NewScrubHandler(inner)

		logger := slog.New(h.WithGroup("upstream"))
		logger.Info("dial", slog.String("host", "backend.example.com:9090"))

		rec := decodeRecord(t, &buf)
		// slog JSON handler encodes WithGroup as a nested JSON object, not "group.key".
		upstreamRaw, ok := rec["upstream"]
		require.True(t, ok, "expected 'upstream' key in JSON output")
		upstream, ok := upstreamRaw.(map[string]any)
		require.True(t, ok, "expected 'upstream' to be a JSON object")
		assert.Equal(t, "<HOST:PORT>", upstream["host"],
			"WithGroup chaining must preserve nesting and still scrub string attrs")
	})
}

func TestScrubHandler_Enabled(t *testing.T) {
	t.Parallel()

	t.Run("delegates to next handler", func(t *testing.T) {
		t.Parallel()
		// Build an inner handler that only accepts Warn and above.
		inner := slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn})
		h := observability.NewScrubHandler(inner)

		ctx := context.Background()
		assert.False(t, h.Enabled(ctx, slog.LevelDebug), "Debug must be disabled when inner is Warn+")
		assert.False(t, h.Enabled(ctx, slog.LevelInfo), "Info must be disabled when inner is Warn+")
		assert.True(t, h.Enabled(ctx, slog.LevelWarn), "Warn must be enabled")
		assert.True(t, h.Enabled(ctx, slog.LevelError), "Error must be enabled")
	})
}

func TestNewScrubHandler_NilPanics(t *testing.T) {
	t.Parallel()

	assert.Panics(t, func() {
		observability.NewScrubHandler(nil)
	}, "NewScrubHandler(nil) must panic")
}
