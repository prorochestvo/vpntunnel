// Package apitls manages TLS certificates for the HTTPS API listener.
//
// v1 uses self-signed certificates only. Clients will see a verification error
// unless they explicitly trust the certificate (e.g. via --cacert or by
// pinning the SHA-256 fingerprint). CA bundle distribution is intentionally out
// of scope for v1. Operators should distribute the fingerprint out-of-band and
// use the fingerprint to pin the certificate on the client side.
//
// On stale certificate regeneration: when an existing cert fails the hostname
// or expiry check, it is overwritten in place. This is intentional — a cert
// that fails those checks is already untrusted by anyone, so there is nothing
// to preserve.
//
// LoadOrGenerate is a one-shot startup function and is NOT safe for concurrent
// use against the same certDir. Two callers racing on the same directory may
// both attempt to write cert.pem and key.pem, producing a torn state. Call it
// once per process during startup under a single goroutine.
package apitls

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/prorochestvo/loginjector"
)

// LoadOrGenerate loads the TLS certificate from certDir (using cert.pem and
// key.pem) or generates a new self-signed ed25519 certificate if the files are
// missing, expired, or do not list hostname in their SANs.
//
// certDir is used as-is; the caller is responsible for resolving relative paths
// against the proxy.json config directory before calling this function.
//
// Returns the certificate (ready for tls.Config.Certificates), the SHA-256
// fingerprint of the DER certificate as a lowercase 64-character hex string,
// and an error.
//
// Returns a loginjector.PublicDetailsError for operator-correctable conditions (wrong
// certDir permissions). Returns a plain error for unexpected I/O or crypto
// failures.
//
// LoadOrGenerate is NOT safe for concurrent use against the same certDir.
func LoadOrGenerate(certDir, hostname string, ipSANs []net.IP, log *slog.Logger) (*tls.Certificate, string, error) {
	if err := ensureCertDir(certDir); err != nil {
		return nil, "", err
	}

	certPath := filepath.Join(certDir, "cert.pem")
	keyPath := filepath.Join(certDir, "key.pem")

	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)

	if certExists && keyExists {
		cert, fp, err := loadAndVerify(certPath, keyPath, hostname, log)
		if err == nil {
			log.Info("tls certificate loaded",
				"cert_path", certPath,
				"key_path", keyPath,
				"not_after", cert.Leaf.NotAfter,
				"sha256_fingerprint", fp,
			)
			return cert, fp, nil
		}
		// err here is only a sentinel indicating we should regenerate; the
		// specific warn was already emitted inside loadAndVerify.
	}

	return generate(certPath, keyPath, hostname, ipSANs, log)
}

// ShouldRegenerate reports whether the loaded certificate is expired or missing
// the hostname from its SANs. The second return is the reason string, one of
// "expired" or "hostname_missing_from_sans", or empty string when the
// certificate is valid.
//
// Clock-skew tolerance (the NotBefore offset applied during generation) is not
// applied here — that offset is only for cert creation to handle clients with
// slightly fast clocks at the moment the cert is first seen.
//
// ShouldRegenerate is exported so tests can exercise the predicate in isolation
// without going through the filesystem. Production code uses time.Now() directly.
func ShouldRegenerate(leaf *x509.Certificate, hostname string, now time.Time) (regen bool, reason string) {
	if !now.Before(leaf.NotAfter) {
		return true, "expired"
	}
	for _, d := range leaf.DNSNames {
		if d == hostname {
			return false, ""
		}
	}
	return true, "hostname_missing_from_sans"
}

