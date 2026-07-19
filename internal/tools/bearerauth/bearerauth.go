// Package bearerauth verifies HTTP Bearer credentials against a single fixed
// token. It is a reusable, transport-agnostic primitive: Verify takes the raw
// value of an Authorization-style header ("Bearer <token>") and reports whether
// it carries the expected token.
//
// Scheme matching is case-insensitive (RFC 7235 §2.1); token comparison is
// byte-exact and uses crypto/subtle.ConstantTimeCompare so the runtime does not
// leak the valid token via timing. All failure paths return false — the package
// never returns errors and never logs; those concerns belong to the caller. The
// token is secret material and must never appear in a log, error, or format
// string.
package bearerauth

import (
	"crypto/subtle"
	"strings"
)

// NewBearerVerifier returns a *BearerVerifier that accepts exactly the single
// Bearer token passed in. It panics if token is empty — constructing a verifier
// with no token is a programmer error. Callers must handle the auth-disabled
// case (empty token) before calling this and pass a non-empty token only.
func NewBearerVerifier(token string) *BearerVerifier {
	if token == "" {
		panic("bearerauth: NewBearerVerifier requires a non-empty token")
	}
	return &BearerVerifier{token: []byte(token)}
}

// BearerVerifier verifies a single-token Bearer credential. Comparison is
// constant-time to prevent timing oracles. The token is stored as bytes and
// never exposed after construction.
type BearerVerifier struct {
	token []byte
}

// Verify reports whether header is a valid "Bearer <token>" credential.
// Returns false for any malformed header, wrong scheme, or wrong token.
// Comparison is constant-time regardless of token length.
func (v *BearerVerifier) Verify(header string) bool {
	parts := strings.Fields(header)
	if len(parts) != 2 {
		return false
	}
	if !strings.EqualFold(parts[0], "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(parts[1]), v.token) == 1
}
