package observability

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/prorochestvo/loginjector"

	"vpntunnel/internal/domain"
	"vpntunnel/internal/infrastructure/config"
)

// PathSanitizePattern is one regex-replacement pair applied to the request
// target before each access-log line is written. Patterns are applied in
// slice order; the output of pattern N feeds into pattern N+1.
// Replacement supports $1 and ${name} capture-group references (Go regexp
// semantics). Backslash-N (Perl-style) does not work.
type PathSanitizePattern struct {
	// Regexp is the precompiled pattern to match against the request target.
	Regexp *regexp.Regexp
	// Replacement is the substitution string passed verbatim to
	// regexp.ReplaceAllString.
	Replacement string
}

// NewAccessLogger constructs an AccessLogger that writes JSONL records to a
// size-rotated file managed by loginjector's RotatingFileHandler. It creates the
// parent directory with mode 0o755 if it does not exist. Returns a plain error
// on mkdir failures — not a PublicError, because these are operator startup
// issues.
//
// Rotation is delegated to loginjector's RotatingFileHandler with full lumberjack
// parity: the live file stays at the fixed cfg.Path (WithStableCurrentName), and
// cfg.MaxSizeMB / cfg.MaxBackups / cfg.MaxAgeDays / cfg.Compress map to
// WithMaxFileSize / WithMaxFiles / WithMaxAge / WithCompress. Rotated backups are
// indexed "<prefix>.<8hex>.log", gzipped to ".gz" when compression is enabled.
//
// opLog is the operational slog logger used to emit a debounced warning when
// access log writes fail (e.g. full disk). Pass nil to suppress those warnings.
//
// sanitizers is an optional ordered list of path-sanitise patterns applied to
// the request target before each log line is written. Pass nil or an empty
// slice to disable sanitisation. The patterns are applied left-to-right; the
// output of pattern N is the input to pattern N+1.
//
// The caller should call Close when done. It is a no-op for the current rotating
// handler (which opens and closes the file per write) but honours io.Closer if a
// future writer implementation holds a handle.
func NewAccessLogger(cfg config.AccessLog, opLog *slog.Logger, sanitizers []PathSanitizePattern) (*AccessLogger, error) {
	for i, p := range sanitizers {
		if p.Regexp == nil {
			return nil, fmt.Errorf("observability: sanitizers[%d].Regexp is nil", i)
		}
	}

	dir := filepath.Dir(cfg.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("observability: create access log directory %s: %w", dir, err)
	}

	// loginjector takes (folder, prefix) and writes "<prefix>.<8hex>.log"; derive
	// the prefix from the configured filename without its .log suffix.
	prefix := strings.TrimSuffix(filepath.Base(cfg.Path), ".log")

	// WithStableCurrentName keeps the live file at the fixed cfg.Path (e.g.
	// access.log) so operators can tail it; rotated backups are indexed.
	opts := []loginjector.RotatingFileOption{loginjector.WithStableCurrentName()}
	if cfg.MaxSizeMB > 0 {
		mb := cfg.MaxSizeMB
		if mb > 4095 { // clamp so mb<<20 fits a uint32
			mb = 4095
		}
		opts = append(opts, loginjector.WithMaxFileSize(uint32(mb)<<20))
	}
	if cfg.MaxBackups > 0 {
		opts = append(opts, loginjector.WithMaxFiles(cfg.MaxBackups))
	}
	if cfg.MaxAgeDays > 0 {
		opts = append(opts, loginjector.WithMaxAge(time.Duration(cfg.MaxAgeDays)*24*time.Hour))
	}
	if cfg.Compress {
		opts = append(opts, loginjector.WithCompress())
	}

	// A bare RotatingFileHandler writes exactly the bytes it is given (no
	// timestamp prefix, unlike loginjector's TimestampedHandler), so the file
	// stays valid JSONL — one slog JSON record per line.
	fileWriter := loginjector.RotatingFileHandler(dir, prefix, opts...)

	// wrap the rotating writer with an error-tracking writer so slog write
	// failures are visible to operators without flooding the operational log.
	tw := &trackingWriter{inner: fileWriter, opLog: opLog}

	handler := slog.NewJSONHandler(tw, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler)

	return &AccessLogger{writer: fileWriter, log: logger, sanitizers: sanitizers}, nil
}

// AccessLogger writes one JSONL record per request to a size-rotated file.
// It is safe for concurrent Log calls; slog serialises each record and
// loginjector's handler serialises its own writes.
//
// Write errors surface as a debounced slog.Warn on the operational logger
// (at most once per minute) so a full disk does not flood the log.
type AccessLogger struct {
	writer     io.Writer
	log        *slog.Logger
	sanitizers []PathSanitizePattern
}

// Log writes one JSONL record for s to the rotating access log.
// The target field is sanitised by applying each PathSanitizePattern in
// order before writing. The original s.Target is never mutated.
func (a *AccessLogger) Log(s domain.RequestSummary) {
	target := s.Target
	upstreamErr := s.UpstreamError
	for _, p := range a.sanitizers {
		target = p.Regexp.ReplaceAllString(target, p.Replacement)
		upstreamErr = p.Regexp.ReplaceAllString(upstreamErr, p.Replacement)
	}
	a.log.Info("request",
		slog.String("method", s.Method),
		slog.String("target", target),
		slog.String("client_addr", s.ClientAddr),
		slog.Int("status_code", s.StatusCode),
		slog.Int64("bytes_in", s.BytesIn),
		slog.Int64("bytes_out", s.BytesOut),
		slog.Int64("duration_ms", s.DurationMS),
		slog.String("upstream_error", upstreamErr),
	)
}

// Close releases the underlying writer if it holds resources. loginjector's
// rotating file handler opens and closes the file on each write, so there is
// nothing to flush and Close is a no-op for it; the io.Closer branch keeps the
// method correct if the writer implementation ever holds a handle. Safe to call
// more than once.
func (a *AccessLogger) Close() error {
	if c, ok := a.writer.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// trackingWriter wraps an io.Writer and emits a debounced slog.Warn on the
// operational logger when writes fail, so a full disk is operator-visible.
type trackingWriter struct {
	inner io.Writer
	opLog *slog.Logger

	mu          sync.Mutex
	lastErrWarn time.Time
}

func (w *trackingWriter) Write(p []byte) (int, error) {
	n, err := w.inner.Write(p)
	if err != nil && w.opLog != nil {
		w.mu.Lock()
		if time.Since(w.lastErrWarn) >= time.Minute {
			w.lastErrWarn = time.Now()
			w.mu.Unlock()
			w.opLog.Warn("access log writes failing", slog.String("err", err.Error()))
		} else {
			w.mu.Unlock()
		}
	}
	return n, err
}
