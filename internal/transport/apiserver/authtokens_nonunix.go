//go:build !unix

package apiserver

import "os"

// checkOwnerUID is a no-op on non-Unix platforms. Production deploys are
// Linux-only; this stub exists so `go build` succeeds on Windows or other
// non-Unix development machines where syscall.Stat_t is unavailable or has a
// different shape.
func checkOwnerUID(_ os.FileInfo) error {
	return nil
}
