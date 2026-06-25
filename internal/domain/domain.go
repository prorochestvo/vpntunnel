// Package domain defines the core value types shared across the vpntunnel
// internal packages. It has no I/O, no business logic, and no imports beyond
// the standard library.
package domain

// RequestSummary holds the per-request fields written to the access log.
// Both HandleHTTP and HandleCONNECT populate this type and pass it to
// observability.AccessLogger.Log after the request completes.
type RequestSummary struct {
	// Method is the HTTP method: "GET", "POST", "CONNECT", etc.
	Method string
	// Target is the absolute URL for plain HTTP requests, or "host:port"
	// for CONNECT requests.
	Target string
	// ClientAddr is r.RemoteAddr from the original request.
	ClientAddr string
	// StatusCode is the HTTP status sent to the client. For CONNECT
	// requests the 200 is sent before hijacking and then set to 0 here
	// to indicate the tunnel was established; transport errors use 502.
	StatusCode int
	// BytesIn is the number of bytes received from the client body.
	BytesIn int64
	// BytesOut is the number of bytes sent to the client body.
	BytesOut int64
	// DurationMS is the request duration in milliseconds.
	DurationMS int64
	// UpstreamError is the upstream error string, empty on success.
	UpstreamError string
}
