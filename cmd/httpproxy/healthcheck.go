package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

// healthcheckDialTimeout is the maximum time runHealthcheck waits for a
// TCP connection to be established.
const healthcheckDialTimeout = 2 * time.Second

// runHealthcheck opens a brief TCP connection to addr and returns 0 when the
// listener is reachable, 1 otherwise. It is used as a self-probe by Docker
// HEALTHCHECK; it MUST NOT read the proxy config, log to the operational sink,
// or block longer than healthcheckDialTimeout.
func runHealthcheck(addr string) int {
	conn, err := net.DialTimeout("tcp", addr, healthcheckDialTimeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	// conn.Close error is logged but not signaled — a successful dial proves
	// reachability; close-time failure is non-actionable from the prober's
	// perspective. This branch is not exercised by tests (would require a
	// net.Conn interface seam, disproportionate at this scale).
	if cerr := conn.Close(); cerr != nil {
		fmt.Fprintln(os.Stderr, "healthcheck close:", cerr)
	}
	return 0
}
