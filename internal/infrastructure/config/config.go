// Package config loads and validates the proxy's JSON configuration file.
// Validation errors that the operator must fix are returned as
// loginjector.PublicDetailsError; I/O and JSON parse failures are plain errors.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/prorochestvo/loginjector"
)

// Default values applied when the corresponding JSON field is absent or zero.
const (
	DefaultListen            = "127.0.0.1:7788"
	DefaultDialTimeout       = 10 * time.Second
	DefaultIdleTimeout       = 90 * time.Second
	DefaultShutdownTimeout   = 15 * time.Second
	DefaultAccessLogPath     = "./logs/access.log"
	DefaultAccessLogSizeMB   = 100
	DefaultAccessLogAgeDays  = 14
	DefaultAccessLogBackups  = 7
	DefaultOperationalLevel  = "info"
	DefaultOperationalFormat = "text"

	DefaultAPIListen              = "127.0.0.1:8888"
	DefaultAPIShutdownTimeout     = 5 * time.Second
	DefaultAPIMaxRequestBodyBytes = int64(10 * 1024 * 1024) // 10 MiB
	DefaultAPIUpstreamTimeout     = 30 * time.Second
	DefaultAPIMaxUpstreamTimeout  = 5 * time.Minute

	DefaultAsyncStoragePath = "/opt/vpntunnel/state/async.db"

	// DefaultTunnelIDHMACKeyFile is the default path (relative to the config dir)
	// for the HMAC key file used to derive stable per-host tunnel ids. The consumer
	// resolves this path against the config dir and generates a 32-byte random key
	// on first run when the file is absent. The key material (file contents) must
	// never be logged.
	DefaultTunnelIDHMACKeyFile = "./auth/tunnel-id.key"

	// DefaultStreamingReconnectMin is the minimum backoff between streaming-role
	// reconnect attempts. On a successful healthy reconnect the backoff resets to
	// this value.
	DefaultStreamingReconnectMin = 10 * time.Minute
	// DefaultStreamingReconnectMax is the ceiling for the exponential backoff
	// between streaming-role reconnect attempts.
	DefaultStreamingReconnectMax = 3 * time.Hour

	// DefaultOnDemandGrace is the time the on-demand scheduler waits after the
	// last same-zone request before switching to another zone's oldest pending job.
	DefaultOnDemandGrace = 10 * time.Second
	// DefaultOnDemandSettleDelay is the mandatory pause between tearing down one
	// on-demand WireGuard device and bringing the next one up. Minimum 5s.
	DefaultOnDemandSettleDelay = 15 * time.Second
	// DefaultOnDemandIdleTTL is how long the on-demand scheduler keeps a live
	// device after the last request before tearing it down proactively.
	DefaultOnDemandIdleTTL = 168 * time.Hour // 7 days
)

// Load reads the JSON config at path, applies defaults, and validates the result.
// The returned Config carries Dir, the absolute directory of path, so consumers
// need no second copy of the config path. It returns a
// loginjector.PublicDetailsError for fields the operator must correct, or a
// plain wrapped error for I/O and JSON failures.
func Load(path string) (Config, error) {
	return LoadWithLogger(path, slog.Default())
}

