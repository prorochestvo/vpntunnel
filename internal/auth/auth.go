// Package auth verifies Proxy-Authorization headers for the forward proxy.
//
// Only the Bearer scheme (RFC 6750) is supported. Scheme matching is
// case-insensitive (RFC 7235 §2.1); token comparison is byte-exact and uses
// crypto/subtle.ConstantTimeCompare so the runtime does not leak the valid
// token via timing. All failure paths return false — the package never returns
// errors and never logs; those concerns belong to the caller.
package auth

import (
	"crypto/subtle"
	"strings"
)

// Verifier checks a Proxy-Authorization header value. Returns true iff the
// header carries valid credentials. The header argument is the raw single value
// of the Proxy-Authorization header; the caller is responsible for picking one
// value when multiple are present.
type Verifier interface {
	Verify(header string) bool
}

// NewBearerVerifier returns a Verifier that accepts exactly the single Bearer
// token passed in. It panics if token is empty — constructing a verifier with
// no token is a programmer error. Callers must check the auth-disabled case
// (empty token) before calling this and pass a non-empty token only.
func NewBearerVerifier(token string) Verifier {
	if token == "" {
		panic("auth: NewBearerVerifier requires a non-empty token")
	}
	return &bearerVerifier{token: []byte(token)}
}

// bearerVerifier verifies a single-token Bearer credential. Comparison is
// constant-time to prevent timing oracles. The token is stored as bytes and
// never exposed after construction.
type bearerVerifier struct {
	token []byte
}

// Verify reports whether header is a valid `Bearer <token>` credential.
// Returns false for any malformed header, wrong scheme, or wrong token.
// Comparison is constant-time regardless of token length.
func (v *bearerVerifier) Verify(header string) bool {
	parts := strings.Fields(header)
	if len(parts) != 2 {
		return false
	}
	if !strings.EqualFold(parts[0], "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(parts[1]), v.token) == 1
}
