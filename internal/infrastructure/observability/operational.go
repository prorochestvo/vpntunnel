// Package observability wires the two logging outputs: an operational slog
// logger (stdout, human-readable or JSON) and a rotating JSONL access log
// via loginjector. The two loggers are independent — different audiences,
// different formats.
package observability

import (
	"log/slog"
	"os"

	"vpntunnel/internal/infrastructure/config"
)

// NewOperationalLogger builds a *slog.Logger from cfg that writes to stdout.
// cfg.Format selects "text" (default) or "json"; cfg.Level sets the minimum
// level. An unknown level defaults to slog.LevelInfo with a warn emitted on
// the returned logger.
func NewOperationalLogger(cfg config.Operational) *slog.Logger {
	level := parseLevel(cfg.Level)

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	switch cfg.Format {
	case "json":
		handler = slog.NewJSONHandler(os.Stdout, opts)
	default:
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	logger := slog.New(handler)

	if _, ok := knownLevels[cfg.Level]; !ok && cfg.Level != "" {
		logger.Warn("unknown operational log level, defaulting to info",
			slog.String("level", cfg.Level))
	}

	return logger
}

var knownLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

func parseLevel(s string) slog.Level {
	if l, ok := knownLevels[s]; ok {
		return l
	}
	return slog.LevelInfo
}
