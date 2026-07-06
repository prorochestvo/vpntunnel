package observability

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

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

// compile-time assertion: lumberjack.Logger implements io.WriteCloser.
var _ io.WriteCloser = (*lumberjack.Logger)(nil)

// NewAccessLogger constructs an AccessLogger that writes JSONL records to a
// rotating file described by cfg. It creates the parent directory with mode
// 0o755 if it does not exist. Returns a plain error on mkdir or file-open
// failures — not a PublicError, because these are operator startup issues.
//
// opLog is the operational slog logger used to emit a debounced warning when
// access log writes fail (e.g. full disk). Pass nil to suppress those warnings.
//
// sanitizers is an optional ordered list of path-sanitise patterns applied to
// the request target before each log line is written. Pass nil or an empty
// slice to disable sanitisation. The patterns are applied left-to-right; the
// output of pattern N is the input to pattern N+1.
//
// The caller must call Close when done to flush any buffered writes.
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

	lj := &lumberjack.Logger{
		Filename:   cfg.Path,
		MaxSize:    cfg.MaxSizeMB,
		MaxAge:     cfg.MaxAgeDays,
		MaxBackups: cfg.MaxBackups,
		Compress:   cfg.Compress,
	}

	// wrap lumberjack with an error-tracking writer so slog write failures
	// are visible to operators without flooding the operational log.
	tw := &trackingWriter{inner: lj, opLog: opLog}

	// Lumberjack opens lazily; the first Write creates the file.
	handler := slog.NewJSONHandler(tw, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler)

	return &AccessLogger{writer: lj, log: logger, sanitizers: sanitizers}, nil
}

// AccessLogger writes one JSONL record per request to a rotating file.
// It is safe for concurrent Log calls; slog and lumberjack both serialize
// their internal writes.
//
// Write errors surface as a debounced slog.Warn on the operational logger
// (at most once per minute) so a full disk does not flood the log.
type AccessLogger struct {
	writer     *lumberjack.Logger
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

// Close closes the underlying lumberjack writer. Safe to call more than once;
// subsequent calls may return an error from the underlying file but will not
// panic.
func (a *AccessLogger) Close() error {
	return a.writer.Close()
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
