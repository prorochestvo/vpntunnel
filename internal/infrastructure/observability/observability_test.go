package observability_test

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/domain"
	"vpntunnel/internal/infrastructure/config"
	"vpntunnel/internal/infrastructure/observability"
)

func TestNewOperationalLogger(t *testing.T) {
	t.Parallel()

	t.Run("text format produces a non-nil logger", func(t *testing.T) {
		t.Parallel()
		logger := observability.NewOperationalLogger(config.Operational{
			Level:  "info",
			Format: "text",
		})
		assert.NotNil(t, logger)
	})

	t.Run("json format produces a non-nil logger", func(t *testing.T) {
		t.Parallel()
		logger := observability.NewOperationalLogger(config.Operational{
			Level:  "debug",
			Format: "json",
		})
		assert.NotNil(t, logger)
	})

	t.Run("unknown level defaults to info and does not panic", func(t *testing.T) {
		t.Parallel()
		assert.NotPanics(t, func() {
			logger := observability.NewOperationalLogger(config.Operational{
				Level:  "bogus",
				Format: "text",
			})
			assert.NotNil(t, logger)
			// logger should be enabled at Info but not below
			assert.True(t, logger.Enabled(t.Context(), slog.LevelInfo))
			assert.False(t, logger.Enabled(t.Context(), slog.LevelDebug))
		})
	})
}

func TestAccessLogger_Log(t *testing.T) {
	t.Parallel()

	t.Run("writes one json record per call", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "access.log")

		al, err := observability.NewAccessLogger(config.AccessLog{
			Path:       logPath,
			MaxSizeMB:  100,
			MaxAgeDays: 14,
			MaxBackups: 7,
			Compress:   false,
		}, nil, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = al.Close() })

		summaries := []domain.RequestSummary{
			{Method: "GET", Target: "http://example.com/1", ClientAddr: "127.0.0.1:1000", StatusCode: 200, BytesOut: 42},
			{Method: "CONNECT", Target: "api.example.com:443", ClientAddr: "127.0.0.1:1001", StatusCode: 0, BytesIn: 1024},
			{Method: "POST", Target: "http://example.com/2", ClientAddr: "127.0.0.1:1002", StatusCode: 201, BytesOut: 10, DurationMS: 5},
		}
		for _, s := range summaries {
			al.Log(s)
		}
		require.NoError(t, al.Close())

		lines := readAccessLogLines(t, dir, "access")
		assert.Len(t, lines, 3, "expected 3 JSONL lines")

		assert.Equal(t, "GET", lines[0]["method"])
		assert.Equal(t, "CONNECT", lines[1]["method"])
		assert.Equal(t, "POST", lines[2]["method"])
	})

	t.Run("creates parent directory if missing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "sub", "dir", "access.log")

		al, err := observability.NewAccessLogger(config.AccessLog{
			Path:       logPath,
			MaxSizeMB:  100,
			MaxAgeDays: 14,
			MaxBackups: 7,
			Compress:   false,
		}, nil, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = al.Close() })

		al.Log(domain.RequestSummary{Method: "GET", Target: "http://x.com"})
		require.NoError(t, al.Close())

		_, statErr := os.Stat(logPath)
		assert.NoError(t, statErr, "expected the access log file to exist at the fixed path")
	})

	t.Run("Close is idempotent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		al, err := observability.NewAccessLogger(config.AccessLog{
			Path:       filepath.Join(dir, "access.log"),
			MaxSizeMB:  100,
			MaxAgeDays: 14,
			MaxBackups: 7,
			Compress:   false,
		}, nil, nil)
		require.NoError(t, err)
		// first Close should succeed
		require.NoError(t, al.Close())
		// second Close must not panic (error is acceptable)
		assert.NotPanics(t, func() { _ = al.Close() })
	})
}

