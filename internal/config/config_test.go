package config_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"httpproxy/internal/config"
	"httpproxy/internal/publicerror"
)

// validConfigJSON returns a minimal valid proxy.json body with one .conf entry.
func validConfigJSON(confPath string) map[string]any {
	return map[string]any{
		"upstream": map[string]any{
			"configs": []string{confPath},
			"active":  "",
		},
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()

	t.Run("loads valid minimal config and applies defaults", func(t *testing.T) {
		t.Parallel()
		// the .conf path doesn't need to exist for config validation
		path := writeJSON(t, validConfigJSON("./tunnels/se.conf"))
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.DefaultListen, cfg.Listen)
		assert.Equal(t, config.DefaultDialTimeout, cfg.DialTimeout)
		assert.Equal(t, config.DefaultIdleTimeout, cfg.IdleTimeout)
		assert.Equal(t, config.DefaultShutdownTimeout, cfg.ShutdownTimeout)
		assert.Equal(t, config.DefaultAccessLogPath, cfg.AccessLog.Path)
		assert.Equal(t, config.DefaultAccessLogSizeMB, cfg.AccessLog.MaxSizeMB)
		assert.Equal(t, config.DefaultAccessLogAgeDays, cfg.AccessLog.MaxAgeDays)
		assert.Equal(t, config.DefaultAccessLogBackups, cfg.AccessLog.MaxBackups)
		assert.True(t, cfg.AccessLog.Compress)
		assert.Equal(t, config.DefaultOperationalLevel, cfg.Operational.Level)
		assert.Equal(t, config.DefaultOperationalFormat, cfg.Operational.Format)
	})

	t.Run("missing file returns plain error", func(t *testing.T) {
		t.Parallel()
		_, err := config.Load("/nonexistent/path/proxy.json")
		require.Error(t, err)
		var pe *publicerror.Error
		assert.False(t, errors.As(err, &pe), "expected plain error, got PublicError")
	})

	t.Run("malformed JSON returns plain error", func(t *testing.T) {
		t.Parallel()
		f, err := os.CreateTemp(t.TempDir(), "proxy*.json")
		require.NoError(t, err)
		_, err = f.WriteString("{not valid json}")
		require.NoError(t, err)
		require.NoError(t, f.Close())

		_, loadErr := config.Load(f.Name())
		require.Error(t, loadErr)
		var pe *publicerror.Error
		assert.False(t, errors.As(loadErr, &pe), "expected plain error, got PublicError")
	})

	t.Run("empty listen falls back to default", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"listen":   "",
			"upstream": map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.DefaultListen, cfg.Listen)
	})

	t.Run("invalid listen host:port with multiple colons returns PublicError", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"listen":   "not-a-hostport::::",
			"upstream": map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
	})

	t.Run("invalid listen host:port returns PublicError", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"listen":   "notahostport",
			"upstream": map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe))
		assert.Contains(t, pe.Details(), "config.listen")
	})

	t.Run("empty configs slice returns PublicError", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{"configs": []string{}, "active": ""},
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe))
		assert.Equal(t,
			"config.upstream.configs: at least one .conf path required; download a wg-quick config and add its path here",
			pe.Details(),
		)
	})

	t.Run("nil configs returns PublicError", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{"active": ""},
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe))
		assert.Contains(t, pe.Details(), "config.upstream.configs")
	})

	t.Run("empty string entry in configs returns PublicError with index", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{"configs": []any{"./tunnels/a.conf", ""}, "active": ""},
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe))
		assert.Equal(t, "config.upstream.configs[1]: path must be non-empty", pe.Details())
	})

	t.Run("active not matching any config returns PublicError listing available", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{
				"configs": []string{"./tunnels/se-sto.conf", "./tunnels/de-fra.conf"},
				"active":  "us-nyc",
			},
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe))
		assert.Contains(t, pe.Details(), "config.upstream.active")
		assert.Contains(t, pe.Details(), `"us-nyc"`)
		assert.Contains(t, pe.Details(), "se-sto")
		assert.Contains(t, pe.Details(), "de-fra")
	})

	t.Run("active empty is valid and selects Configs[0] at wiring time", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{
				"configs": []string{"./tunnels/se-sto.conf"},
				"active":  "",
			},
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Empty(t, cfg.Upstream.Active)
		assert.Len(t, cfg.Upstream.Configs, 1)
	})

	t.Run("active matching a config by basename succeeds", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{
				"configs": []string{"./tunnels/se-sto.conf", "./tunnels/de-fra.conf"},
				"active":  "de-fra",
			},
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, "de-fra", cfg.Upstream.Active)
	})

	t.Run("negative dial timeout returns PublicError", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream":     map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
			"dial_timeout": -1,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe))
		assert.Contains(t, pe.Details(), "dial_timeout")
	})

	t.Run("duration accepts raw nanosecond integer", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream":     map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
			"dial_timeout": int64(5 * time.Second),
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, 5*time.Second, cfg.DialTimeout)
	})

	t.Run("duration accepts string form", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream":     map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
			"dial_timeout": "7s",
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, 7*time.Second, cfg.DialTimeout)
	})

	t.Run("bind on non-loopback emits warn but loads", func(t *testing.T) {
		t.Parallel()
		var records []slog.Record
		handler := &capturingHandler{records: &records}
		logger := slog.New(handler)

		path := writeJSON(t, map[string]any{
			"listen":   "0.0.0.0:8080",
			"upstream": map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
		})
		cfg, err := config.LoadWithLogger(path, logger)
		require.NoError(t, err)
		assert.Equal(t, "0.0.0.0:8080", cfg.Listen)

		var found bool
		for _, r := range records {
			if r.Level == slog.LevelWarn {
				found = true
				break
			}
		}
		assert.True(t, found, "expected a slog warn for non-loopback bind")
	})

	t.Run("missing admin block applies defaults", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, validConfigJSON("./tunnels/se.conf"))
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.DefaultAdminListen, cfg.Admin.Listen)
		assert.Equal(t, config.DefaultAdminShutdownTimeout, cfg.Admin.ShutdownTimeout)
	})

	t.Run("missing health block applies defaults", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, validConfigJSON("./tunnels/se.conf"))
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.DefaultHealthHandshakeMaxAge, cfg.Health.HandshakeMaxAge)
	})

	t.Run("explicit admin.listen overrides default", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
			"admin":    map[string]any{"listen": "0.0.0.0:9999"},
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, "0.0.0.0:9999", cfg.Admin.Listen)
	})

	t.Run("admin block present but empty still gets defaults", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
			"admin":    map[string]any{},
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.DefaultAdminListen, cfg.Admin.Listen)
		assert.Equal(t, config.DefaultAdminShutdownTimeout, cfg.Admin.ShutdownTimeout)
	})

	t.Run("validate rejects health.handshake_max_age = 0", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
			"health":   map[string]any{"handshake_max_age": "0s"},
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.health.handshake_max_age: must be > 0", pe.Details())
	})

	t.Run("validate rejects malformed admin.listen", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
			"admin":    map[string]any{"listen": "not-a-host-port"},
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.admin.listen: must be host:port", pe.Details())
	})

	t.Run("explicit admin.shutdown_timeout 0s is preserved as 0", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
			"admin":    map[string]any{"listen": "127.0.0.1:8081", "shutdown_timeout": "0s"},
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, time.Duration(0), cfg.Admin.ShutdownTimeout,
			"explicit 0s must not be replaced by the default")
	})

	t.Run("absent auth block loads with zero Auth and disables auth", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, validConfigJSON("./tunnels/se.conf"))
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.Auth{}, cfg.Auth)
	})

	t.Run("auth with only token loads successfully", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
			"auth":     map[string]any{"token": "mytoken"},
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, "mytoken", cfg.Auth.Token)
		assert.Empty(t, cfg.Auth.TokenFile)
	})

	t.Run("auth with only token_file loads successfully", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
			"auth":     map[string]any{"token_file": "./auth/token"},
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Empty(t, cfg.Auth.Token)
		assert.Equal(t, "./auth/token", cfg.Auth.TokenFile)
	})

	t.Run("auth with both token and token_file returns PublicError", func(t *testing.T) {
		t.Parallel()
		// use a distinctive value that can't appear in a generic error message
		const sensitiveValue = "xQ9z-SENSITIVE-INLINE-TOKEN"
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
			"auth":     map[string]any{"token": sensitiveValue, "token_file": "./auth/token"},
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.auth: token and token_file are mutually exclusive; pick one", pe.Details())
		// error must not echo the actual token value
		assert.NotContains(t, pe.Details(), sensitiveValue)
	})

	t.Run("auth with empty strings on both fields is treated as absent", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
			"auth":     map[string]any{"token": "", "token_file": ""},
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.Auth{}, cfg.Auth)
	})

	t.Run("auth token with surrounding whitespace is trimmed", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{"configs": []string{"./tunnels/se.conf"}, "active": ""},
			"auth":     map[string]any{"token": "  trimmed  "},
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, "trimmed", cfg.Auth.Token)
	})

}

func writeJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	f, err := os.CreateTemp(t.TempDir(), "proxy*.json")
	require.NoError(t, err)
	_, err = f.Write(b)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return filepath.Clean(f.Name())
}

// capturingHandler is a minimal slog.Handler that captures records for assertion.
type capturingHandler struct {
	records *[]slog.Record
}

var _ slog.Handler = (*capturingHandler)(nil)

func (h *capturingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)
	return nil
}

func (h *capturingHandler) WithAttrs(_ []slog.Attr) slog.Handler {
	return h
}

func (h *capturingHandler) WithGroup(_ string) slog.Handler {
	return h
}
