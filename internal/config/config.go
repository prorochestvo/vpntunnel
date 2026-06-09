// Package config loads and validates the proxy's JSON configuration file.
// Validation errors that the operator must fix are returned as
// *publicerror.Error; I/O and JSON parse failures are plain errors.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"httpproxy/internal/publicerror"
)

// Default values applied when the corresponding JSON field is absent or zero.
const (
	DefaultListen            = "127.0.0.1:8080"
	DefaultDialTimeout       = 10 * time.Second
	DefaultIdleTimeout       = 90 * time.Second
	DefaultShutdownTimeout   = 15 * time.Second
	DefaultAccessLogPath     = "./logs/access.log"
	DefaultAccessLogSizeMB   = 100
	DefaultAccessLogAgeDays  = 14
	DefaultAccessLogBackups  = 7
	DefaultOperationalLevel  = "info"
	DefaultOperationalFormat = "text"

	DefaultAdminListen           = "127.0.0.1:8081"
	DefaultAdminShutdownTimeout  = 5 * time.Second
	DefaultHealthHandshakeMaxAge = 180 * time.Second
)

// Load reads the JSON config at path, applies defaults, and validates the result.
// It returns a *publicerror.Error for fields the operator must correct, or a
// plain wrapped error for I/O and JSON failures.
func Load(path string) (Config, error) {
	return LoadWithLogger(path, slog.Default())
}

// LoadWithLogger is Load with an explicit slog.Logger for the non-loopback
// bind warning. Useful in tests that capture log output.
func LoadWithLogger(path string, logger *slog.Logger) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var raw rawConfig
	if err := json.NewDecoder(f).Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}

	cfg := raw.toConfig()
	cfg.applyDefaults()

	if err := cfg.validate(logger); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Config is the fully-parsed, validated proxy configuration.