// LoadWithLogger is Load with an explicit slog.Logger for the non-loopback
// bind warning. Useful in tests that capture log output.
func LoadWithLogger(path string, logger *slog.Logger) (Config, error) {
	// read once; decode twice: first into a probe map to detect removed keys,
	// then into the typed rawConfig.
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config %s: %w", path, err)
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	if _, hasAdmin := probe["admin"]; hasAdmin {
		return Config{}, loginjector.NewPublicErrorDetails("config.admin: removed in v5; use the api block")
	}
	if upstreamRaw, hasUpstream := probe["upstream"]; hasUpstream {
		var upstreamProbe map[string]json.RawMessage
		if err := json.Unmarshal(upstreamRaw, &upstreamProbe); err != nil {
			// the upstream block is malformed JSON; log a warning so the operator
			// knows the migration probe was skipped, then fall through — the main
			// typed decode below will surface a proper parse error.
			logger.Warn("config.upstream: failed to probe for removed fields; skipping migration check",
				slog.String("err", err.Error()))
		} else if _, hasConfigs := upstreamProbe["configs"]; hasConfigs {
			return Config{}, loginjector.NewPublicErrorDetails(
				"config.upstream.configs: removed; tunnels are now auto-discovered from " +
					filepath.Join(filepath.Dir(path), "tunnels") +
					" — delete this field",
			)
		} else {
			return Config{}, loginjector.NewPublicErrorDetails(
				"config.upstream: moved to vpnstream (allowed_countries → vpnstream.allowed_countries)",
			)
		}
	}
	// top-level key-move probes: fire for every v5 key that moved or was removed.
	// structural-first ordering so the operator sees the biggest migration step first.
	// Order matches the canonical key-move map (probes #2-10).
	if err := probeRemovedKey(probe, "listen", "config.listen: moved to vpnstream.listen"); err != nil {
		return Config{}, err
	}
	if err := probeRemovedKey(probe, "auth", "config.auth: moved to vpnstream.auth"); err != nil {
		return Config{}, err
	}
	if err := probeRemovedKey(probe, "dial_timeout", "config.dial_timeout: moved to vpnstream.dial_timeout"); err != nil {
		return Config{}, err
	}
	if err := probeRemovedKey(probe, "idle_timeout", "config.idle_timeout: moved to vpnstream.idle_timeout"); err != nil {
		return Config{}, err
	}
	if err := probeRemovedKey(probe, "shutdown_timeout", "config.shutdown_timeout: moved to vpnstream.shutdown_timeout"); err != nil {
		return Config{}, err
	}
	if err := probeRemovedKey(probe, "streaming", "config.streaming: moved to vpnstream (reconnect_min/reconnect_max)"); err != nil {
		return Config{}, err
	}
	if err := probeRemovedKey(probe, "ondemand", "config.ondemand: moved to api.vpn.demand"); err != nil {
		return Config{}, err
	}
	if _, hasHealth := probe["health"]; hasHealth {
		return Config{}, loginjector.NewPublicErrorDetails(
			"config.health: removed; handshake_max_age is now a built-in constant (tunnelpool.DefaultHandshakeMaxAge = 180s)",
		)
	}

	// nested api-block probes: unmarshal api once; skip entirely when api is absent.
	if apiRaw, hasAPI := probe["api"]; hasAPI {
		var apiProbe map[string]json.RawMessage
		if err := json.Unmarshal(apiRaw, &apiProbe); err != nil {
			// malformed api block — warn and fall through; the typed decode below
			// will surface a proper parse error.
			logger.Warn("config.api: failed to probe for removed fields; skipping migration check",
				slog.String("err", err.Error()))
		} else {
			if err := probeRemovedKey(apiProbe, "tls",
				"config.api.tls: removed; use the -tls-cert-dir, -tls-hostname, -tls-ip-sans CLI flags"); err != nil {
				return Config{}, err
			}
			if err := probeRemovedKey(apiProbe, "upstream_timeout",
				"config.api.upstream_timeout: moved to api.vpn.timeout"); err != nil {
				return Config{}, err
			}
			if err := probeRemovedKey(apiProbe, "max_upstream_timeout",
				"config.api.max_upstream_timeout: moved to api.vpn.max_timeout"); err != nil {
				return Config{}, err
			}
			if err := probeRemovedKey(apiProbe, "async",
				"config.api.async: moved to api.vpn.async (only storage_path survives; max_concurrent_jobs/pending_timeout/complete_ttl/tombstone_ttl are now built-in constants in internal/application/asyncjob)"); err != nil {
				return Config{}, err
			}

			// nested api.vpn.async probes: fire when a removed knob appears at the
			// NEW valid location api.vpn.async (operator moved the block but kept a
			// now-removed field).
			if vpnRaw, hasVPN := apiProbe["vpn"]; hasVPN {
				var vpnProbe map[string]json.RawMessage
				if err := json.Unmarshal(vpnRaw, &vpnProbe); err != nil {
					logger.Warn("config.api.vpn: failed to probe for removed fields; skipping migration check",
						slog.String("err", err.Error()))
				} else if asyncRaw, hasAsync := vpnProbe["async"]; hasAsync {
					var asyncProbe map[string]json.RawMessage
					if err := json.Unmarshal(asyncRaw, &asyncProbe); err != nil {
						logger.Warn("config.api.vpn.async: failed to probe for removed fields; skipping migration check",
							slog.String("err", err.Error()))
					} else {
						if err := probeRemovedKey(asyncProbe, "max_concurrent_jobs",
							"config.api.vpn.async.max_concurrent_jobs: removed; now a built-in constant in internal/application/asyncjob"); err != nil {
							return Config{}, err
						}
						if err := probeRemovedKey(asyncProbe, "pending_timeout",
							"config.api.vpn.async.pending_timeout: removed; now a built-in constant in internal/application/asyncjob"); err != nil {
							return Config{}, err
						}
						if err := probeRemovedKey(asyncProbe, "complete_ttl",
							"config.api.vpn.async.complete_ttl: removed; now a built-in constant in internal/application/asyncjob"); err != nil {
							return Config{}, err
						}
						if err := probeRemovedKey(asyncProbe, "tombstone_ttl",
							"config.api.vpn.async.tombstone_ttl: removed; now a built-in constant in internal/application/asyncjob"); err != nil {
							return Config{}, err
						}
					}
				}
			}

			// nested api.auth probes: fire for renamed/removed token-file keys.
			// user_token_file was renamed to proxy_token_file; deploy_token_file was
			// removed (the deploy role no longer exists). Check the rename before
			// deploy so a config that still has both old keys gets the rename message first.
			if authRaw, hasAuth := apiProbe["auth"]; hasAuth {
				var authProbe map[string]json.RawMessage
				if err := json.Unmarshal(authRaw, &authProbe); err != nil {
					logger.Warn("config.api.auth: failed to probe for removed fields; skipping migration check",
						slog.String("err", err.Error()))
				} else {
					if err := probeRemovedKey(authProbe, "user_token_file",
						"config.api.auth.user_token_file: renamed to proxy_token_file"); err != nil {
						return Config{}, err
					}
					if err := probeRemovedKey(authProbe, "deploy_token_file",
						"config.api.auth.deploy_token_file: removed; the deploy role no longer exists — the release health-check uses the admin token"); err != nil {
						return Config{}, err
					}
				}
			}
		}
	}

	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}

	cfg, err := raw.toConfig()
	if err != nil {
		return Config{}, err
	}

	// derive Dir from the absolute config path so consumers anchor relative
	// paths cwd-independently, whatever path the caller passed in.
	absPath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("resolve config path %s: %w", path, err)
	}
	cfg.Dir = filepath.Dir(absPath)

	cfg.applyDefaults()

	if err := cfg.validate(logger); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Config is the fully-parsed, validated proxy configuration.
