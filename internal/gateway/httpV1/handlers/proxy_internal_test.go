package handlers

import "vpntunnel/internal/application/asyncjob"

// Compile-time contract assertion: *asyncjob.Pool must satisfy asyncPool.
// asyncPool is unexported, so this lives in a white-box (package handlers) test
// file rather than the black-box proxy_test.go.
var _ asyncPool = (*asyncjob.Pool)(nil)
