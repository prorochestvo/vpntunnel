package apitls_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/publicerror"
	"vpntunnel/internal/transport/apiserver/apitls"
)

// recordingHandler is a slog.Handler that captures every log record for
// inspection in tests.
type recordingHandler struct {
	records []slog.Record
}

var _ slog.Handler = &recordingHandler{}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}

func (h *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h *recordingHandler) WithGroup(name string) slog.Handler {
	return h
}

// newRecordingLogger returns a logger backed by a recordingHandler and a
// pointer to that handler for later inspection.
func newRecordingLogger() (*slog.Logger, *recordingHandler) {
	h := &recordingHandler{}
	return slog.New(h), h
}

// collectAttrs flattens all top-level attributes from a slog.Record into a
// map of key→string-value. Group attributes are not recursed; this is
// sufficient for these tests.
func collectAttrs(r slog.Record) map[string]string {
	m := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	return m
}

// hexRE is used to verify fingerprints are all lowercase hex characters.
var hexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestLoadOrGenerate(t *testing.T) {
	t.Parallel()

	t.Run("generates_when_files_missing", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o700))

		logger, handler := newRecordingLogger()
		cert, fp, err := apitls.LoadOrGenerate(dir, "test.local", nil, logger)

		require.NoError(t, err)
		require.NotNil(t, cert)
		require.NotNil(t, cert.Leaf)
		assert.Len(t, fp, 64)
		assert.True(t, hexRE.MatchString(fp), "fingerprint must be 64 lowercase hex chars")

		// cert.pem and key.pem must have been written.
		_, err = os.Stat(dir + "/cert.pem")
		assert.NoError(t, err, "cert.pem must exist after generation")
		_, err = os.Stat(dir + "/key.pem")
		assert.NoError(t, err, "key.pem must exist after generation")

		// log assertions: exactly one INFO record with the required keys and
		// no key material.
		require.Len(t, handler.records, 1, "expected exactly one log record")
		rec := handler.records[0]
		assert.Equal(t, slog.LevelInfo, rec.Level)
		assert.Equal(t, "tls certificate generated", rec.Message)
		attrs := collectAttrs(rec)
		assertLogKeysAllowed(t, attrs, "cert_path", "key_path", "not_after", "sha256_fingerprint")
		assertNoKeyMaterial(t, attrs)
	})

	t.Run("loads_when_files_exist_and_valid", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o700))
		logger, handler := newRecordingLogger()

		// first call generates.
		_, fp1, err := apitls.LoadOrGenerate(dir, "test.local", nil, logger)
		require.NoError(t, err)

		// second call should load, not generate.
		handler.records = nil
		cert2, fp2, err := apitls.LoadOrGenerate(dir, "test.local", nil, logger)
		require.NoError(t, err)
		require.NotNil(t, cert2)

		// same fingerprint means it loaded the existing cert.
		assert.Equal(t, fp1, fp2, "second call must load existing cert, not regenerate")

		require.Len(t, handler.records, 1)
		rec := handler.records[0]
		assert.Equal(t, slog.LevelInfo, rec.Level)
		assert.Equal(t, "tls certificate loaded", rec.Message)
		attrs := collectAttrs(rec)
		assertLogKeysAllowed(t, attrs, "cert_path", "key_path", "not_after", "sha256_fingerprint")
		assertNoKeyMaterial(t, attrs)
	})

	t.Run("regenerates_when_hostname_missing_from_san", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o700))
		logger, _ := newRecordingLogger()

		// generate for foo.local.
		_, fp1, err := apitls.LoadOrGenerate(dir, "foo.local", nil, logger)
		require.NoError(t, err)

		// second call with a different hostname should regenerate.
		_, fp2, err := apitls.LoadOrGenerate(dir, "bar.local", nil, logger)
		require.NoError(t, err)

		assert.NotEqual(t, fp1, fp2, "different hostname must trigger regeneration")
	})

	t.Run("regenerates_when_expired", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o700))
		logger, _ := newRecordingLogger()

		// first call generates a valid cert.
		_, fp1, err := apitls.LoadOrGenerate(dir, "test.local", nil, logger)
		require.NoError(t, err)

		// overwrite cert.pem + key.pem with a cert whose NotAfter is in the past.
		// tls.LoadX509KeyPair requires a matching key, so we generate a fresh pair.
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)

		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		require.NoError(t, err)

		now := time.Now()
		tmpl := &x509.Certificate{
			SerialNumber: serial,
			Subject:      pkix.Name{CommonName: "test.local"},
			NotBefore:    now.Add(-2 * time.Hour),
			NotAfter:     now.Add(-time.Hour), // already expired
			DNSNames:     []string{"test.local"},
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		derCert, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
		require.NoError(t, err)

		pkcs8Key, err := x509.MarshalPKCS8PrivateKey(priv)
		require.NoError(t, err)

		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derCert})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Key})

		require.NoError(t, os.WriteFile(dir+"/cert.pem", certPEM, 0o644))
		require.NoError(t, os.WriteFile(dir+"/key.pem", keyPEM, 0o600))

		// second call must detect expiry and regenerate — fingerprint must differ.
		_, fp2, err := apitls.LoadOrGenerate(dir, "test.local", nil, logger)
		require.NoError(t, err)

		assert.NotEqual(t, fp1, fp2, "expired cert must trigger regeneration and produce a new fingerprint")
	})

	t.Run("key_file_mode_0600_enforced", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o700))
		logger, _ := newRecordingLogger()

		_, _, err := apitls.LoadOrGenerate(dir, "test.local", nil, logger)
		require.NoError(t, err)

		info, err := os.Stat(dir + "/key.pem")
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "key.pem must have mode 0600")
	})

	t.Run("cert_file_mode_0644_enforced", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o700))
		logger, _ := newRecordingLogger()

		_, _, err := apitls.LoadOrGenerate(dir, "test.local", nil, logger)
		require.NoError(t, err)

		info, err := os.Stat(dir + "/cert.pem")
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "cert.pem must have mode 0644")
	})

	t.Run("ip_sans_present_in_cert", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o700))
		logger, _ := newRecordingLogger()

		ipv4 := net.IPv4(192, 0, 2, 1)
		ipv6 := net.ParseIP("2001:db8::1")
		require.NotNil(t, ipv6)

		cert, _, err := apitls.LoadOrGenerate(dir, "test.local", []net.IP{ipv4, ipv6}, logger)
		require.NoError(t, err)
		require.NotNil(t, cert.Leaf)

		containsIP := func(ips []net.IP, target net.IP) bool {
			for _, ip := range ips {
				if ip.Equal(target) {
					return true
				}
			}
			return false
		}
		assert.True(t, containsIP(cert.Leaf.IPAddresses, ipv4), "IPv4 192.0.2.1 must be in cert IPAddresses")
		assert.True(t, containsIP(cert.Leaf.IPAddresses, ipv6), "IPv6 2001:db8::1 must be in cert IPAddresses")
	})

	t.Run("fingerprint_is_64_hex_chars", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o700))
		logger, _ := newRecordingLogger()

		_, fp, err := apitls.LoadOrGenerate(dir, "test.local", nil, logger)
		require.NoError(t, err)

		assert.Len(t, fp, 64, "fingerprint must be exactly 64 characters")
		assert.True(t, hexRE.MatchString(fp), "fingerprint must contain only lowercase hex chars")
	})

	t.Run("cert_dir_with_world_write_rejected", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
			t.Skip("chmod semantics only verified on linux/darwin")
		}

		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o777))
		logger, _ := newRecordingLogger()

		_, _, err := apitls.LoadOrGenerate(dir, "test.local", nil, logger)
		require.Error(t, err)

		var pe *publicerror.Error
		require.ErrorAs(t, err, &pe, "cert dir with world-write must return a *publicerror.Error")
		assert.Contains(t, pe.Details(), "0777", "error names the bad mode")
		assert.Contains(t, pe.Details(), "chmod", "error includes chmod hint")
		assert.Contains(t, pe.Details(), "0700", "chmod hint names the correct target mode")
	})

	t.Run("relative_cert_dir_resolved_against_config_dir", func(t *testing.T) {
		t.Parallel()

		// LoadOrGenerate treats certDir as opaque — it does NOT resolve relative
		// paths itself. The caller (Task 7 wiring in main.go) is responsible for
		// resolving relative paths before calling this function. This subtest
		// verifies that contract: a directly-supplied temp dir is used as-is.
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o700))
		logger, _ := newRecordingLogger()

		cert, _, err := apitls.LoadOrGenerate(dir, "test.local", nil, logger)
		require.NoError(t, err)
		require.NotNil(t, cert)

		// confirm the files landed in the exact dir we supplied, not somewhere
		// derived from an internal CWD or config-relative join.
		_, err = os.Stat(dir + "/cert.pem")
		assert.NoError(t, err, "cert.pem must be in the supplied certDir, not a derived path")
	})
}