type Config struct {
	// VPNStream configures the forward-proxy listener and streaming WireGuard tunnel.
	VPNStream VPNStream
	// AccessLog controls the rotating access-log file.
	AccessLog AccessLog
	// Operational controls the operational (stderr) structured logger.
	Operational Operational
	// API configures the HTTPS API listener (default 127.0.0.1:8888). Required in v6;
	// absent config fails validation. TLS settings are no longer part of Config —
	// they are supplied via CLI flags (-tls-cert-dir, -tls-hostname, -tls-ip-sans).
	API API
	// TunnelIDHMACKeyFile is the path to the HMAC key file used to derive stable
	// per-host tunnel ids. The path is resolved relative to the config dir by the
	// consumer. When the file is absent the consumer generates a 32-byte random key
	// and writes it at 0600. The key material (file contents) must never be logged;
	// the derived hex id is non-secret and may appear in logs and API responses.
	// Default: DefaultTunnelIDHMACKeyFile.
	TunnelIDHMACKeyFile string
	// Dir is the absolute directory holding the config file this Config was
	// loaded from. It is derived by Load, never an operator-supplied JSON key,
	// and is the anchor consumers resolve every relative config-tree path
	// against (tunnels/, the HMAC key file, the auth token files).
	Dir string
}

// VPNStream configures the forward-HTTP/CONNECT proxy listener and its upstream WireGuard
// streaming tunnel. All knobs that affect the outbound proxy path live here.
type VPNStream struct {
	// Listen is the local address the proxy binds to, in "host:port" form. Default: "127.0.0.1:7788".
	Listen string
	// AllowedCountries is an optional list of two-letter ISO country codes restricting which
	// discovered configs are eligible for the streaming role. Empty list means no filter.
	// Each code must be exactly two lowercase ASCII letters. Invalid codes fail validation.
	AllowedCountries []string
	// Auth configures optional Bearer-token authentication for the proxy listener. When both
	// Token and TokenFile are empty, auth is disabled. Token and TokenFile are mutually exclusive.
	Auth Auth
	// ReconnectMin is the initial backoff between streaming reconnect attempts. Must be > 0. Default: 10m.
	ReconnectMin time.Duration
	// ReconnectMax is the ceiling for the exponential backoff. Must be >= ReconnectMin. Default: 3h.
	ReconnectMax time.Duration
	// DialTimeout is the timeout for each outbound dial. Default: 10s.
	DialTimeout time.Duration
	// IdleTimeout is the http.Server idle-connection timeout. Default: 90s.
	IdleTimeout time.Duration
	// ShutdownTimeout is the maximum time given to graceful shutdown. Default: 15s.
	ShutdownTimeout time.Duration
}

// API configures the HTTPS API listener on port 8888. The api block is
// Required in v6; omitting it fails validation with a publicerror. TLS
// certificate settings have been moved to CLI flags; they are no longer
// part of this struct.
type API struct {
	// Listen is the bind address, "host:port" form. Default: "127.0.0.1:8888".
	Listen string
	// ShutdownTimeout bounds the API server's graceful shutdown. Default: 5s.
	// An explicit "0s" is valid and passes as 0.
	ShutdownTimeout time.Duration
	// Auth holds the token-file paths for the two API roles.
	Auth APIAuth
	// MaxRequestBodyBytes is the maximum allowed request body size. Default: 10 MiB.
	MaxRequestBodyBytes int64
	// VPN groups the on-demand VPN proxy knobs.
	VPN APIVPN
	// Log configures access-log options such as path-sanitise patterns.
	Log LogConfig
}

// APIVPN groups the on-demand VPN proxy knobs under api.vpn.
type APIVPN struct {
	// Async holds the async job storage configuration.
	Async AsyncConfig
	// Demand configures the per-request on-demand tunnel scheduler.
	Demand OnDemand
	// Timeout is the per-request timeout for upstream dialling. Must be > 0. Default: 30s.
	Timeout time.Duration
	// MaxTimeout is the ceiling callers may request. Must be > 0. Default: 5m.
	MaxTimeout time.Duration
}

