// Package router implements the HTTPS API listener for the vpntunnel
// daemon: TLS setup, request-ID middleware, Bearer-token auth, role-based
// routing, and the UUIDv7 request-ID generator. Tokens and Role types are
// defined in this file; the server skeleton lives in server.go; handlers
// for /v1/admin/health, /v1/tunnels, and /v1/proxy/... live in the
// sibling httpV1/handlers package.
package router

import (
	"bytes"
	"crypto/sha512"
	"crypto/subtle"
	"fmt"
	"os"
	"path/filepath"

	"vpntunnel/internal/infrastructure/config"
	"vpntunnel/internal/publicerror"
)

// Role identifies which API role a Bearer token grants.
type Role string

// Role constants for the two API roles.
const (
	// RoleAdmin grants administrative access via the API.
	RoleAdmin Role = "admin"
	// RoleProxy grants forward-proxy access via the API.
	RoleProxy Role = "proxy"
)

// TokenRoleCount is the number of distinct role-tokens LoadTokens always loads
// (admin + user). main.go logs this at startup; keeping it as an
// exported constant means the log stays accurate if the schema ever changes.
const TokenRoleCount = 2

// LoadTokens reads the two token files configured in auth, validates each
// file's permissions and ownership, hashes the contents with SHA-512, and
// returns a *Tokens ready for use in Match calls.
//
// Behaviour:
//   - Each file must exist, have mode exactly 0600, and be owned by the
//     current process UID (Unix only; the UID check is a no-op on non-Unix
//     platforms where production deploys never run).
//   - Token plaintext must be between 64 and 512 bytes (after TrimSpace).
//     Shorter tokens increase brute-force risk from a privileged local attacker
//     on a network-exposed daemon.
//   - Both hashes must be distinct (checked via crypto/subtle.ConstantTimeCompare).
//   - Token files are read once at startup; no hot-reload in v1.
//
// Returns a *publicerror.Error for operator-correctable conditions (wrong mode,
// wrong owner, bad length, duplicate tokens). Returns a plain error for
// unexpected I/O failures.
func LoadTokens(auth config.APIAuth, configDir string) (*Tokens, error) {
	type tokenEntry struct {
		label string
		path  string
	}
	entries := []tokenEntry{
		{"admin", auth.AdminTokenFile},
		{"proxy", auth.ProxyTokenFile},
	}

	type loaded struct {
		hash     [64]byte
		resolved string
	}
	results := make([]loaded, len(entries))
	for i, e := range entries {
		h, resolved, err := loadOneToken(e.path, e.label, configDir)
		if err != nil {
			return nil, err
		}
		results[i] = loaded{hash: h, resolved: resolved}
	}

	// uniqueness check on hashes — both compares run before any decision
	// is made (constant-time discipline even on aggregate checks).
	adminEqUser := subtle.ConstantTimeCompare(results[0].hash[:], results[1].hash[:]) == 1
	if adminEqUser {
		return nil, publicerror.New(fmt.Sprintf(
			"api.auth: admin and proxy token hashes are identical (files: %q, %q)",
			filepath.Base(results[0].resolved), filepath.Base(results[1].resolved),
		))
	}

	return &Tokens{
		admin: results[0].hash,
		proxy: results[1].hash,
	}, nil
}

// Tokens retains only SHA-512 hashes of the configured admin and user tokens.
// Tokens are never logged; only SHA-512 hashes are retained in memory. Plaintext
// is zeroed immediately after hashing at load time. Match also hashes incoming
// header bytes and zeroes them before comparison.
//
// Tokens is safe for concurrent use by multiple goroutines after LoadTokens
// returns — all fields are written once at construction and are read-only thereafter.
type Tokens struct {
	admin [64]byte
	proxy [64]byte
}

