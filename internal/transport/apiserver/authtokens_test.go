package apiserver

import (
	"crypto/sha512"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/config"
	"vpntunnel/internal/publicerror"
)

// hashToken returns the SHA-512 hash of plaintext. Test fixtures use this
// helper instead of hardcoding hash values so the assertion does not drift if
// the hash algorithm ever changes.
func hashToken(t *testing.T, plaintext []byte) [64]byte {
	t.Helper()
	return sha512.Sum512(plaintext)
}

// makeTokenFile writes contents to a file in dir, then chmods it to 0600.
// The explicit chmod is required because some platforms (macOS especially)
// apply umask differently, so WriteFile alone cannot be trusted to produce 0600.
func makeTokenFile(t *testing.T, dir, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	require.NoError(t, os.Chmod(path, 0o600))
	// verify the mode landed correctly before returning — so the test does not
	// pass-by-accident on a permissive umask.
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "makeTokenFile: mode not 0600 after chmod")
	return path
}

// makeAPIAuth builds a config.APIAuth referencing the provided token file paths.
func makeAPIAuth(userFile, adminFile string) config.APIAuth {
	return config.APIAuth{
		ProxyTokenFile: userFile,
		AdminTokenFile: adminFile,
	}
}

// distinctTokens returns two distinct byte slices each of exactly minLen bytes.
func distinctTokens() ([]byte, []byte) {
	mk := func(b byte) []byte {
		s := make([]byte, minTokenLen)
		for i := range s {
			s[i] = b + byte(i%26)
		}
		return s
	}
	return mk('a'), mk('A')
}

