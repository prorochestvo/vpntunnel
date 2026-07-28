package policy

import "time"

// Default values applied when the corresponding JSON field is absent or zero.
// These are application-policy decisions, not parsing concerns: the config
// package only reads and validates the operator's file, it does not decide what
// the daemon should do when a knob is omitted.
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