// ensureCertDir creates certDir with mode 0700 if it does not exist, or
// verifies that the existing directory has exactly mode 0700.
func ensureCertDir(certDir string) error {
	info, err := os.Stat(certDir)
	if os.IsNotExist(err) {
		if mkErr := os.MkdirAll(certDir, 0o700); mkErr != nil {
			return fmt.Errorf("apitls: create cert dir %q: %w", certDir, mkErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("apitls: stat cert dir %q: %w", certDir, err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		return loginjector.NewPublicErrorDetails(fmt.Sprintf(
			"apitls: cert dir %q has mode %04o; must be 0700 — run: chmod 0700 %s",
			certDir, perm, certDir,
		))
	}
	return nil
}

// loadAndVerify loads the certificate pair from certPath/keyPath, parses the
// leaf, and calls ShouldRegenerate. If the cert should be regenerated it logs
// a warning and returns a non-nil error (the error value itself is not
// meaningful to callers; it is only used as a sentinel by LoadOrGenerate).
func loadAndVerify(certPath, keyPath, hostname string, log *slog.Logger) (*tls.Certificate, string, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, "", fmt.Errorf("load: %w", err)
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, "", fmt.Errorf("parse leaf: %w", err)
	}
	cert.Leaf = leaf

	fp := fingerprintDER(cert.Certificate[0])

	if regen, reason := ShouldRegenerate(leaf, hostname, time.Now()); regen {
		log.Warn("tls certificate invalid, regenerating",
			"cert_path", certPath,
			"key_path", keyPath,
			"reason", reason,
		)
		return nil, "", fmt.Errorf("sentinel: %s", reason)
	}

	return &cert, fp, nil
}

// generate creates a new self-signed ed25519 certificate, writes it atomically
// to certPath and keyPath, and returns the loaded *tls.Certificate plus its
// SHA-256 fingerprint.
func generate(certPath, keyPath, hostname string, ipSANs []net.IP, log *slog.Logger) (*tls.Certificate, string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("apitls: generate ed25519 key: %w", err)
	}

	max := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, "", fmt.Errorf("apitls: generate serial number: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		DNSNames:     []string{hostname},
		IPAddresses:  ipSANs,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         false,
		// BasicConstraintsValid must be true so IsCA=false is encoded in the
		// certificate per RFC 5280 §4.2.1.9. Without it the IsCA flag is
		// meaningless.
		BasicConstraintsValid: true,
	}

	derCert, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, "", fmt.Errorf("apitls: create certificate: %w", err)
	}

	leaf, err := x509.ParseCertificate(derCert)
	if err != nil {
		return nil, "", fmt.Errorf("apitls: parse generated certificate: %w", err)
	}

	if err := writePEMFiles(certPath, keyPath, derCert, priv); err != nil {
		return nil, "", err
	}

	fp := fingerprintDER(derCert)

	log.Info("tls certificate generated",
		"cert_path", certPath,
		"key_path", keyPath,
		"not_after", leaf.NotAfter,
		"sha256_fingerprint", fp,
	)

	cert := &tls.Certificate{
		Certificate: [][]byte{derCert},
		PrivateKey:  priv,
		Leaf:        leaf,
	}
	return cert, fp, nil
}

// writePEMFiles writes cert and key atomically using temp files + rename. On
// any failure it attempts to clean up the temp files before returning.
func writePEMFiles(certPath, keyPath string, derCert []byte, priv ed25519.PrivateKey) error {
	certTmp := certPath + ".tmp"
	keyTmp := keyPath + ".tmp"

	// best-effort cleanup of temp files on failure.
	cleanup := func() {
		_ = os.Remove(certTmp)
		_ = os.Remove(keyTmp)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: derCert,
	})

	if err := os.WriteFile(certTmp, certPEM, 0o644); err != nil {
		cleanup()
		return fmt.Errorf("apitls: write cert temp file: %w", err)
	}

	pkcs8Key, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		cleanup()
		return fmt.Errorf("apitls: marshal private key: %w", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8Key,
	})

	if err := os.WriteFile(keyTmp, keyPEM, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("apitls: write key temp file: %w", err)
	}

	if err := os.Rename(certTmp, certPath); err != nil {
		cleanup()
		return fmt.Errorf("apitls: rename cert file: %w", err)
	}

	if err := os.Rename(keyTmp, keyPath); err != nil {
		// certPath is already in place; only remove keyTmp.
		_ = os.Remove(keyTmp)
		return fmt.Errorf("apitls: rename key file: %w", err)
	}

	// os.WriteFile sets the mode only at creation; if keyTmp already existed
	// (e.g. leftover from a previous crashed run), the mode may be wrong.
	// An explicit chmod after rename ensures the final file always has 0600.
	if err := os.Chmod(keyPath, 0o600); err != nil {
		return fmt.Errorf("apitls: chmod key file: %w", err)
	}

	return nil
}

// fingerprintDER returns the SHA-256 digest of the DER-encoded certificate as
// a lowercase hex string (64 characters). This is the standard fingerprint
// format used by TLS inspection tools.
func fingerprintDER(derCert []byte) string {
	sum := sha256.Sum256(derCert)
	return hex.EncodeToString(sum[:])
}

// fileExists reports whether path exists (any file type).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