// APIAuth holds the two token-file paths required by the API listener.
// Both are required when the api block is present. The file paths must
// be distinct (after filepath.Clean). File contents are read at token-load time
// by the consumer; this struct only records the paths.
type APIAuth struct {
	// ProxyTokenFile is the path to the Bearer token for the proxy role (forward-proxy requests).
	ProxyTokenFile string
	// AdminTokenFile is the path to the Bearer token for the admin role.
	AdminTokenFile string
}

// OnDemand configures the per-request on-demand tunnel scheduler. The
// scheduler owns at most one live WireGuard device, time-multiplexed
// across zones (config basenames). Zone switches obey a mandatory settle
// delay; the device is kept warm for IdleTTL after the last request.
type OnDemand struct {
	// Grace is the time the scheduler waits after the last same-zone request
	// before switching to the next zone's oldest pending job.
	// Must be > 0. Default: 10s.
	Grace time.Duration
	// SettleDelay is the mandatory pause between tearing down one on-demand
	// WireGuard device and bringing up the next. Minimum 5s enforced by
	// validation. Default: 15s.
	SettleDelay time.Duration
	// IdleTTL is how long the scheduler keeps a live device after the last
	// request before proactively tearing it down. Must be > 0. Default: 168h.
	IdleTTL time.Duration
}

// PathSanitizePattern is one compiled entry from api.log.path_sanitize_patterns.
// Pattern is the pre-compiled regex; Replacement is the Go replacement string
// (supports $1, ${name} capture-group references).
type PathSanitizePattern struct {
	// Pattern is the precompiled form of the operator-supplied regex string.
	Pattern *regexp.Regexp
	// Replacement is the substitution string, passed verbatim to
	// regexp.ReplaceAllString. Capture groups are referenced as $1 or ${name}.
	Replacement string
}

// LogConfig holds the API listener's access-log options.
type LogConfig struct {
	// PathSanitizePatterns is the ordered list of precompiled regex+replacement
	// pairs applied to the request URL path before each access-log line is
	// written. An empty slice means no scrubbing. Patterns are applied
	// left-to-right; the output of pattern N is fed into pattern N+1.
	PathSanitizePatterns []PathSanitizePattern
}

// AsyncConfig holds the async job subsystem knobs. TTL and concurrency values
// are now built-in constants in the asyncjob package; only the storage path
// remains operator-configurable.
type AsyncConfig struct {
	// StoragePath is the path to the bbolt database file. Default:
	// "/opt/vpntunnel/state/async.db".
	StoragePath string
}

// Auth configures optional Bearer-token authentication for the proxy listener.
// When both Token and TokenFile are empty, auth is disabled and the proxy serves
// requests without challenge — matching pre-v1 behaviour. Token and TokenFile are
// mutually exclusive; setting both fails validation.
type Auth struct {
	// Token is the inline Bearer token. Empty means "use TokenFile or disable auth".
	// Treat as secret — never log this value.
	Token string
	// TokenFile is a path to a file whose contents are the Bearer token.
	// Resolved relative to the directory of proxy.json by the caller.
	// Trailing whitespace (including the newline that text editors add) is trimmed
	// when the file is read.
	TokenFile string
}

// AccessLog controls the rotating access log written by observability.AccessLogger.
type AccessLog struct {
	// Path is the log file path. Default: "./logs/access.log".
	Path string
	// MaxSizeMB is the maximum file size in megabytes before rotation. Default: 100.
	MaxSizeMB int
	// MaxAgeDays is the maximum age in days of rotated files. Default: 14.
	MaxAgeDays int
	// MaxBackups is the maximum number of rotated backup files kept. Default: 7.
	MaxBackups int
	// Compress enables gzip compression of rotated files. Default: true.
	Compress bool
}

// Operational controls the slog-based operational logger written to stderr.
type Operational struct {
	// Level is the minimum log level: "debug", "info", "warn", "error". Default: "info".
	Level string
	// Format selects the slog handler: "text" or "json". Default: "text".
	Format string
}

// rawConfig mirrors Config with custom duration unmarshalling. It is used only
// during JSON decode and converted to Config immediately after.
type rawConfig struct {
	VPNStream           rawVPNStream   `json:"vpnstream"`
	AccessLog           rawAccessLog   `json:"access_log"`
	Operational         rawOperational `json:"operational"`
	API                 *rawAPI        `json:"api"`
	TunnelIDHMACKeyFile *string        `json:"tunnel_id_hmac_key_file"`
}

// rawVPNStream mirrors VPNStream for JSON unmarshalling.
type rawVPNStream struct {
	Listen           string    `json:"listen"`
	AllowedCountries []string  `json:"allowed_countries"`
	Auth             rawAuth   `json:"auth"`
	ReconnectMin     *duration `json:"reconnect_min"`
	ReconnectMax     *duration `json:"reconnect_max"`
	DialTimeout      duration  `json:"dial_timeout"`
	IdleTimeout      duration  `json:"idle_timeout"`
	ShutdownTimeout  duration  `json:"shutdown_timeout"`
}

