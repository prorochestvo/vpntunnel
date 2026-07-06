//go:build unix

package router

import (
	"fmt"
	"os"
	"syscall"
)

// checkOwnerUID verifies that the file described by info is owned by the current
// process UID. Production deploys are Linux-only, so this check is meaningful in
// all real environments. Returns an error if the ownership cannot be determined or
// does not match.
func checkOwnerUID(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot determine file owner: Sys() did not return *syscall.Stat_t")
	}
	processUID := uint32(os.Getuid()) //nolint:gosec // Getuid returns a non-negative int; truncation to uint32 is safe on all supported platforms.
	if st.Uid != processUID {
		return fmt.Errorf("file UID %d does not match process UID %d", st.Uid, processUID)
	}
	return nil
}
