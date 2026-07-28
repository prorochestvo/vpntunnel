package hmackey

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/prorochestvo/loginjector"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadOrGenerate(t *testing.T) {
	t.Parallel()

	t.Run("generates 64-byte 0600 file when absent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "tunnel-id.key")
		var logBuf bytes.Buffer
		log := newBufLog(&logBuf)

		key, err := LoadOrGenerate(path, dir, log)
		require.NoError(t, err)
		assert.Len(t, key, keyLen, "returned key must be 64 bytes")

		// file must exist and be readable.
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "file must be mode 0600")

		// on-disk bytes must equal the returned key.
		onDisk, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, key, onDisk, "on-disk bytes must match returned key")

		// the key itself must never appear in the log.
		assertKeyNotLogged(t, &logBuf, key)
	})

	t.Run("loads existing 64-byte 0600 file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "tunnel-id.key")

		want := make([]byte, keyLen)
		for i := range want {
			want[i] = byte(i + 1)
		}
		require.NoError(t, os.WriteFile(path, want, 0o600))

		var logBuf bytes.Buffer
		log := newBufLog(&logBuf)

		key, err := LoadOrGenerate(path, dir, log)
		require.NoError(t, err)
		assert.Equal(t, want, key, "loaded key must match written bytes")

		assertKeyNotLogged(t, &logBuf, key)
	})

	t.Run("rejects wrong mode", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "tunnel-id.key")

		want := make([]byte, keyLen)
		require.NoError(t, os.WriteFile(path, want, 0o644))

		var logBuf bytes.Buffer
		log := newBufLog(&logBuf)

		_, err := LoadOrGenerate(path, dir, log)
		require.Error(t, err)

		var pe loginjector.PublicDetailsError
		require.True(t, errors.As(err, &pe), "error must be loginjector.PublicDetailsError")
		assert.Contains(t, pe.Details(), "0600", "error message must mention required mode 0600")
	})

	t.Run("rejects wrong length", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "tunnel-id.key")

		shortKey := make([]byte, 16)
		for i := range shortKey {
			shortKey[i] = byte(i + 0x41)
		}
		require.NoError(t, os.WriteFile(path, shortKey, 0o600))

		var logBuf bytes.Buffer
		log := newBufLog(&logBuf)

		_, err := LoadOrGenerate(path, dir, log)
		require.Error(t, err)

		var pe loginjector.PublicDetailsError
		require.True(t, errors.As(err, &pe), "error must be loginjector.PublicDetailsError")
		assert.Contains(t, pe.Details(), "16", "error message must mention actual byte count")

		// the raw bytes must not appear in the error message.
		for _, b := range shortKey {
			assert.NotContains(t, pe.Details(), string([]byte{b}),
				"error must not contain raw key bytes")
		}
	})

	t.Run("resolves relative path against configDir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		subdir := filepath.Join(dir, "auth")
		require.NoError(t, os.MkdirAll(subdir, 0o700))

		relPath := "auth/tunnel-id.key"
		var logBuf bytes.Buffer
		log := newBufLog(&logBuf)

		key, err := LoadOrGenerate(relPath, dir, log)
		require.NoError(t, err)
		assert.Len(t, key, keyLen)

		expectedPath := filepath.Join(dir, "auth", "tunnel-id.key")
		_, err = os.Stat(expectedPath)
		require.NoError(t, err, "key file must be created at the joined path")
	})

	t.Run("generated key is usable by DeriveID", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "tunnel-id.key")
		var logBuf bytes.Buffer
		log := newBufLog(&logBuf)

		key, err := LoadOrGenerate(path, dir, log)
		require.NoError(t, err)

		id := DeriveID(key, "se-sto-wg-001")
		assert.Len(t, id, 64, "DeriveID must return a 64-char hex string")

		matched, err := regexp.MatchString(`^[0-9a-f]{64}$`, id)
		require.NoError(t, err)
		assert.True(t, matched, "DeriveID output must be lowercase hex")
	})

	t.Run("slog output never contains the key bytes", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "tunnel-id.key")
		var logBuf bytes.Buffer
		log := newBufLog(&logBuf)

		key, err := LoadOrGenerate(path, dir, log)
		require.NoError(t, err)

		assertKeyNotLogged(t, &logBuf, key)
	})
}

// newBufLog returns a slog.Logger that writes JSON to buf and the *bytes.Buffer
// so the caller can inspect what was logged.
func newBufLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// assertKeyNotLogged checks that neither the raw key bytes nor their hex
// encoding appear anywhere in the log output written to buf.
func assertKeyNotLogged(t *testing.T, buf *bytes.Buffer, key []byte) {
	t.Helper()
	logOutput := buf.String()

	// check for raw key as a substring of the log text.
	assert.NotContains(t, logOutput, string(key), "raw key bytes must not appear in logs")

	// check for the hex-encoded key.
	hexKey := make([]byte, len(key)*2)
	const hextable = "0123456789abcdef"
	for i, b := range key {
		hexKey[i*2] = hextable[b>>4]
		hexKey[i*2+1] = hextable[b&0x0f]
	}

	assert.NotContains(t, logOutput, string(hexKey), "hex-encoded key must not appear in logs")
}