type rawAccessLog struct {
	Path       string `json:"path"`
	MaxSizeMB  int    `json:"max_size_mb"`
	MaxAgeDays int    `json:"max_age_days"`
	MaxBackups int    `json:"max_backups"`
	Compress   *bool  `json:"compress"`
}

type rawOperational struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

// rawAPI mirrors API for JSON unmarshalling, using pointers for duration fields
// that must distinguish absent (nil → default) from explicit "0s" (stored as 0).
// TLS settings are no longer part of this struct; they come from CLI flags.
type rawAPI struct {
	Listen              string     `json:"listen"`
	ShutdownTimeout     *duration  `json:"shutdown_timeout"`
	Auth                rawAPIAuth `json:"auth"`
	MaxRequestBodyBytes int64      `json:"max_request_body_bytes"`
	VPN                 *rawAPIVPN `json:"vpn"`
	Log                 rawAPILog  `json:"log"`
}

// rawAPIVPN mirrors APIVPN for JSON unmarshalling.
type rawAPIVPN struct {
	Async      *rawAsync        `json:"async"`
	Demand     *rawAPIVPNDemand `json:"demand"`
	Timeout    duration         `json:"timeout"`
	MaxTimeout duration         `json:"max_timeout"`
}

// rawAPIVPNDemand mirrors OnDemand for JSON unmarshalling under api.vpn.demand.
type rawAPIVPNDemand struct {
	Grace       *duration `json:"grace"`
	SettleDelay *duration `json:"settle_delay"`
	IdleTTL     *duration `json:"idle_ttl"`
}

// rawAsync mirrors AsyncConfig for JSON unmarshalling. A nil pointer means the
// entire api.vpn.async block was absent; defaults are applied in applyDefaults.
// StoragePath is *string so applyDefaults can distinguish "field absent"
// (nil → apply default) from "field is explicitly empty" (pointer to "" → fail
// validation). TTL and concurrency knobs are now built-in constants in asyncjob.
type rawAsync struct {
	StoragePath *string `json:"storage_path"`
}

// rawPathSanitizePattern mirrors PathSanitizePattern before regex compilation.
type rawPathSanitizePattern struct {
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
}

// rawAPILog mirrors LogConfig for JSON unmarshalling.
type rawAPILog struct {
	PathSanitizePatterns []rawPathSanitizePattern `json:"path_sanitize_patterns"`
}

type rawAPIAuth struct {
	ProxyTokenFile string `json:"proxy_token_file"`
	AdminTokenFile string `json:"admin_token_file"`
}

type rawAuth struct {
	Token     string `json:"token"`
	TokenFile string `json:"token_file"`
}