func TestLoadTokens(t *testing.T) {
	t.Parallel()

	t.Run("happy_path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tok1, tok2 := distinctTokens()
		u := makeTokenFile(t, dir, "user.token", tok1)
		a := makeTokenFile(t, dir, "admin.token", tok2)

		tokens, err := LoadTokens(makeAPIAuth(u, a), dir)
		require.NoError(t, err)
		require.NotNil(t, tokens)
	})

	t.Run("token_too_short_rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		short := make([]byte, minTokenLen-1) // 63 bytes
		for i := range short {
			short[i] = 'x'
		}
		tok1, tok2 := distinctTokens()
		// admin is the first entry loaded; use a bad user token to hit the short-path.
		a := makeTokenFile(t, dir, "admin.token", tok1)
		u := makeTokenFile(t, dir, "user.token", short)
		_ = tok2

		_, err := LoadTokens(makeAPIAuth(u, a), dir)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok, "expected publicerror, got %T: %v", err, err)
		assert.Contains(t, pe.Details(), "user.token")
		assert.Contains(t, pe.Details(), fmt.Sprintf("%d", minTokenLen-1))
		// never the token value
		assert.NotContains(t, pe.Details(), string(short))
	})

	t.Run("token_too_long_rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		long := make([]byte, maxTokenLen+1) // 513 bytes
		for i := range long {
			long[i] = 'y'
		}
		tok1, _ := distinctTokens()
		a := makeTokenFile(t, dir, "admin.token", tok1)
		u := makeTokenFile(t, dir, "user.token", long)

		_, err := LoadTokens(makeAPIAuth(u, a), dir)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok, "expected publicerror, got %T: %v", err, err)
		assert.Contains(t, pe.Details(), "user.token")
		assert.Contains(t, pe.Details(), fmt.Sprintf("%d", maxTokenLen+1))
	})

	t.Run("token_with_only_whitespace_rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// after TrimSpace the length is 0, which is < minTokenLen.
		whitespace := []byte("   \t\n   ")
		tok1, _ := distinctTokens()
		a := makeTokenFile(t, dir, "admin.token", tok1)
		u := makeTokenFile(t, dir, "user.token", whitespace)

		_, err := LoadTokens(makeAPIAuth(u, a), dir)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok, "expected publicerror, got %T: %v", err, err)
		assert.Contains(t, pe.Details(), "user.token")
		assert.Contains(t, pe.Details(), "0")
	})

	t.Run("duplicate_tokens_rejected_admin_eq_user", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tok1, _ := distinctTokens()
		// admin and user have identical content → hashes collide.
		a := makeTokenFile(t, dir, "admin.token", tok1)
		u := makeTokenFile(t, dir, "user.token", tok1)

		_, err := LoadTokens(makeAPIAuth(u, a), dir)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok, "expected publicerror, got %T: %v", err, err)
		assert.Contains(t, pe.Details(), "admin")
		assert.Contains(t, pe.Details(), "user")
	})

	t.Run("mode_world_readable_rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tok1, tok2 := distinctTokens()
		a := makeTokenFile(t, dir, "admin.token", tok1)
		u := makeTokenFile(t, dir, "user.token", tok2)
		// widen mode after the 0600 write.
		require.NoError(t, os.Chmod(u, 0o644))

		_, err := LoadTokens(makeAPIAuth(u, a), dir)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok, "expected publicerror, got %T: %v", err, err)
		assert.Contains(t, pe.Details(), "user.token")
		assert.Contains(t, pe.Details(), "0644")
		assert.Contains(t, pe.Details(), "chmod 0600")
	})

	// the owner_mismatch path in LoadTokens cannot be exercised here without
	// root (CI runners are unprivileged); manual verification on the production
	// host is the substitute. See plans/001-https-api-listener.md revision
	// history for the trade-off.

	t.Run("file_missing_rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tok1, _ := distinctTokens()
		a := makeTokenFile(t, dir, "admin.token", tok1)
		// user token file does not exist.
		missingPath := filepath.Join(dir, "ghost.token")

		_, err := LoadTokens(makeAPIAuth(missingPath, a), dir)
		require.Error(t, err)
		pe, ok := publicerror.Is(err)
		require.True(t, ok, "expected publicerror for missing file, got %T: %v", err, err)
		assert.Contains(t, pe.Details(), "ghost.token")
	})

	t.Run("stored_fields_are_hash_not_plaintext", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tok1, tok2 := distinctTokens()
		a := makeTokenFile(t, dir, "admin.token", tok1)
		u := makeTokenFile(t, dir, "user.token", tok2)

		tokens, err := LoadTokens(makeAPIAuth(u, a), dir)
		require.NoError(t, err)

		// test lives in the same package, so unexported fields are accessible directly.
		assert.Equal(t, hashToken(t, tok1), tokens.admin, "admin field must be SHA-512 hash of plaintext")
		assert.Equal(t, hashToken(t, tok2), tokens.proxy, "proxy field must be SHA-512 hash of plaintext")

		// confirm no field equals the raw plaintext (sanity — plaintext is shorter
		// than 64 bytes here so the comparison would never match, but the assertion
		// documents intent).
		var plaintext1 [64]byte
		copy(plaintext1[:], tok1)
		assert.NotEqual(t, plaintext1, tokens.admin, "admin field must not be raw plaintext")
	})
}

func TestTokens_Match(t *testing.T) {
	t.Parallel()

	// shared fixture: two distinct 64-byte tokens loaded into a *Tokens.
	tok1, tok2 := distinctTokens()
	dir := t.TempDir()
	u := makeTokenFile(t, dir, "user.token", tok1)
	a := makeTokenFile(t, dir, "admin.token", tok2)
	tokens, err := LoadTokens(makeAPIAuth(u, a), dir)
	require.NoError(t, err)

	t.Run("valid_proxy_token_resolves_to_user", func(t *testing.T) {
		t.Parallel()
		role, ok := tokens.Match(string(tok1))
		require.True(t, ok)
		assert.Equal(t, RoleProxy, role)
	})

	t.Run("valid_admin_token_resolves_to_admin", func(t *testing.T) {
		t.Parallel()
		role, ok := tokens.Match(string(tok2))
		require.True(t, ok)
		assert.Equal(t, RoleAdmin, role)
	})

	t.Run("empty_header_returns_false", func(t *testing.T) {
		t.Parallel()
		role, ok := tokens.Match("")
		assert.False(t, ok)
		assert.Equal(t, Role(""), role)
	})

	t.Run("wrong_token_returns_false", func(t *testing.T) {
		t.Parallel()
		wrong := make([]byte, minTokenLen)
		for i := range wrong {
			wrong[i] = 'Z'
		}
		role, ok := tokens.Match(string(wrong))
		assert.False(t, ok)
		assert.Equal(t, Role(""), role)
	})
}