func TestAccessLogger_RotationCompressionStableName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")

	al, err := observability.NewAccessLogger(config.AccessLog{
		Path:       logPath,
		MaxSizeMB:  1,
		MaxAgeDays: 14,
		MaxBackups: 3,
		Compress:   true,
	}, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = al.Close() })

	// write well over 1 MiB to force at least one rotation. A padded target
	// makes each JSONL line ~0.5 KiB, so a few thousand lines suffice.
	padded := "http://example.com/" + strings.Repeat("x", 400)
	for range 4000 {
		al.Log(domain.RequestSummary{Method: "GET", Target: padded, ClientAddr: "127.0.0.1:1000", StatusCode: 200})
	}
	require.NoError(t, al.Close())

	// WithStableCurrentName: the live file stays at the fixed path.
	_, statErr := os.Stat(logPath)
	require.NoError(t, statErr, "live access.log must exist at the fixed path")

	// WithCompress: at least one rotated backup must be gzip-compressed.
	gzs, err := filepath.Glob(filepath.Join(dir, "access.*.log.gz"))
	require.NoError(t, err)
	require.NotEmpty(t, gzs, "expected at least one gzipped rotated backup")

	// the compressed backup must decompress to valid access-log JSONL.
	f, err := os.Open(gzs[0])
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	gr, err := gzip.NewReader(f)
	require.NoError(t, err)
	defer func() { _ = gr.Close() }()
	scanner := bufio.NewScanner(gr)
	require.True(t, scanner.Scan(), "compressed backup must contain at least one line")
	var rec map[string]any
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &rec))
	assert.Equal(t, "GET", rec["method"])
}

func TestAccessLogger_PathSanitization(t *testing.T) {
	t.Parallel()

	t.Run("nil Regexp in sanitizer is rejected at construction", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_, err := observability.NewAccessLogger(config.AccessLog{
			Path:       filepath.Join(dir, "access.log"),
			MaxSizeMB:  100,
			MaxAgeDays: 14,
			MaxBackups: 7,
			Compress:   false,
		}, nil, []observability.PathSanitizePattern{{Regexp: nil, Replacement: "x"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "sanitizers[0].Regexp is nil")
	})

	t.Run("empty patterns slice leaves target unchanged", func(t *testing.T) {
		t.Parallel()
		al, readTargets := newTestAccessLogger(t, nil)
		al.Log(domain.RequestSummary{
			Method: "GET",
			Target: "/v1/proxy/https/api.example.com/bot123:abc/sendMessage",
		})
		targets := readTargets()
		require.Len(t, targets, 1)
		assert.Equal(t, "/v1/proxy/https/api.example.com/bot123:abc/sendMessage", targets[0])
	})

	t.Run("single Telegram-bot pattern redacts token", func(t *testing.T) {
		t.Parallel()
		sanitizers := []observability.PathSanitizePattern{
			{
				Regexp:      regexp.MustCompile(`(/bot)[^/]+(/)`),
				Replacement: `$1<REDACTED>$2`,
			},
		}
		al, readTargets := newTestAccessLogger(t, sanitizers)
		al.Log(domain.RequestSummary{
			Method: "POST",
			Target: "/v1/proxy/https/api.telegram.org/bot123:abc/sendMessage",
		})
		targets := readTargets()
		require.Len(t, targets, 1)
		assert.Equal(t, "/v1/proxy/https/api.telegram.org/bot<REDACTED>/sendMessage", targets[0])
	})

	t.Run("multiple stacked patterns are applied left-to-right", func(t *testing.T) {
		t.Parallel()
		sanitizers := []observability.PathSanitizePattern{
			{
				Regexp:      regexp.MustCompile(`/x/`),
				Replacement: `/y/`,
			},
			{
				Regexp:      regexp.MustCompile(`/y/`),
				Replacement: `/z/`,
			},
		}
		al, readTargets := newTestAccessLogger(t, sanitizers)
		al.Log(domain.RequestSummary{
			Method: "GET",
			Target: "/x/foo",
		})
		targets := readTargets()
		require.Len(t, targets, 1)
		assert.Equal(t, "/z/foo", targets[0])
	})

	t.Run("capture-group replacement preserves structural prefix", func(t *testing.T) {
		t.Parallel()
		sanitizers := []observability.PathSanitizePattern{
			{
				Regexp:      regexp.MustCompile(`(/bot)([^/]+)(/sendMessage)`),
				Replacement: `${1}<REDACTED>${3}`,
			},
		}
		al, readTargets := newTestAccessLogger(t, sanitizers)
		al.Log(domain.RequestSummary{
			Method: "POST",
			Target: "/bot999:secret/sendMessage",
		})
		targets := readTargets()
		require.Len(t, targets, 1)
		assert.Equal(t, "/bot<REDACTED>/sendMessage", targets[0])
	})

	t.Run("original request summary target is unchanged after logging", func(t *testing.T) {
		t.Parallel()
		sanitizers := []observability.PathSanitizePattern{
			{
				Regexp:      regexp.MustCompile(`secret`),
				Replacement: `REDACTED`,
			},
		}
		al, _ := newTestAccessLogger(t, sanitizers)
		t.Cleanup(func() { _ = al.Close() })
		s := domain.RequestSummary{
			Method: "GET",
			Target: "/path/with/secret/value",
		}
		originalTarget := s.Target
		al.Log(s)
		assert.Equal(t, originalTarget, s.Target, "Log must not mutate RequestSummary.Target")
	})

	t.Run("upstream_error field is also sanitized", func(t *testing.T) {
		t.Parallel()
		sanitizers := []observability.PathSanitizePattern{
			{
				Regexp:      regexp.MustCompile(`(/bot)[^/]+(/)`),
				Replacement: `$1<REDACTED>$2`,
			},
		}
		al, readUpstreamErrors := newTestAccessLoggerUpstreamErr(t, sanitizers)
		al.Log(domain.RequestSummary{
			Method:        "POST",
			Target:        "/ok",
			UpstreamError: "dial /bot123:abc/sendMessage failed",
		})
		errs := readUpstreamErrors()
		require.Len(t, errs, 1)
		assert.NotContains(t, errs[0], "bot123:abc", "token must be redacted from upstream_error")
		assert.Contains(t, errs[0], "bot<REDACTED>/", "replacement must appear in upstream_error")
	})
}

// newTestAccessLogger is a test helper that creates an AccessLogger writing to
// a temp file, with the supplied sanitizers, and returns both the logger and a
// function that reads the target field from every emitted JSONL line.
// readTargets owns the single Close call; callers that do not invoke readTargets
// must register their own t.Cleanup to close the logger.
// readAccessLogLines reads every JSONL record from the live access log. With
// WithStableCurrentName the live file stays at the fixed "<prefix>.log" path;
// these tests write too little to trigger rotation, so all records are there.
func readAccessLogLines(t *testing.T, dir, prefix string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, prefix+".log"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var recs []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var rec map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &rec))
		recs = append(recs, rec)
	}
	require.NoError(t, scanner.Err())
	return recs
}