// toConfig converts rawConfig into Config.
func (r rawConfig) toConfig() (Config, error) {
	compress := true
	if r.AccessLog.Compress != nil {
		compress = *r.AccessLog.Compress
	}

	cfg := Config{
		VPNStream: VPNStream{
			Listen:           r.VPNStream.Listen,
			AllowedCountries: r.VPNStream.AllowedCountries,
			Auth:             Auth{Token: strings.TrimSpace(r.VPNStream.Auth.Token), TokenFile: strings.TrimSpace(r.VPNStream.Auth.TokenFile)},
			DialTimeout:      time.Duration(r.VPNStream.DialTimeout),
			IdleTimeout:      time.Duration(r.VPNStream.IdleTimeout),
			ShutdownTimeout:  time.Duration(r.VPNStream.ShutdownTimeout),
			ReconnectMin:     lazyDurationAbsent,
			ReconnectMax:     lazyDurationAbsent,
		},
		AccessLog: AccessLog{
			Path:       r.AccessLog.Path,
			MaxSizeMB:  r.AccessLog.MaxSizeMB,
			MaxAgeDays: r.AccessLog.MaxAgeDays,
			MaxBackups: r.AccessLog.MaxBackups,
			Compress:   compress,
		},
		Operational: Operational{
			Level:  r.Operational.Level,
			Format: r.Operational.Format,
		},
	}

	if r.VPNStream.ReconnectMin != nil {
		cfg.VPNStream.ReconnectMin = time.Duration(*r.VPNStream.ReconnectMin)
	}
	if r.VPNStream.ReconnectMax != nil {
		cfg.VPNStream.ReconnectMax = time.Duration(*r.VPNStream.ReconnectMax)
	}

	if r.API == nil {
		return Config{}, loginjector.NewPublicErrorDetails(
			"config.api: block is required; add an api block (see configs/proxy.example.json for the required fields)",
		)
	}

	// compile api.log.path_sanitize_patterns — any compile failure is a config error.
	patterns := make([]PathSanitizePattern, 0, len(r.API.Log.PathSanitizePatterns))
	for i, rp := range r.API.Log.PathSanitizePatterns {
		if rp.Pattern == "" {
			return Config{}, loginjector.NewPublicErrorDetails(fmt.Sprintf(
				"config.api.log.path_sanitize_patterns[%d].pattern: must not be empty", i,
			))
		}
		re, err := regexp.Compile(rp.Pattern)
		if err != nil {
			return Config{}, loginjector.NewPublicErrorDetails(fmt.Sprintf(
				"config.api.log.path_sanitize_patterns[%d].pattern: invalid regex: %s", i, err.Error(),
			))
		}
		patterns = append(patterns, PathSanitizePattern{
			Pattern:     re,
			Replacement: rp.Replacement,
		})
	}

	// convert api.vpn.async — a nil block means "use the default storage path".
	// StoragePath is handled here rather than in applyDefaults: a nil pointer means
	// "absent, use default" so we apply DefaultAsyncStoragePath immediately; a
	// non-nil pointer to "" means the operator explicitly wrote "storage_path": ""
	// and is stored verbatim so validate can reject it with a field-named error.
	async := AsyncConfig{StoragePath: DefaultAsyncStoragePath}
	if r.API.VPN != nil && r.API.VPN.Async != nil {
		if r.API.VPN.Async.StoragePath != nil {
			async.StoragePath = *r.API.VPN.Async.StoragePath
		}
	}

	// build api.vpn block — nil means absent; all durations default to the absent sentinel.
	vpn := APIVPN{
		Async:      async,
		Demand:     OnDemand{Grace: lazyDurationAbsent, SettleDelay: lazyDurationAbsent, IdleTTL: lazyDurationAbsent},
		Timeout:    0,
		MaxTimeout: 0,
	}
	if r.API.VPN != nil {
		vpn.Timeout = time.Duration(r.API.VPN.Timeout)
		vpn.MaxTimeout = time.Duration(r.API.VPN.MaxTimeout)
		if r.API.VPN.Demand != nil {
			if r.API.VPN.Demand.Grace != nil {
				vpn.Demand.Grace = time.Duration(*r.API.VPN.Demand.Grace)
			}
			if r.API.VPN.Demand.SettleDelay != nil {
				vpn.Demand.SettleDelay = time.Duration(*r.API.VPN.Demand.SettleDelay)
			}
			if r.API.VPN.Demand.IdleTTL != nil {
				vpn.Demand.IdleTTL = time.Duration(*r.API.VPN.Demand.IdleTTL)
			}
		}
	}

	cfg.API = API{
		Listen: r.API.Listen,
		ShutdownTimeout: func() time.Duration {
			if r.API.ShutdownTimeout == nil {
				return apiShutdownAbsent
			}
			return time.Duration(*r.API.ShutdownTimeout)
		}(),
		Auth: APIAuth{
			ProxyTokenFile: r.API.Auth.ProxyTokenFile,
			AdminTokenFile: r.API.Auth.AdminTokenFile,
		},
		MaxRequestBodyBytes: r.API.MaxRequestBodyBytes,
		VPN:                 vpn,
		Log: LogConfig{
			PathSanitizePatterns: patterns,
		},
	}

	// apply default for tunnel_id_hmac_key_file: a nil pointer means the field
	// was absent, so we use DefaultTunnelIDHMACKeyFile; a non-nil pointer (even
	// to "") is stored verbatim so validate can reject an explicit empty string.
	if r.TunnelIDHMACKeyFile == nil {
		cfg.TunnelIDHMACKeyFile = DefaultTunnelIDHMACKeyFile
	} else {
		cfg.TunnelIDHMACKeyFile = *r.TunnelIDHMACKeyFile
	}

	return cfg, nil
}

// apiShutdownAbsent is the sentinel stored in API.ShutdownTimeout when the
// "shutdown_timeout" field is absent from a present api block. applyDefaults
// replaces it with DefaultAPIShutdownTimeout; an explicit "0s" is stored as 0
// and passes validation (>= 0 is the contract).
const apiShutdownAbsent = time.Duration(-1)

// lazyDurationAbsent is the sentinel for streaming and ondemand duration fields
// when absent from JSON (pointer nil). applyDefaults replaces each with its
// respective default. The value math.MinInt64 + 1 is used to avoid collision
// with asyncDurationAbsent while remaining equally unexpressible as a duration string.
const lazyDurationAbsent = time.Duration(math.MinInt64 + 1)

// duration is a time.Duration that unmarshals from a JSON string ("10s") or
// a raw nanosecond integer for forward-compat with json.Marshal output.
type duration time.Duration

func (d *duration) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		s, err := strconv.Unquote(string(b))
		if err != nil {
			return err
		}
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		*d = duration(parsed)
		return nil
	}
	var ns int64
	if err := json.Unmarshal(b, &ns); err != nil {
		return err
	}
	*d = duration(time.Duration(ns))
	return nil
}

