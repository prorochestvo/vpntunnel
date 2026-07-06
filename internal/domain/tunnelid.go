package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// TunnelID returns the stable per-host HMAC tunnel id for a config basename.
// basename is the .conf filename without the ".conf" suffix (the same string
// CountryFromID consumes). The returned id is lowercase hex, 64 chars, and is
// NON-secret (published in /v1/tunnels and used as the {id} path segment). key
// is key material and must never be logged.
func TunnelID(key []byte, basename string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(basename))
	return hex.EncodeToString(h.Sum(nil))
}
