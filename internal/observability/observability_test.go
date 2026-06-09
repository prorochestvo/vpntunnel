package observability_test

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"httpproxy/internal/config"
	"httpproxy/internal/domain"
	"httpproxy/internal/observability"
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
		}, nil)
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

		f, err := os.Open(logPath)
		require.NoError(t, err)
		defer func() { _ = f.Close() }()

		var lines []map[string]any
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			var rec map[string]any
			require.NoError(t, json.Unmarshal(scanner.Bytes(), &rec))
			lines = append(lines, rec)
		}
		require.NoError(t, scanner.Err())
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
		}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = al.Close() })

		al.Log(domain.RequestSummary{Method: "GET", Target: "http://x.com"})
		require.NoError(t, al.Close())

		_, statErr := os.Stat(logPath)
		assert.NoError(t, statErr, "expected log file to exist")
	})

	t.Run("Close closes underlying writer", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		al, err := observability.NewAccessLogger(config.AccessLog{
			Path:       filepath.Join(dir, "access.log"),
			MaxSizeMB:  100,
			MaxAgeDays: 14,
			MaxBackups: 7,
			Compress:   false,
		}, nil)
		require.NoError(t, err)
		// first Close should succeed
		require.NoError(t, al.Close())
		// second Close must not panic (error is acceptable)
		assert.NotPanics(t, func() { _ = al.Close() })
	})
}
