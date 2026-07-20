package observability

import (
	"io"
	"log/slog"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// Compile-time contract assertions for the observability implementations.
// scrubHandler is unexported, so these live in a white-box (package
// observability) test file rather than the black-box observability_test files.
var (
	_ slog.Handler   = (*scrubHandler)(nil)
	_ io.WriteCloser = (*lumberjack.Logger)(nil)
)