func (c *Config) applyDefaults() {
	if c.VPNStream.Listen == "" {
		c.VPNStream.Listen = DefaultListen
	}
	if c.VPNStream.DialTimeout == 0 {
		c.VPNStream.DialTimeout = DefaultDialTimeout
	}
	if c.VPNStream.IdleTimeout == 0 {
		c.VPNStream.IdleTimeout = DefaultIdleTimeout
	}
	if c.VPNStream.ShutdownTimeout == 0 {
		c.VPNStream.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.AccessLog.Path == "" {
		c.AccessLog.Path = DefaultAccessLogPath
	}
	if c.AccessLog.MaxSizeMB == 0 {
		c.AccessLog.MaxSizeMB = DefaultAccessLogSizeMB
	}
	if c.AccessLog.MaxAgeDays == 0 {
		c.AccessLog.MaxAgeDays = DefaultAccessLogAgeDays
	}
	if c.AccessLog.MaxBackups == 0 {
		c.AccessLog.MaxBackups = DefaultAccessLogBackups
	}
	if c.Operational.Level == "" {
		c.Operational.Level = DefaultOperationalLevel
	}
	if c.Operational.Format == "" {
		c.Operational.Format = DefaultOperationalFormat
	}

	// vpnstream streaming defaults — sentinel marks fields absent from JSON.
	if c.VPNStream.ReconnectMin == lazyDurationAbsent {
		c.VPNStream.ReconnectMin = DefaultStreamingReconnectMin
	}
	if c.VPNStream.ReconnectMax == lazyDurationAbsent {
		c.VPNStream.ReconnectMax = DefaultStreamingReconnectMax
	}

	// api block defaults — always applied; if the api block was absent, toConfig
	// already returned an error before applyDefaults is reached.
	if c.API.ShutdownTimeout == apiShutdownAbsent {
		c.API.ShutdownTimeout = DefaultAPIShutdownTimeout
	}
	if c.API.Listen == "" {
		c.API.Listen = DefaultAPIListen
	}
	if c.API.MaxRequestBodyBytes == 0 {
		c.API.MaxRequestBodyBytes = DefaultAPIMaxRequestBodyBytes
	}
	if c.API.VPN.Timeout == 0 {
		c.API.VPN.Timeout = DefaultAPIUpstreamTimeout
	}
	if c.API.VPN.MaxTimeout == 0 {
		c.API.VPN.MaxTimeout = DefaultAPIMaxUpstreamTimeout
	}

	// async: StoragePath default is applied in toConfig so explicit "" is
	// preserved for validate to catch. TTL and concurrency values are constants
	// in the asyncjob package and require no default application here.

	// ondemand defaults — sentinel marks fields absent from JSON.
	if c.API.VPN.Demand.Grace == lazyDurationAbsent {
		c.API.VPN.Demand.Grace = DefaultOnDemandGrace
	}
	if c.API.VPN.Demand.SettleDelay == lazyDurationAbsent {
		c.API.VPN.Demand.SettleDelay = DefaultOnDemandSettleDelay
	}
	if c.API.VPN.Demand.IdleTTL == lazyDurationAbsent {
		c.API.VPN.Demand.IdleTTL = DefaultOnDemandIdleTTL
	}
}

// validate checks all required fields and constraints. It uses logger to emit
// a slog warn when vpnstream.listen or api.listen is bound to a non-loopback address.
func (c *Config) validate(logger *slog.Logger) error {
	if c.VPNStream.Listen == "" {
		return loginjector.NewPublicErrorDetails("config.vpnstream.listen: must be host:port")
	}
	host, _, err := net.SplitHostPort(c.VPNStream.Listen)
	if err != nil {
		return loginjector.NewPublicErrorDetails("config.vpnstream.listen: must be host:port")
	}
	warnNonLoopback(logger, "vpnstream.listen", host)

	if c.VPNStream.DialTimeout < 0 {
		return loginjector.NewPublicErrorDetails("config.vpnstream.dial_timeout: must be >= 0")
	}
	if c.VPNStream.IdleTimeout < 0 {
		return loginjector.NewPublicErrorDetails("config.vpnstream.idle_timeout: must be >= 0")
	}
	if c.VPNStream.ShutdownTimeout < 0 {
		return loginjector.NewPublicErrorDetails("config.vpnstream.shutdown_timeout: must be >= 0")
	}

	if c.AccessLog.Path == "" {
		return loginjector.NewPublicErrorDetails("config.access_log.path: required")
	}

	if c.VPNStream.Auth.Token != "" && c.VPNStream.Auth.TokenFile != "" {
		return loginjector.NewPublicErrorDetails("config.vpnstream.auth: token and token_file are mutually exclusive; pick one")
	}

	// allowed_countries: each entry must be exactly two lowercase ASCII letters.
	for i, cc := range c.VPNStream.AllowedCountries {
		if len(cc) != 2 || cc[0] < 'a' || cc[0] > 'z' || cc[1] < 'a' || cc[1] > 'z' {
			return loginjector.NewPublicErrorDetails(fmt.Sprintf(
				"config.vpnstream.allowed_countries[%d]: %q is not a valid two-letter lowercase country code",
				i, cc,
			))
		}
	}

	// vpnstream streaming — explicit 0s are invalid.
	if c.VPNStream.ReconnectMin <= 0 {
		return loginjector.NewPublicErrorDetails("config.vpnstream.reconnect_min: must be > 0")
	}
	if c.VPNStream.ReconnectMax <= 0 {
		return loginjector.NewPublicErrorDetails("config.vpnstream.reconnect_max: must be > 0")
	}
	if c.VPNStream.ReconnectMin > c.VPNStream.ReconnectMax {
		return loginjector.NewPublicErrorDetails(fmt.Sprintf(
			"config.vpnstream.reconnect_min (%s) must be <= reconnect_max (%s)",
			c.VPNStream.ReconnectMin, c.VPNStream.ReconnectMax,
		))
	}

	if c.API.Listen == "" {
		return loginjector.NewPublicErrorDetails("config.api.listen: must be host:port")
	}
	apiHost, _, err := net.SplitHostPort(c.API.Listen)
	if err != nil {
		return loginjector.NewPublicErrorDetails("config.api.listen: must be host:port")
	}
	warnNonLoopback(logger, "api.listen", apiHost)

	if c.API.ShutdownTimeout < 0 {
		return loginjector.NewPublicErrorDetails("config.api.shutdown_timeout: must be >= 0")
	}

	if c.API.Auth.ProxyTokenFile == "" {
		return loginjector.NewPublicErrorDetails("config.api.auth.proxy_token_file: required")
	}
	if c.API.Auth.AdminTokenFile == "" {
		return loginjector.NewPublicErrorDetails("config.api.auth.admin_token_file: required")
	}
	// token file paths must be distinct after cleaning.
	userClean := filepath.Clean(c.API.Auth.ProxyTokenFile)
	adminClean := filepath.Clean(c.API.Auth.AdminTokenFile)
	if userClean == adminClean {
		return loginjector.NewPublicErrorDetails("config.api.auth: proxy_token_file and admin_token_file resolve to the same path")
	}

	if c.API.MaxRequestBodyBytes <= 0 {
		return loginjector.NewPublicErrorDetails("config.api.max_request_body_bytes: must be > 0")
	}
	if c.API.VPN.Timeout <= 0 {
		return loginjector.NewPublicErrorDetails("config.api.vpn.timeout: must be > 0")
	}
	if c.API.VPN.MaxTimeout <= 0 {
		return loginjector.NewPublicErrorDetails("config.api.vpn.max_timeout: must be > 0")
	}
	if c.API.VPN.Timeout > c.API.VPN.MaxTimeout {
		return loginjector.NewPublicErrorDetails(fmt.Sprintf(
			"config.api.vpn.timeout (%s) must be <= max_timeout (%s)",
			c.API.VPN.Timeout, c.API.VPN.MaxTimeout,
		))
	}

	if c.API.VPN.Async.StoragePath == "" {
		return loginjector.NewPublicErrorDetails("config.api.vpn.async.storage_path: must not be empty")
	}

	if c.TunnelIDHMACKeyFile == "" {
		return loginjector.NewPublicErrorDetails("config.tunnel_id_hmac_key_file: must not be empty")
	}

	// ondemand block — explicit 0s are invalid; settle_delay has a minimum.
	if c.API.VPN.Demand.Grace <= 0 {
		return loginjector.NewPublicErrorDetails("config.api.vpn.demand.grace: must be > 0")
	}
	if c.API.VPN.Demand.SettleDelay <= 0 {
		return loginjector.NewPublicErrorDetails("config.api.vpn.demand.settle_delay: must be > 0")
	}
	const minSettleDelay = 5 * time.Second
	if c.API.VPN.Demand.SettleDelay < minSettleDelay {
		return loginjector.NewPublicErrorDetails(fmt.Sprintf(
			"config.api.vpn.demand.settle_delay: must be >= %s (got %s)",
			minSettleDelay, c.API.VPN.Demand.SettleDelay,
		))
	}
	if c.API.VPN.Demand.IdleTTL <= 0 {
		return loginjector.NewPublicErrorDetails("config.api.vpn.demand.idle_ttl: must be > 0")
	}

	return nil
}

// probeRemovedKey checks whether key is present in probe. If it is, it returns
// a loginjector.PublicDetailsError with message so the operator knows where the key moved.
// It does NOT check the value — presence alone is sufficient to fire the probe.
func probeRemovedKey(probe map[string]json.RawMessage, key, message string) error {
	if _, ok := probe[key]; ok {
		return loginjector.NewPublicErrorDetails(message)
	}
	return nil
}

// warnNonLoopback emits a slog warn when host is non-loopback. field is the
// config field name used in the log message (e.g. "vpnstream.listen" or "api.listen").
// Hostnames (e.g. "localhost") are accepted without warning; only explicit
// non-loopback IPs and the wildcard addresses trigger the warn.
func warnNonLoopback(logger *slog.Logger, field, host string) {
	if host == "" {
		return
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// hostname — assume intentional, no warn
		return
	}
	if ip.IsLoopback() {
		return
	}
	// 0.0.0.0 or :: or any other non-loopback IP
	logger.Warn("binding to a non-loopback address exposes the proxy to the network; configure auth or restrict access",
		slog.String(field, host))
}
