package router

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"
)

// newRequestID returns a UUIDv7 string in canonical 8-4-4-4-12 hex format.
//
// Layout per RFC 9562:
//   - bytes 0-5:  big-endian Unix milliseconds (48 bits)
//   - bytes 6-7:  version (4 bits = 0x7) + 12 random bits
//   - bytes 8-9:  variant (2 bits = 0b10) + 14 random bits
//   - bytes 10-15: random
func newRequestID() string {
	var b [16]byte

	// encode the 48-bit millisecond timestamp in the top 6 bytes of the
	// 8-byte big-endian uint64, then overwrite the lower 2 bytes with random.
	ms := uint64(time.Now().UnixMilli()) & 0x0000_FFFF_FFFF_FFFF
	binary.BigEndian.PutUint64(b[:8], ms<<16)

	// fill bytes 6-15 with random data; this covers the version nibble area
	// (bytes 6-7) and the full random tail (bytes 8-15).
	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand should never fail; if it does, the server cannot safely
		// generate unique request IDs and must not continue silently.
		panic("crypto/rand: " + err.Error())
	}

	// set version 7 in byte 6, top 4 bits.
	b[6] = (b[6] & 0x0F) | 0x70
	// set RFC 9562 variant (0b10) in byte 8, top 2 bits.
	b[8] = (b[8] & 0x3F) | 0x80

	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