// Match returns the role matched by the header, or ("", false) if no role
// matches. The header parameter arrives as a string from net/http; Match
// copies it to a mutable byte slice that is zeroed after hashing. The
// original net/http string copy lives outside this package's control and
// is not zeroed — this is an unavoidable limitation of the stdlib HTTP
// interface, not a bug in this package.
//
// Match runs both constant-time comparisons against the stored admin/user
// hashes before returning, regardless of which one (if any) matches. This
// avoids per-request timing differences that could otherwise distinguish
// "wrong-but-close" from "wrong-and-far" inputs. Future maintainers MUST NOT
// short-circuit any of the subtle.ConstantTimeCompare calls; the no-early-exit
// invariant is enforced by code review, not by a runtime timing assertion.
//
// An empty header (after TrimSpace) returns ("", false) without any comparison.
//
// Match is safe to call from multiple goroutines.
func (t *Tokens) Match(header string) (Role, bool) {
	// header arrives as a string from net/http; copy to a mutable byte slice
	// we can zero after hashing. The original string allocation in net/http
	// is out of our control.
	h := bytes.TrimSpace([]byte(header))
	if len(h) == 0 {
		return "", false
	}
	sum := sha512.Sum512(h)
	// zero the mutable copy immediately — before any branch that could retain it.
	for i := range h {
		h[i] = 0
	}
	// run both compares before returning — no early exit, so a wrong-token
	// caller cannot distinguish "doesn't match admin" from "doesn't match anything"
	// by timing.
	okAdmin := subtle.ConstantTimeCompare(sum[:], t.admin[:]) == 1
	okProxy := subtle.ConstantTimeCompare(sum[:], t.proxy[:]) == 1
	switch {
	case okAdmin:
		return RoleAdmin, true
	case okProxy:
		return RoleProxy, true
	default:
		return "", false
	}
}

// minTokenLen and maxTokenLen define the accepted plaintext token length range
// (after TrimSpace). 64 bytes is the minimum to resist brute force from a
// privileged local attacker; 512 is a generous upper bound to avoid pathological
// inputs while accepting any reasonable secret.
const (
	minTokenLen = 64
	maxTokenLen = 512
)

// loadOneToken resolves, stat-checks, reads, hashes, and zeroes one token file.
// label is the human-readable role name used in error messages (never the token
// value). Returns the SHA-512 hash of the trimmed plaintext, the resolved path,
// and an error.
func loadOneToken(path, label, configDir string) ([64]byte, string, error) {
	resolved := resolvePath(path, configDir)

	// os.Stat (not Lstat) is intentional: a symlink to a valid 0600 file owned
	// by the process UID is acceptable. The mode and owner checks apply to the
	// target file, not the symlink itself.
	info, err := os.Stat(resolved)
	if err != nil {
		return [64]byte{}, resolved, publicerror.New(fmt.Sprintf(
			"api.auth.%s_token_file: cannot stat %q: %v",
			label, filepath.Base(resolved), err,
		))
	}

	// permission check runs before read — per project security constraints.
	if perm := info.Mode().Perm(); perm != 0o600 {
		return [64]byte{}, resolved, publicerror.New(fmt.Sprintf(
			"api.auth.%s_token_file: %q has mode %04o; must be 0600 — run: chmod 0600 %s",
			label, filepath.Base(resolved), perm, resolved,
		))
	}

	// owner check runs before read — unix-only; no-op stub on non-unix.
	// project security constraint: token file must be owned by the process UID.
	if err := checkOwnerUID(info); err != nil {
		return [64]byte{}, resolved, publicerror.New(fmt.Sprintf(
			"api.auth.%s_token_file: %q is not owned by the current process UID: %v",
			label, filepath.Base(resolved), err,
		))
	}

	raw, err := os.ReadFile(resolved)
	if err != nil {
		return [64]byte{}, resolved, fmt.Errorf("api.auth.%s_token_file: read %q: %w", label, filepath.Base(resolved), err)
	}

	// trim without converting to string — string() would create an immutable copy
	// that survives the zeroing step below.
	plaintext := bytes.TrimSpace(raw)
	n := len(plaintext)

	// length check before hashing — reject obviously bad input without wasting CPU.
	// zero both slices before returning so invalid-length plaintext does not linger
	// on the heap.
	if n < minTokenLen || n > maxTokenLen {
		for i := range plaintext {
			plaintext[i] = 0
		}
		for i := range raw {
			raw[i] = 0
		}
		return [64]byte{}, resolved, publicerror.New(fmt.Sprintf(
			"api.auth.%s_token_file: %q token length is %d bytes after trimming; must be between %d and %d bytes",
			label, filepath.Base(resolved), n, minTokenLen, maxTokenLen,
		))
	}

	// hash after length validation passes.
	hash := sha512.Sum512(plaintext)
	// zero the trimmed slice immediately after hashing.
	for i := range plaintext {
		plaintext[i] = 0
	}
	// also zero raw in case it differs from plaintext (leading/trailing whitespace).
	for i := range raw {
		raw[i] = 0
	}

	return hash, resolved, nil
}

// resolvePath returns path as-is when absolute; otherwise joins it onto configDir.
// Mirrors the resolution rule used by config.Load for upstream .conf files.
func resolvePath(path, configDir string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(configDir, path)
}
