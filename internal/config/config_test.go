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

	"vpntunnel/internal/config"
	"vpntunnel/internal/publicerror"
)

// validAPIBlock returns a minimal valid api block for use in test JSON payloads.
// max_request_body_bytes, vpn.timeout, and vpn.max_timeout are set explicitly so
// the fixture is valid without relying on applyDefaults filling them — if a default
// ever changes to 0, the fixture must not silently break.
// TLS settings have moved to CLI flags and are no longer part of the api block.
func validAPIBlock() map[string]any {
	return map[string]any{
		"listen": "127.0.0.1:8888",
		"auth": map[string]any{
			"proxy_token_file": "./auth/proxy_token",
			"admin_token_file": "./auth/admin_token",
		},
		"max_request_body_bytes": int64(10 * 1024 * 1024), // 10 MiB
		"vpn": map[string]any{
			"timeout":     "30s",
			"max_timeout": "5m",
		},
	}
}

// validConfigJSON returns a minimal valid proxy.json body with a fully-populated
// api block required by v5. confPath is no longer used (tunnels are
// auto-discovered), but the parameter is retained so callers do not need to be
// updated.
func validConfigJSON(_ string) map[string]any {
	return map[string]any{
		"api": validAPIBlock(),
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
		assert.Equal(t, config.DefaultListen, cfg.VPNStream.Listen)
		assert.Equal(t, config.DefaultDialTimeout, cfg.VPNStream.DialTimeout)
		assert.Equal(t, config.DefaultIdleTimeout, cfg.VPNStream.IdleTimeout)
		assert.Equal(t, config.DefaultShutdownTimeout, cfg.VPNStream.ShutdownTimeout)
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
			"vpnstream": map[string]any{"listen": ""},
			"api":       validAPIBlock(),
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.DefaultListen, cfg.VPNStream.Listen)
	})

	t.Run("invalid listen host:port with multiple colons returns PublicError", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"listen": "not-a-hostport::::"},
			"api":       validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
	})

	t.Run("invalid listen host:port returns PublicError", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"listen": "notahostport"},
			"api":       validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe))
		assert.Contains(t, pe.Details(), "vpnstream.listen")
	})

	t.Run("rejects removed upstream.configs key", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{
				"configs": []string{"./tunnels/se.conf"},
			},
			"api": validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Contains(t, pe.Details(), "config.upstream.configs")
		assert.Contains(t, pe.Details(), "removed")
		assert.Contains(t, pe.Details(), "tunnels")
	})

	t.Run("upstream block without configs is rejected as moved", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{},
			"api":      validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.upstream: moved to vpnstream (allowed_countries → vpnstream.allowed_countries)", pe.Details())
	})

	t.Run("upstream block with allowed_countries is rejected as moved", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"upstream": map[string]any{"allowed_countries": []string{"us"}},
			"api":      validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.upstream: moved to vpnstream (allowed_countries → vpnstream.allowed_countries)", pe.Details())
		// error must not echo the country code values from the config
		assert.NotContains(t, pe.Details(), "us", "error must not echo the country code values")
	})

	t.Run("negative dial timeout returns PublicError", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"dial_timeout": -1},
			"api":       validAPIBlock(),
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
			"vpnstream": map[string]any{"dial_timeout": int64(5 * time.Second)},
			"api":       validAPIBlock(),
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, 5*time.Second, cfg.VPNStream.DialTimeout)
	})

	t.Run("duration accepts string form", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"dial_timeout": "7s"},
			"api":       validAPIBlock(),
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, 7*time.Second, cfg.VPNStream.DialTimeout)
	})

	t.Run("bind on non-loopback emits warn but loads", func(t *testing.T) {
		t.Parallel()
		var records []slog.Record
		handler := &capturingHandler{records: &records}
		logger := slog.New(handler)

		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"listen": "0.0.0.0:7788"},
			"api":       validAPIBlock(),
		})
		cfg, err := config.LoadWithLogger(path, logger)
		require.NoError(t, err)
		assert.Equal(t, "0.0.0.0:7788", cfg.VPNStream.Listen)

		var found bool
		for _, r := range records {
			if r.Level == slog.LevelWarn {
				found = true
				break
			}
		}
		assert.True(t, found, "expected a slog warn for non-loopback bind")
	})

	t.Run("config with health block is rejected with migration probe", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"health": map[string]any{"handshake_max_age": "180s"},
			"api":    validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.health: removed; handshake_max_age is now a built-in constant (lazy.DefaultHandshakeMaxAge = 180s)", pe.Details())
	})

	t.Run("absent auth block loads with zero Auth and disables auth", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, validConfigJSON("./tunnels/se.conf"))
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.Auth{}, cfg.VPNStream.Auth)
	})

	t.Run("auth with only token loads successfully", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"auth": map[string]any{"token": "mytoken"}},
			"api":       validAPIBlock(),
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, "mytoken", cfg.VPNStream.Auth.Token)
		assert.Empty(t, cfg.VPNStream.Auth.TokenFile)
	})

	t.Run("auth with only token_file loads successfully", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"auth": map[string]any{"token_file": "./auth/token"}},
			"api":       validAPIBlock(),
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Empty(t, cfg.VPNStream.Auth.Token)
		assert.Equal(t, "./auth/token", cfg.VPNStream.Auth.TokenFile)
	})

	t.Run("auth with both token and token_file returns PublicError", func(t *testing.T) {
		t.Parallel()
		// use a distinctive value that can't appear in a generic error message
		const sensitiveValue = "xQ9z-SENSITIVE-INLINE-TOKEN"
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"auth": map[string]any{"token": sensitiveValue, "token_file": "./auth/token"}},
			"api":       validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.vpnstream.auth: token and token_file are mutually exclusive; pick one", pe.Details())
		// error must not echo the actual token value
		assert.NotContains(t, pe.Details(), sensitiveValue)
	})

	t.Run("auth with empty strings on both fields is treated as absent", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"auth": map[string]any{"token": "", "token_file": ""}},
			"api":       validAPIBlock(),
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.Auth{}, cfg.VPNStream.Auth)
	})

	t.Run("auth token with surrounding whitespace is trimmed", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"auth": map[string]any{"token": "  trimmed  "}},
			"api":       validAPIBlock(),
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, "trimmed", cfg.VPNStream.Auth.Token)
	})

	t.Run("api_block_missing_rejected", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t,
			"config.api: block is required; add an api block (see configs/proxy.example.json for the required fields)",
			pe.Details(),
		)
	})

	t.Run("admin_block_present_rejected", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"admin": map[string]any{"listen": "127.0.0.1:8081"},
			"api":   validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.admin: removed in v5; use the api block", pe.Details())
	})

	t.Run("api_three_tokens_required", func(t *testing.T) {
		t.Parallel()

		t.Run("admin_token_file_empty_rejected", func(t *testing.T) {
			t.Parallel()
			api := validAPIBlock()
			api["auth"] = map[string]any{
				"proxy_token_file": "./auth/proxy_token",
				"admin_token_file": "",
			}
			path := writeJSON(t, map[string]any{
				"api": api,
			})
			_, err := config.Load(path)
			require.Error(t, err)
			var pe *publicerror.Error
			assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
			assert.Equal(t, "config.api.auth.admin_token_file: required", pe.Details())
		})

		t.Run("proxy_token_file_empty_rejected", func(t *testing.T) {
			t.Parallel()
			api := validAPIBlock()
			api["auth"] = map[string]any{
				"proxy_token_file": "",
				"admin_token_file": "./auth/admin_token",
			}
			path := writeJSON(t, map[string]any{
				"api": api,
			})
			_, err := config.Load(path)
			require.Error(t, err)
			var pe *publicerror.Error
			assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
			assert.Equal(t, "config.api.auth.proxy_token_file: required", pe.Details())
		})
	})

	t.Run("api_user_admin_same_path_rejected", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["auth"] = map[string]any{
			"proxy_token_file": "./auth/shared_token",
			"admin_token_file": "./auth/shared_token",
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t,
			"config.api.auth: proxy_token_file and admin_token_file resolve to the same path",
			pe.Details(),
		)
	})

	t.Run("api_listen_invalid", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["listen"] = "not-a-hostport"
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.api.listen: must be host:port", pe.Details())
	})

	t.Run("upstream_timeout_exceeds_max_rejected", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "10m",
			"max_timeout": "5m",
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t,
			"config.api.vpn.timeout (10m0s) must be <= max_timeout (5m0s)",
			pe.Details(),
		)
	})

	t.Run("defaults_applied", func(t *testing.T) {
		t.Parallel()
		// omit every optional field; only the required ones are present.
		path := writeJSON(t, map[string]any{
			"api": map[string]any{
				"auth": map[string]any{
					"proxy_token_file": "./auth/proxy_token",
					"admin_token_file": "./auth/admin_token",
				},
			},
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.DefaultListen, cfg.VPNStream.Listen)
		assert.Equal(t, config.DefaultDialTimeout, cfg.VPNStream.DialTimeout)
		assert.Equal(t, config.DefaultIdleTimeout, cfg.VPNStream.IdleTimeout)
		assert.Equal(t, config.DefaultShutdownTimeout, cfg.VPNStream.ShutdownTimeout)
		assert.Equal(t, config.DefaultAPIListen, cfg.API.Listen)
		assert.Equal(t, config.DefaultAPIShutdownTimeout, cfg.API.ShutdownTimeout)
		assert.Equal(t, config.DefaultAPIMaxRequestBodyBytes, cfg.API.MaxRequestBodyBytes)
		assert.Equal(t, config.DefaultAPIUpstreamTimeout, cfg.API.VPN.Timeout)
		assert.Equal(t, config.DefaultAPIMaxUpstreamTimeout, cfg.API.VPN.MaxTimeout)
		assert.Equal(t, config.DefaultAsyncStoragePath, cfg.API.VPN.Async.StoragePath)
		assert.Equal(t, config.DefaultOnDemandGrace, cfg.API.VPN.Demand.Grace)
		assert.Equal(t, config.DefaultOnDemandSettleDelay, cfg.API.VPN.Demand.SettleDelay)
		assert.Equal(t, config.DefaultOnDemandIdleTTL, cfg.API.VPN.Demand.IdleTTL)
		assert.Equal(t, config.DefaultTunnelIDHMACKeyFile, cfg.TunnelIDHMACKeyFile)
	})

	t.Run("upstream_timeout_equal_to_max_accepted", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "5m",
			"max_timeout": "5m",
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, 5*time.Minute, cfg.API.VPN.Timeout)
		assert.Equal(t, 5*time.Minute, cfg.API.VPN.MaxTimeout)
	})

	t.Run("async_defaults_applied_when_block_absent", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, validConfigJSON("./tunnels/se.conf"))
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.DefaultAsyncStoragePath, cfg.API.VPN.Async.StoragePath)
	})

	t.Run("async_defaults_applied_when_block_present_but_fields_absent", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "30s",
			"max_timeout": "5m",
			"async":       map[string]any{}, // present but empty
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.DefaultAsyncStoragePath, cfg.API.VPN.Async.StoragePath)
	})

	t.Run("async_valid_full_config_accepted", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "30s",
			"max_timeout": "5m",
			"async": map[string]any{
				"storage_path": "/var/lib/vpntunnel/async.db",
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, "/var/lib/vpntunnel/async.db", cfg.API.VPN.Async.StoragePath)
	})

	t.Run("async_explicit_empty_storage_path_rejected", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "30s",
			"max_timeout": "5m",
			"async": map[string]any{
				"storage_path": "",
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.api.vpn.async.storage_path: must not be empty", pe.Details())
	})

	t.Run("async_partial_block_only_storage_path_set_defaults_applied", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "30s",
			"max_timeout": "5m",
			"async": map[string]any{
				"storage_path": "/custom/path/async.db",
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, "/custom/path/async.db", cfg.API.VPN.Async.StoragePath)
	})

	t.Run("log_path_sanitize_patterns_empty_slice_accepted", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, validConfigJSON("./tunnels/se.conf"))
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Empty(t, cfg.API.Log.PathSanitizePatterns)
	})

	t.Run("log_path_sanitize_patterns_valid_compiled_on_load", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["log"] = map[string]any{
			"path_sanitize_patterns": []any{
				map[string]any{"pattern": `/bot(\d+:[A-Za-z0-9_-]+)/`, "replacement": "/bot<TOKEN>/"},
				map[string]any{"pattern": `api_key=[^&]+`, "replacement": "api_key=<REDACTED>"},
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		require.Len(t, cfg.API.Log.PathSanitizePatterns, 2)
		assert.NotNil(t, cfg.API.Log.PathSanitizePatterns[0].Pattern)
		assert.Equal(t, "/bot<TOKEN>/", cfg.API.Log.PathSanitizePatterns[0].Replacement)
		assert.NotNil(t, cfg.API.Log.PathSanitizePatterns[1].Pattern)
		assert.Equal(t, "api_key=<REDACTED>", cfg.API.Log.PathSanitizePatterns[1].Replacement)
		// verify the compiled regex actually works
		result := cfg.API.Log.PathSanitizePatterns[0].Pattern.ReplaceAllString(
			"/v1/proxy/https/api.telegram.org/bot123456789:ABCDefgh/sendMessage",
			cfg.API.Log.PathSanitizePatterns[0].Replacement,
		)
		assert.Equal(t, "/v1/proxy/https/api.telegram.org/bot<TOKEN>/sendMessage", result)
	})

	t.Run("log_path_sanitize_patterns_invalid_regex_at_index_0_rejected", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["log"] = map[string]any{
			"path_sanitize_patterns": []any{
				map[string]any{"pattern": `[invalid`, "replacement": "x"},
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Contains(t, pe.Details(), "path_sanitize_patterns[0]")
	})

	t.Run("log_path_sanitize_patterns_invalid_regex_at_index_1_rejected", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["log"] = map[string]any{
			"path_sanitize_patterns": []any{
				map[string]any{"pattern": `valid`, "replacement": "x"},
				map[string]any{"pattern": `(unclosed`, "replacement": "y"},
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Contains(t, pe.Details(), "path_sanitize_patterns[1]")
	})

	t.Run("log_path_sanitize_patterns_empty_pattern_at_index_0_rejected", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["log"] = map[string]any{
			"path_sanitize_patterns": []any{
				map[string]any{"pattern": "", "replacement": "x"},
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t,
			"config.api.log.path_sanitize_patterns[0].pattern: must not be empty",
			pe.Details(),
		)
	})

	t.Run("log_path_sanitize_patterns_missing_pattern_key_rejected", func(t *testing.T) {
		t.Parallel()
		// omitting "pattern" key entirely gives "" after JSON unmarshal — same as empty
		api := validAPIBlock()
		api["log"] = map[string]any{
			"path_sanitize_patterns": []any{
				map[string]any{"replacement": "x"}, // no "pattern" key
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		assert.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Contains(t, pe.Details(), "path_sanitize_patterns[0]")
	})

	t.Run("log_path_sanitize_patterns_capture_group_replacement_works", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["log"] = map[string]any{
			"path_sanitize_patterns": []any{
				map[string]any{
					"pattern":     `(/bot)\d+:[A-Za-z0-9_-]+`,
					"replacement": "${1}<TOKEN>",
				},
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		require.Len(t, cfg.API.Log.PathSanitizePatterns, 1)
		result := cfg.API.Log.PathSanitizePatterns[0].Pattern.ReplaceAllString(
			"/bot123456:ABCDEFabcdef",
			cfg.API.Log.PathSanitizePatterns[0].Replacement,
		)
		assert.Equal(t, "/bot<TOKEN>", result)
	})

	t.Run("streaming and ondemand defaults applied when blocks absent", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, validConfigJSON("./tunnels/se.conf"))
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.DefaultStreamingReconnectMin, cfg.VPNStream.ReconnectMin)
		assert.Equal(t, config.DefaultStreamingReconnectMax, cfg.VPNStream.ReconnectMax)
		assert.Equal(t, config.DefaultOnDemandGrace, cfg.API.VPN.Demand.Grace)
		assert.Equal(t, config.DefaultOnDemandSettleDelay, cfg.API.VPN.Demand.SettleDelay)
		assert.Equal(t, config.DefaultOnDemandIdleTTL, cfg.API.VPN.Demand.IdleTTL)
	})

	t.Run("streaming block with explicit values accepted", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"reconnect_min": "5m", "reconnect_max": "2h"},
			"api":       validAPIBlock(),
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, 5*time.Minute, cfg.VPNStream.ReconnectMin)
		assert.Equal(t, 2*time.Hour, cfg.VPNStream.ReconnectMax)
	})

	t.Run("streaming reconnect_min zero rejected", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"reconnect_min": "0s", "reconnect_max": "3h"},
			"api":       validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok)
		assert.Contains(t, pe.Details(), "reconnect_min")
	})

	t.Run("streaming reconnect_max zero rejected", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"reconnect_min": "10m", "reconnect_max": "0s"},
			"api":       validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok)
		assert.Contains(t, pe.Details(), "reconnect_max")
	})

	t.Run("streaming reconnect_min greater than reconnect_max rejected", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"reconnect_min": "4h", "reconnect_max": "3h"},
			"api":       validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok)
		assert.Contains(t, pe.Details(), "reconnect_min")
		assert.Contains(t, pe.Details(), "reconnect_max")
	})

	t.Run("ondemand block with explicit values accepted", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "30s",
			"max_timeout": "5m",
			"demand": map[string]any{
				"grace":        "30s",
				"settle_delay": "20s",
				"idle_ttl":     "72h",
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, 30*time.Second, cfg.API.VPN.Demand.Grace)
		assert.Equal(t, 20*time.Second, cfg.API.VPN.Demand.SettleDelay)
		assert.Equal(t, 72*time.Hour, cfg.API.VPN.Demand.IdleTTL)
	})

	t.Run("ondemand settle_delay below minimum rejected", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "30s",
			"max_timeout": "5m",
			"demand": map[string]any{
				"grace":        "10s",
				"settle_delay": "4s", // below 5s minimum
				"idle_ttl":     "168h",
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok)
		assert.Contains(t, pe.Details(), "settle_delay")
	})

	t.Run("ondemand grace zero rejected", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "30s",
			"max_timeout": "5m",
			"demand":      map[string]any{"grace": "0s"},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok)
		assert.Contains(t, pe.Details(), "grace")
	})

	t.Run("ondemand idle_ttl zero rejected", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "30s",
			"max_timeout": "5m",
			"demand":      map[string]any{"idle_ttl": "0s"},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok)
		assert.Contains(t, pe.Details(), "idle_ttl")
	})

	t.Run("allowed_countries valid codes accepted", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{
				"allowed_countries": []string{"us", "gb", "ua", "de"},
			},
			"api": validAPIBlock(),
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, []string{"us", "gb", "ua", "de"}, cfg.VPNStream.AllowedCountries)
	})

	t.Run("allowed_countries empty list accepted and disables filter", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{
				"allowed_countries": []string{},
			},
			"api": validAPIBlock(),
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Empty(t, cfg.VPNStream.AllowedCountries)
	})

	t.Run("allowed_countries invalid code rejected", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{
				"allowed_countries": []string{"us", "USA"}, // "USA" is three letters — invalid
			},
			"api": validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok)
		assert.Contains(t, pe.Details(), "allowed_countries[1]")
		assert.Contains(t, pe.Details(), "USA")
	})

	t.Run("allowed_countries uppercase code rejected", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{
				"allowed_countries": []string{"US"}, // uppercase — invalid
			},
			"api": validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok)
		assert.Contains(t, pe.Details(), "allowed_countries[0]")
	})

	// Fix 4: settle_delay "0s" is a distinct branch from the < 5s (4s) test.
	// The <= 0 guard fires before the >= minSettleDelay check.
	t.Run("ondemand settle_delay zero rejected", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "30s",
			"max_timeout": "5m",
			"demand":      map[string]any{"settle_delay": "0s"},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok)
		assert.Contains(t, pe.Details(), "settle_delay")
	})

	// Fix 5: boundary cases for allowed_countries entry validation.
	t.Run("allowed_countries empty string entry rejected", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{
				"allowed_countries": []any{"us", ""},
			},
			"api": validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok)
		assert.Contains(t, pe.Details(), "allowed_countries[1]")
	})

	t.Run("allowed_countries digit-containing entry rejected", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{
				"allowed_countries": []string{"1a"},
			},
			"api": validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok)
		assert.Contains(t, pe.Details(), "allowed_countries[0]")
		assert.Contains(t, pe.Details(), "1a")
	})

	t.Run("allowed_countries single-char entry rejected", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{
				"allowed_countries": []string{"u"},
			},
			"api": validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok)
		assert.Contains(t, pe.Details(), "allowed_countries[0]")
		assert.Contains(t, pe.Details(), `"u"`)
	})

	// Fix 6: partial streaming block — only reconnect_min set; reconnect_max absent
	// should receive DefaultStreamingReconnectMax.
	t.Run("streaming partial block only reconnect_min set defaults reconnect_max", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"vpnstream": map[string]any{"reconnect_min": "5m"},
			"api":       validAPIBlock(),
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, 5*time.Minute, cfg.VPNStream.ReconnectMin)
		assert.Equal(t, config.DefaultStreamingReconnectMax, cfg.VPNStream.ReconnectMax)
	})

	// Wave 11: exhaustive migration-probe tests. Each subtest verifies that exactly
	// one old v5 key triggers the right publicerror message.

	t.Run("rejects_moved_top_level_listen", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"listen": "127.0.0.1:7788",
			"api":    validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.listen: moved to vpnstream.listen", pe.Details())
	})

	t.Run("rejects_moved_top_level_auth", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"auth": map[string]any{"token_file": "./auth/token"},
			"api":  validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.auth: moved to vpnstream.auth", pe.Details())
	})

	t.Run("rejects_moved_top_level_dial_timeout", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"dial_timeout": "10s",
			"api":          validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.dial_timeout: moved to vpnstream.dial_timeout", pe.Details())
	})

	t.Run("rejects_moved_top_level_idle_timeout", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"idle_timeout": "90s",
			"api":          validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.idle_timeout: moved to vpnstream.idle_timeout", pe.Details())
	})

	t.Run("rejects_moved_top_level_shutdown_timeout", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"shutdown_timeout": "15s",
			"api":              validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.shutdown_timeout: moved to vpnstream.shutdown_timeout", pe.Details())
	})

	t.Run("rejects_moved_top_level_streaming", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"streaming": map[string]any{"reconnect_min": "10m", "reconnect_max": "3h"},
			"api":       validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.streaming: moved to vpnstream (reconnect_min/reconnect_max)", pe.Details())
	})

	t.Run("rejects_moved_top_level_ondemand", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"ondemand": map[string]any{"grace": "10s", "settle_delay": "15s", "idle_ttl": "168h"},
			"api":      validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.ondemand: moved to api.vpn.demand", pe.Details())
	})

	t.Run("rejects_removed_api_tls", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["tls"] = map[string]any{"hostname": "localhost", "cert_dir": "/tmp/tls"}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t,
			"config.api.tls: removed; use the -tls-cert-dir, -tls-hostname, -tls-ip-sans CLI flags",
			pe.Details(),
		)
	})

	t.Run("rejects_moved_api_upstream_timeout", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["upstream_timeout"] = "30s"
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.api.upstream_timeout: moved to api.vpn.timeout", pe.Details())
	})

	t.Run("rejects_moved_api_max_upstream_timeout", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["max_upstream_timeout"] = "5m"
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.api.max_upstream_timeout: moved to api.vpn.max_timeout", pe.Details())
	})

	t.Run("rejects_moved_api_async_block_at_old_location", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["async"] = map[string]any{"storage_path": "/opt/vpntunnel/state/async.db"}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t,
			"config.api.async: moved to api.vpn.async (only storage_path survives; max_concurrent_jobs/pending_timeout/complete_ttl/tombstone_ttl are now built-in constants in internal/asyncjob)",
			pe.Details(),
		)
	})

	t.Run("rejects_removed_api_vpn_async_max_concurrent_jobs", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "30s",
			"max_timeout": "5m",
			"async": map[string]any{
				"storage_path":        "/opt/vpntunnel/state/async.db",
				"max_concurrent_jobs": 100,
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t,
			"config.api.vpn.async.max_concurrent_jobs: removed; now a built-in constant in internal/asyncjob",
			pe.Details(),
		)
	})

	t.Run("rejects_removed_api_vpn_async_pending_timeout", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "30s",
			"max_timeout": "5m",
			"async": map[string]any{
				"storage_path":    "/opt/vpntunnel/state/async.db",
				"pending_timeout": "5m",
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t,
			"config.api.vpn.async.pending_timeout: removed; now a built-in constant in internal/asyncjob",
			pe.Details(),
		)
	})

	t.Run("rejects_removed_api_vpn_async_complete_ttl", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "30s",
			"max_timeout": "5m",
			"async": map[string]any{
				"storage_path": "/opt/vpntunnel/state/async.db",
				"complete_ttl": "1h",
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t,
			"config.api.vpn.async.complete_ttl: removed; now a built-in constant in internal/asyncjob",
			pe.Details(),
		)
	})

	t.Run("rejects_removed_api_vpn_async_tombstone_ttl", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		api["vpn"] = map[string]any{
			"timeout":     "30s",
			"max_timeout": "5m",
			"async": map[string]any{
				"storage_path":  "/opt/vpntunnel/state/async.db",
				"tombstone_ttl": "24h",
			},
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t,
			"config.api.vpn.async.tombstone_ttl: removed; now a built-in constant in internal/asyncjob",
			pe.Details(),
		)
	})

	// first-match-wins ordering: a config with both listen and health returns
	// the listen message (listen is probe #2; health is probe #10; structural-first).
	t.Run("first_match_wins_listen_before_health", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"listen": "127.0.0.1:7788",
			"health": map[string]any{"handshake_max_age": "180s"},
			"api":    validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.listen: moved to vpnstream.listen", pe.Details())
	})

	// first-match-wins across top-level probes added in Wave 11: listen fires before
	// downstream probes (auth, dial_timeout, etc.).
	t.Run("first_match_wins_listen_before_auth", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"listen": "127.0.0.1:7788",
			"auth":   map[string]any{"token_file": "./auth/token"},
			"api":    validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.listen: moved to vpnstream.listen", pe.Details())
	})

	// api.auth migration probes: user_token_file renamed to proxy_token_file;
	// deploy_token_file removed.

	t.Run("rejects_renamed_api_auth_user_token_file", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		// set the old key alongside valid new keys — probe must fire before validate.
		api["auth"] = map[string]any{
			"user_token_file":  "./auth/proxy_token",
			"proxy_token_file": "./auth/proxy_token",
			"admin_token_file": "./auth/admin_token",
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t,
			"config.api.auth.user_token_file: renamed to proxy_token_file",
			pe.Details(),
		)
	})

	t.Run("rejects_removed_api_auth_deploy_token_file", func(t *testing.T) {
		t.Parallel()
		api := validAPIBlock()
		// set the old deploy key alongside valid new keys — probe must fire before validate.
		api["auth"] = map[string]any{
			"deploy_token_file": "./auth/deploy_token",
			"proxy_token_file":  "./auth/proxy_token",
			"admin_token_file":  "./auth/admin_token",
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t,
			"config.api.auth.deploy_token_file: removed; the deploy role no longer exists — the release health-check uses the admin token",
			pe.Details(),
		)
	})

	t.Run("first_match_wins_rename_before_deploy_auth_probe", func(t *testing.T) {
		t.Parallel()
		// both old keys present — user_token_file rename probe fires first.
		api := validAPIBlock()
		api["auth"] = map[string]any{
			"user_token_file":   "./auth/proxy_token",
			"deploy_token_file": "./auth/deploy_token",
			"proxy_token_file":  "./auth/proxy_token",
			"admin_token_file":  "./auth/admin_token",
		}
		path := writeJSON(t, map[string]any{
			"api": api,
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t,
			"config.api.auth.user_token_file: renamed to proxy_token_file",
			pe.Details(),
		)
	})

	// tunnel_id_hmac_key_file field subtests.

	t.Run("tunnel_id_hmac_key_file_absent_defaults_applied", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, validConfigJSON("./tunnels/se.conf"))
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, config.DefaultTunnelIDHMACKeyFile, cfg.TunnelIDHMACKeyFile)
	})

	t.Run("tunnel_id_hmac_key_file_explicit_path_preserved", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"tunnel_id_hmac_key_file": "/opt/vpntunnel/auth/tunnel-id.key",
			"api":                     validAPIBlock(),
		})
		cfg, err := config.Load(path)
		require.NoError(t, err)
		assert.Equal(t, "/opt/vpntunnel/auth/tunnel-id.key", cfg.TunnelIDHMACKeyFile)
	})

	t.Run("tunnel_id_hmac_key_file_explicit_empty_rejected", func(t *testing.T) {
		t.Parallel()
		path := writeJSON(t, map[string]any{
			"tunnel_id_hmac_key_file": "",
			"api":                     validAPIBlock(),
		})
		_, err := config.Load(path)
		require.Error(t, err)
		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected PublicError, got: %v", err)
		assert.Equal(t, "config.tunnel_id_hmac_key_file: must not be empty", pe.Details())
	})

}

// TestProxyExampleJSON is a schema-drift guard: it loads the shipped
// configs/proxy.example.json through config.Load and asserts it parses clean.
// A failing test here means either the example has stale keys (probe rejects it)
// or a valid v6 key was accidentally probed as removed (false-positive).
func TestProxyExampleJSON(t *testing.T) {
	t.Parallel()
	// test package cwd is internal/config; the example is two dirs up.
	examplePath := filepath.Join("..", "..", "configs", "proxy.example.json")
	cfg, err := config.Load(examplePath)
	require.NoError(t, err, "configs/proxy.example.json failed to load — schema drift detected")
	// spot-check a handful of values so a silent zero-value parse failure is caught.
	assert.Equal(t, "127.0.0.1:7788", cfg.VPNStream.Listen)
	assert.Equal(t, "/opt/vpntunnel/state/async.db", cfg.API.VPN.Async.StoragePath)
	assert.Equal(t, []string{"ch", "se", "us", "gb", "ua", "de"}, cfg.VPNStream.AllowedCountries)
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