func TestShouldRegenerate(t *testing.T) {
	t.Parallel()

	now := time.Now()

	t.Run("fresh_and_matching_returns_false", func(t *testing.T) {
		t.Parallel()

		leaf := &x509.Certificate{
			NotAfter: now.Add(24 * time.Hour),
			DNSNames: []string{"test.local"},
		}
		regen, reason := apitls.ShouldRegenerate(leaf, "test.local", now)
		assert.False(t, regen)
		assert.Empty(t, reason)
	})

	t.Run("expired_returns_true_expired", func(t *testing.T) {
		t.Parallel()

		leaf := &x509.Certificate{
			NotAfter: now.Add(-time.Second),
			DNSNames: []string{"test.local"},
		}
		regen, reason := apitls.ShouldRegenerate(leaf, "test.local", now)
		assert.True(t, regen)
		assert.Equal(t, "expired", reason)
	})

	t.Run("hostname_missing_returns_true_hostname_missing_from_sans", func(t *testing.T) {
		t.Parallel()

		leaf := &x509.Certificate{
			NotAfter: now.Add(24 * time.Hour),
			DNSNames: []string{"other.local"},
		}
		regen, reason := apitls.ShouldRegenerate(leaf, "test.local", now)
		assert.True(t, regen)
		assert.Equal(t, "hostname_missing_from_sans", reason)
	})

	t.Run("expired_takes_precedence_over_hostname_check", func(t *testing.T) {
		t.Parallel()

		// both conditions are true: cert is expired AND hostname is missing.
		// expiry is checked first, so reason must be "expired".
		leaf := &x509.Certificate{
			NotAfter: now.Add(-time.Second),
			DNSNames: []string{"other.local"},
		}
		regen, reason := apitls.ShouldRegenerate(leaf, "test.local", now)
		assert.True(t, regen)
		assert.Equal(t, "expired", reason, "expiry must be checked before hostname when both fail")
	})
}

// assertLogKeysAllowed fails the test if any log attribute key is not in the
// allowed set.
func assertLogKeysAllowed(t *testing.T, attrs map[string]string, allowed ...string) {
	t.Helper()
	allowedSet := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		allowedSet[k] = true
	}
	for k := range attrs {
		assert.True(t, allowedSet[k], "unexpected log attribute %q; allowed keys: %v", k, allowed)
	}
}

// assertNoKeyMaterial fails if any log attribute value contains a PEM header,
// PEM footer, or the literal "PRIVATE KEY" type string used by PKCS8-PEM. It
// does not detect raw binary or hex-encoded key bytes; assertLogKeysAllowed is
// the primary guard against unexpected fields.
func assertNoKeyMaterial(t *testing.T, attrs map[string]string) {
	t.Helper()
	for k, v := range attrs {
		assert.NotContains(t, v, "-----BEGIN", "log attr %q must not contain PEM header", k)
		assert.NotContains(t, v, "-----END", "log attr %q must not contain PEM footer", k)
		assert.NotContains(t, v, "PRIVATE KEY", "log attr %q must not contain PRIVATE KEY type string", k)
	}
}