type Config struct {
	// Listen is the local address the proxy binds to, in "host:port" form.
	// Default: "127.0.0.1:8080".
	Listen string
	// Upstream selects the WireGuard tunnel the proxy egresses through.
	Upstream Upstream
	// DialTimeout is the timeout for each outbound dial. Default: 10s.
	DialTimeout time.Duration
	// IdleTimeout is the http.Server idle-connection timeout. Default: 90s.
	IdleTimeout time.Duration
	// ShutdownTimeout is the maximum time given to graceful shutdown. Default: 15s.
	ShutdownTimeout time.Duration
	// AccessLog controls the rotating access-log file.
	AccessLog AccessLog
	// Operational controls the operational (stderr) structured logger.
	Operational Operational
	// Admin configures the admin/probe listener (default 127.0.0.1:8081).
	Admin Admin
	// Health configures the /healthz liveness probe.
	Health Health
	// Auth configures optional Bearer-token authentication for the proxy listener.
	Auth Auth
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

// Admin configures the admin/probe listener (default 127.0.0.1:8081). The
// admin listener serves the /healthz liveness probe and nothing else. It is
// intentionally separate from the proxy listener; binding it to 0.0.0.0 turns
// /healthz into a tunnel-status oracle for any network attacker, so leave it
// on loopback unless you know what you're doing.
type Admin struct {
	// Listen is the bind address, "host:port" form.
	// Default: "127.0.0.1:8081".
	Listen string
	// ShutdownTimeout bounds the admin server's graceful shutdown.
	// Default: 5s. Independent of the proxy's ShutdownTimeout.
	ShutdownTimeout time.Duration
}

// Health configures the /healthz liveness probe.
type Health struct {
	// HandshakeMaxAge is the maximum age of the last WireGuard
	// handshake before /healthz returns 503. Default: 180s, which
	// is ~3x the 25s persistent keepalive plus a safety margin so
	// quiet tunnels don't flap.
	HandshakeMaxAge time.Duration
}

// Upstream selects the WireGuard tunnel the proxy egresses through.
//
// Configs is a list of paths to wg-quick(8) .conf files. Paths are
// resolved relative to the directory containing proxy.json (NOT the
// process cwd — systemd / cron set cwd to /). At least one entry is
// required.
//
// Active is the basename (without ".conf") of the entry in Configs to
// use. If empty, Configs[0] is used. If non-empty and no Configs entry
// has a matching basename, validation fails.
type Upstream struct {
	// Configs is a list of paths to wg-quick .conf files; at least one required.
	Configs []string `json:"configs"`
	// Active is the basename (without ".conf") of the tunnel to use.
	// Empty means use Configs[0].
	Active string `json:"active"`
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
	Listen          string         `json:"listen"`
	Upstream        rawUpstream    `json:"upstream"`
	DialTimeout     duration       `json:"dial_timeout"`
	IdleTimeout     duration       `json:"idle_timeout"`
	ShutdownTimeout duration       `json:"shutdown_timeout"`
	AccessLog       rawAccessLog   `json:"access_log"`
	Operational     rawOperational `json:"operational"`
	Admin           rawAdmin       `json:"admin"`
	Health          rawHealth      `json:"health"`
	Auth            rawAuth        `json:"auth"`
}

type rawUpstream struct {
	Configs []string `json:"configs"`
	Active  string   `json:"active"`
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

type rawAdmin struct {
	Listen string `json:"listen"`
	// ShutdownTimeout uses a pointer so we can distinguish absent (nil → apply
	// default) from explicit zero ("0s" → 0, validated as >= 0). JSON null is
	// treated as absent and receives the default.
	ShutdownTimeout *duration `json:"shutdown_timeout"`
}

type rawHealth struct {
	// HandshakeMaxAge uses a pointer so we can distinguish absent (nil → apply
	// default) from explicit zero ("0s" → fail validation).
	HandshakeMaxAge *duration `json:"handshake_max_age"`
}

type rawAuth struct {
	Token     string `json:"token"`
	TokenFile string `json:"token_file"`
}

func (r rawConfig) toConfig() Config {
	compress := true
	if r.AccessLog.Compress != nil {
		compress = *r.AccessLog.Compress
	}

	return Config{
		Listen: r.Listen,
		Upstream: Upstream{
			Configs: r.Upstream.Configs,
			Active:  r.Upstream.Active,
		},
		DialTimeout:     time.Duration(r.DialTimeout),
		IdleTimeout:     time.Duration(r.IdleTimeout),
		ShutdownTimeout: time.Duration(r.ShutdownTimeout),
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
		Admin: Admin{
			Listen: r.Admin.Listen,
			ShutdownTimeout: func() time.Duration {
				if r.Admin.ShutdownTimeout == nil {
					return adminShutdownAbsent
				}
				return time.Duration(*r.Admin.ShutdownTimeout)
			}(),
		},
		Health: Health{
			HandshakeMaxAge: func() time.Duration {
				if r.Health.HandshakeMaxAge == nil {
					return healthMaxAgeAbsent
				}
				return time.Duration(*r.Health.HandshakeMaxAge)
			}(),
		},
		Auth: Auth{
			Token:     strings.TrimSpace(r.Auth.Token),
			TokenFile: strings.TrimSpace(r.Auth.TokenFile),
		},
	}
}

// healthMaxAgeAbsent is the sentinel value stored in Health.HandshakeMaxAge
// when the "handshake_max_age" JSON field is absent from the config file.
// applyDefaults replaces it with DefaultHealthHandshakeMaxAge; validate
// rejects non-positive values, so an explicit "0s" in JSON is rejected.
const healthMaxAgeAbsent = time.Duration(-1)

// adminShutdownAbsent is the sentinel value stored in Admin.ShutdownTimeout
// when the "shutdown_timeout" JSON field is absent from the admin block.
// applyDefaults replaces it with DefaultAdminShutdownTimeout; an explicit
// "0s" is stored as 0 and passes validation (>= 0 is the contract).
const adminShutdownAbsent = time.Duration(-1)

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
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = DefaultDialTimeout
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
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
	if c.Admin.Listen == "" {
		c.Admin.Listen = DefaultAdminListen
	}
	if c.Admin.ShutdownTimeout == adminShutdownAbsent {
		c.Admin.ShutdownTimeout = DefaultAdminShutdownTimeout
	}
	if c.Health.HandshakeMaxAge == healthMaxAgeAbsent {
		c.Health.HandshakeMaxAge = DefaultHealthHandshakeMaxAge
	}
}

// validate checks all required fields and constraints. It uses logger to emit
// a slog warn when Listen is bound to a non-loopback address.
func (c *Config) validate(logger *slog.Logger) error {
	if c.Listen == "" {
		return publicerror.New("config.listen: must be host:port")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return publicerror.New("config.listen: must be host:port")
	}
	warnNonLoopback(logger, host)

	if len(c.Upstream.Configs) == 0 {
		return publicerror.New("config.upstream.configs: at least one .conf path required; download a wg-quick config and add its path here")
	}
	for i, p := range c.Upstream.Configs {
		if p == "" {
			return publicerror.New(fmt.Sprintf("config.upstream.configs[%d]: path must be non-empty", i))
		}
	}
	if c.Upstream.Active != "" {
		found := false
		for _, p := range c.Upstream.Configs {
			if strings.TrimSuffix(filepath.Base(p), ".conf") == c.Upstream.Active {
				found = true
				break
			}
		}
		if !found {
			names := make([]string, 0, len(c.Upstream.Configs))
			for _, p := range c.Upstream.Configs {
				names = append(names, strings.TrimSuffix(filepath.Base(p), ".conf"))
			}
			return publicerror.New(fmt.Sprintf(
				"config.upstream.active: %q not found in configs; available: [%s]",
				c.Upstream.Active,
				strings.Join(names, ", "),
			))
		}
	}

	if c.DialTimeout < 0 {
		return publicerror.New("config.dial_timeout: must be >= 0")
	}
	if c.IdleTimeout < 0 {
		return publicerror.New("config.idle_timeout: must be >= 0")
	}
	if c.ShutdownTimeout < 0 {
		return publicerror.New("config.shutdown_timeout: must be >= 0")
	}

	if c.AccessLog.Path == "" {
		return publicerror.New("config.access_log.path: required")
	}

	if c.Admin.Listen == "" {
		return publicerror.New("config.admin.listen: must be host:port")
	}
	if _, _, err := net.SplitHostPort(c.Admin.Listen); err != nil {
		return publicerror.New("config.admin.listen: must be host:port")
	}
	if c.Admin.ShutdownTimeout < 0 {
		return publicerror.New("config.admin.shutdown_timeout: must be >= 0")
	}
	if c.Health.HandshakeMaxAge <= 0 {
		return publicerror.New("config.health.handshake_max_age: must be > 0")
	}

	if c.Auth.Token != "" && c.Auth.TokenFile != "" {
		return publicerror.New("config.auth: token and token_file are mutually exclusive; pick one")
	}

	return nil
}

// warnNonLoopback emits a slog warn when host is non-loopback. Hostnames
// (e.g. "localhost") are accepted without warning; only explicit non-loopback
// IPs and the wildcard addresses trigger the warn.
func warnNonLoopback(logger *slog.Logger, host string) {
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
		slog.String("listen", host))
}
