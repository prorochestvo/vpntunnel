package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"httpproxy/internal/config"
)

func TestResolveAuthToken(t *testing.T) {
	t.Parallel()

	t.Run("both empty returns empty string", func(t *testing.T) {
		t.Parallel()
		tok, err := resolveAuthToken(config.Auth{}, "/some/dir")
		require.NoError(t, err)
		assert.Empty(t, tok)
	})

	t.Run("inline token only is returned trimmed", func(t *testing.T) {
		t.Parallel()
		tok, err := resolveAuthToken(config.Auth{Token: "  mytoken  "}, "/some/dir")
		require.NoError(t, err)
		assert.Equal(t, "mytoken", tok)
	})

	t.Run("inline token without padding is returned as-is", func(t *testing.T) {
		t.Parallel()
		tok, err := resolveAuthToken(config.Auth{Token: "mytoken"}, "/some/dir")
		require.NoError(t, err)
		assert.Equal(t, "mytoken", tok)
	})

	t.Run("token_file with relative path is resolved against configDir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tokenPath := filepath.Join(dir, "token.txt")
		require.NoError(t, os.WriteFile(tokenPath, []byte("filetoken\n"), 0o600))

		tok, err := resolveAuthToken(config.Auth{TokenFile: "token.txt"}, dir)
		require.NoError(t, err)
		assert.Equal(t, "filetoken", tok)
	})

	t.Run("token_file with absolute path is used as-is", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tokenPath := filepath.Join(dir, "token.txt")
		require.NoError(t, os.WriteFile(tokenPath, []byte("abstoken\n"), 0o600))

		tok, err := resolveAuthToken(config.Auth{TokenFile: tokenPath}, "/different/dir")
		require.NoError(t, err)
		assert.Equal(t, "abstoken", tok)
	})

	t.Run("token_file with CRLF line ending is trimmed correctly", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tokenPath := filepath.Join(dir, "token.txt")
		require.NoError(t, os.WriteFile(tokenPath, []byte("crlftoken\r\n"), 0o600))

		tok, err := resolveAuthToken(config.Auth{TokenFile: "token.txt"}, dir)
		require.NoError(t, err)
		assert.Equal(t, "crlftoken", tok)
	})

	t.Run("unreadable token_file returns wrapped error", func(t *testing.T) {
		t.Parallel()
		_, err := resolveAuthToken(config.Auth{TokenFile: "nonexistent.txt"}, "/nonexistent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read auth.token_file")
		// error must not contain token contents (there are none, but check shape)
	})

	t.Run("token_file with whitespace-only contents returns error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tokenPath := filepath.Join(dir, "empty.txt")
		require.NoError(t, os.WriteFile(tokenPath, []byte("   \n  \t  \n"), 0o600))

		_, err := resolveAuthToken(config.Auth{TokenFile: "empty.txt"}, dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "token is empty after trim")
		// error must not contain the file contents
		assert.NotContains(t, err.Error(), "   ")
	})

	t.Run("token takes precedence when both set (defensive; config validation prevents this)", func(t *testing.T) {
		t.Parallel()
		// config.Load would have rejected this, but resolveAuthToken should handle it
		// gracefully by preferring the inline token.
		tok, err := resolveAuthToken(config.Auth{Token: "inlinetoken", TokenFile: "somefile.txt"}, "/dir")
		require.NoError(t, err)
		assert.Equal(t, "inlinetoken", tok)
	})
}
