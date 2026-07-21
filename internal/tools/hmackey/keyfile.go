package hmackey

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/prorochestvo/loginjector"
)

// keyLen is the length in bytes of the HMAC key. 64 is the SHA-256 block size:
// HMAC uses a key up to the block size directly, whereas a longer key is first
// hashed down to 32 bytes — no security gain and one extra hash. 64 is
// therefore the longest key that adds entropy without an extra hashing step.
// The key is HMAC'd once per name at startup, never per request, so its length
// has no effect on request-path performance.
const keyLen = 64

// LoadOrGenerate returns the 64-byte HMAC key at path. If the file does not
// exist, a fresh 64-byte random key is generated, written to path at mode 0600,
// and returned. If the file already exists, it is stat-checked (must be 0600),
// read, and length-checked (must be exactly 64 bytes). The key bytes are never
// logged — only the file basename and key_len appear in log records.
//
// path is resolved relative to configDir when it is not absolute.
func LoadOrGenerate(path, configDir string, opLog *slog.Logger) ([]byte, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(configDir, path)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("open tunnel-id hmac key file %q: %w", path, err)
		}
		// file already exists — load it.
		return loadExistingKey(path, opLog)
	}

	// file was created — generate and write a fresh key.
	var k [keyLen]byte
	if _, err := rand.Read(k[:]); err != nil {
		// close and remove the empty placeholder we just created.
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("generate tunnel-id hmac key: %w", err)
	}
	if _, err := f.Write(k[:]); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("write tunnel-id hmac key file %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close tunnel-id hmac key file %q: %w", path, err)
	}

	opLog.Info("tunnel id hmac key generated",
		slog.String("source", filepath.Base(path)),
		slog.Int("key_len", keyLen),
	)
	return k[:], nil
}

// loadExistingKey reads, validates, and returns the 64-byte key from an
// existing file. It enforces mode 0600 and exact length before returning.
func loadExistingKey(path string, opLog *slog.Logger) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat tunnel-id hmac key file %q: %w", path, err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		return nil, loginjector.NewPublicErrorDetails(fmt.Sprintf(
			"tunnel_id_hmac_key_file: %q has mode %04o; must be 0600 — run: chmod 0600 %s",
			filepath.Base(path), perm, path,
		))
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read tunnel-id hmac key file %q: %w", path, err)
	}

	if len(raw) != keyLen {
		return nil, loginjector.NewPublicErrorDetails(fmt.Sprintf(
			"tunnel_id_hmac_key_file: %q contains %d bytes; must be exactly %d bytes",
			filepath.Base(path), len(raw), keyLen,
		))
	}

	opLog.Info("tunnel id hmac key loaded",
		slog.String("source", filepath.Base(path)),
		slog.Int("key_len", keyLen),
	)
	return raw, nil
}
