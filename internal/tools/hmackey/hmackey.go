// Package hmackey manages the lifecycle of a secret HMAC key and derives
// stable, non-secret identifiers from it. LoadOrGenerate loads the key file
// (generating and persisting a fresh key at mode 0600 on first run); DeriveID
// turns key material plus a name into a deterministic lowercase-hex digest
// suitable for use as a public identifier. The key is secret material and must
// never be logged; the derived id is not.
package hmackey

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// DeriveID returns HMAC-SHA256(key, name) hex-encoded (64 lowercase chars). The
// result is deterministic for a given key+name and is safe to publish; key is
// key material and must never appear in a log, error, or format string.
func DeriveID(key []byte, name string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(name))
	return hex.EncodeToString(h.Sum(nil))
}
