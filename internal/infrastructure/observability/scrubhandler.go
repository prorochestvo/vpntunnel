package observability

import (
	"context"
	"log/slog"
	"regexp"
)

// NewScrubHandler wraps next so every emitted slog.Record has its message and
// string-valued attributes (including nested groups) regex-scrubbed for
// host:port patterns. Non-string attribute kinds pass through unchanged.
// Panics if next is nil.
func NewScrubHandler(next slog.Handler) slog.Handler {
	if next == nil {
		panic("observability: NewScrubHandler requires a non-nil next handler")
	}
	return &scrubHandler{next: next}
}

// scrubHandler is an slog.Handler that redacts host:port patterns from log
// messages and string-valued attributes before forwarding to the next handler.
type scrubHandler struct {
	next slog.Handler
}

// Enabled delegates to the wrapped handler.
func (h *scrubHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle scrubs the record message and all string-valued attributes (including
// nested groups) before forwarding to the wrapped handler. It does not mutate
// the incoming record — it clones via slog.NewRecord.
func (h *scrubHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, scrubString(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(scrubAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

// WithAttrs scrubs each bound attribute once at call time, then chains to the
// wrapped handler. Pre-scrubbing at WithAttrs time avoids re-scrubbing the
// same static fields on every Handle call.
func (h *scrubHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		scrubbed = append(scrubbed, scrubAttr(a))
	}
	return &scrubHandler{next: h.next.WithAttrs(scrubbed)}
}

// WithGroup delegates to the wrapped handler, preserving group nesting.
func (h *scrubHandler) WithGroup(name string) slog.Handler {
	return &scrubHandler{next: h.next.WithGroup(name)}
}

var (
	reIPv4Port = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}:\d+\b`)
	// reIPv6Port matches only bracket-enclosed IPv6 addresses (e.g. [fe80::1]:443).
	// Raw IPv6 without brackets (e.g. fe80::1:443) is intentionally not matched
	// because Go's net.JoinHostPort always brackets IPv6, so this pattern covers
	// all stdlib-produced host:port strings without risking false positives on
	// colon-separated data that is not a port number.
	reIPv6Port = regexp.MustCompile(`\[[0-9a-fA-F:]+\]:\d+`)
	// reHostnamePort requires a non-path delimiter immediately before the hostname
	// token so that path segments like "/users.json:5" or ".pem:12" are not matched.
	// The leading delimiter character (space, quote, equals, comma, paren) is
	// captured by ReplaceAllStringFunc and preserved in the replacement.
	reHostnamePort  = regexp.MustCompile(`(?:^|[\s"'=,(])[a-zA-Z][\w.-]*\.[a-zA-Z]{2,}:\d+\b`)
	reLocalhostPort = regexp.MustCompile(`\blocalhost:\d+\b`)

	// compile-time assertion that scrubHandler implements slog.Handler.
	_ slog.Handler = (*scrubHandler)(nil)
)

const scrubReplacement = "<HOST:PORT>"

// scrubAttr recursively redacts host:port from string-valued attributes and
// group members. It resolves slog.LogValuer before inspecting kind so that
// custom types implementing LogValuer are scrubbed correctly. All other
// attribute kinds are returned unchanged.
func scrubAttr(a slog.Attr) slog.Attr {
	a.Value = a.Value.Resolve()
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, scrubString(a.Value.String()))
	case slog.KindGroup:
		sub := a.Value.Group()
		scrubbed := make([]slog.Attr, 0, len(sub))
		for _, child := range sub {
			scrubbed = append(scrubbed, scrubAttr(child))
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(scrubbed...)}
	default:
		return a
	}
}

// scrubString applies all four host:port regexes in sequence, replacing each
// match with scrubReplacement. The hostname regex uses ReplaceAllStringFunc to
// preserve any leading delimiter character that anchored the match.
func scrubString(s string) string {
	s = reIPv4Port.ReplaceAllString(s, scrubReplacement)
	s = reIPv6Port.ReplaceAllString(s, scrubReplacement)
	s = reHostnamePort.ReplaceAllStringFunc(s, func(m string) string {
		if len(m) > 0 && m[0] != ' ' && m[0] != '\t' && m[0] != '\n' &&
			m[0] != '"' && m[0] != '\'' && m[0] != '=' && m[0] != ',' && m[0] != '(' {
			// match started at position 0 in the string (^ anchor) — no delimiter to preserve
			return scrubReplacement
		}
		return string(m[0]) + scrubReplacement
	})
	s = reLocalhostPort.ReplaceAllString(s, scrubReplacement)
	return s
}
