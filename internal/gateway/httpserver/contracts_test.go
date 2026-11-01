package httpserver

import "vpntunnel/internal/application"

// Compile-time contract assertion: *application.ProxyService satisfies
// proxyHandler. proxyHandler is unexported, so this lives in a white-box
// (package httpserver) test file rather than the black-box server_test.go.
var _ proxyHandler = (*application.ProxyService)(nil)
