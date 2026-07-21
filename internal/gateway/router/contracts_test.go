package router

import "vpntunnel/internal/gateway/httpV1/handlers"

// Compile-time contract assertion: the router's default no-op async-job counter
// must satisfy handlers.AsyncJobCounter. noopCounter is unexported, so this lives
// in a white-box (package router) test file.
var _ handlers.AsyncJobCounter = noopCounter{}
