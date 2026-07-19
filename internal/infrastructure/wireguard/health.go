package wireguard

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ipcGetter is the subset of *device.Device used by LastHandshake.
// Defined as an interface so tests can inject a stub that returns an
// error — the live device's UAPI handler does not surface IpcGet
// errors via its public API in this wireguard-go revision.
type ipcGetter interface {
	IpcGet() (string, error)
}

// LastHandshake reports the most recent moment the WireGuard device
// completed a handshake with the peer, by parsing IpcGet output. A
// zero time.Time means no handshake has yet completed since
// NewDialer returned. See egress.HealthReporter for the contract.
//
// LastHandshake is safe for concurrent use: IpcGet acquires the
// device's internal lock. It is O(peers) and allocates one string
// per call; cheap enough for a /healthz handler even under sustained
// polling.
func (d *WireGuardDialer) LastHandshake() (time.Time, error) {
	uapi, err := d.ipc.IpcGet()
	if err != nil {
		return time.Time{}, fmt.Errorf("wireguard: ipcget: %w", err)
	}
	return parseLastHandshake(uapi)
}

// parseLastHandshake extracts the latest last_handshake_time_sec
// value across all peers in a UAPI Get response. Returns zero
// time.Time if no handshake has occurred yet (every peer reports 0)
// or if no peer block was found.
func parseLastHandshake(uapi string) (time.Time, error) {
	var maxSec int64
	sc := bufio.NewScanner(strings.NewReader(uapi))
	for sc.Scan() {
		line := sc.Text()
		const prefix = "last_handshake_time_sec="
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		n, err := strconv.ParseInt(line[len(prefix):], 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse %q: %w", line, err)
		}
		if n > maxSec {
			maxSec = n
		}
	}
	if err := sc.Err(); err != nil {
		return time.Time{}, fmt.Errorf("scan uapi: %w", err)
	}
	if maxSec == 0 {
		return time.Time{}, nil
	}
	return time.Unix(maxSec, 0), nil
}