func newTestAccessLogger(t *testing.T, sanitizers []observability.PathSanitizePattern) (*observability.AccessLogger, func() []string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")

	al, err := observability.NewAccessLogger(config.AccessLog{
		Path:       logPath,
		MaxSizeMB:  100,
		MaxAgeDays: 14,
		MaxBackups: 7,
		Compress:   false,
	}, nil, sanitizers)
	require.NoError(t, err)

	readTargets := func() []string {
		require.NoError(t, al.Close())
		var targets []string
		for _, rec := range readAccessLogLines(t, dir, "access") {
			if v, ok := rec["target"].(string); ok {
				targets = append(targets, v)
			}
		}
		return targets
	}

	return al, readTargets
}

// newTestAccessLoggerUpstreamErr is a variant of newTestAccessLogger that reads
// the upstream_error field instead of target.
func newTestAccessLoggerUpstreamErr(t *testing.T, sanitizers []observability.PathSanitizePattern) (*observability.AccessLogger, func() []string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")

	al, err := observability.NewAccessLogger(config.AccessLog{
		Path:       logPath,
		MaxSizeMB:  100,
		MaxAgeDays: 14,
		MaxBackups: 7,
		Compress:   false,
	}, nil, sanitizers)
	require.NoError(t, err)

	readUpstreamErrors := func() []string {
		require.NoError(t, al.Close())
		var errs []string
		for _, rec := range readAccessLogLines(t, dir, "access") {
			if v, ok := rec["upstream_error"].(string); ok {
				errs = append(errs, v)
			}
		}
		return errs
	}

	return al, readUpstreamErrors
}
