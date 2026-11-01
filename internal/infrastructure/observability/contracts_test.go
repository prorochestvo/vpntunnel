package observability

import "log/slog"

// Compile-time contract assertion: scrubHandler must satisfy slog.Handler.
// scrubHandler is unexported, so this lives in a white-box (package
// observability) test file rather than the black-box observability_test files.
var _ slog.Handler = (*scrubHandler)(nil)
