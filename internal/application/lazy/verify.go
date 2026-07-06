package lazy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"vpntunnel/internal/infrastructure/wireguard/wgconf"
	"vpntunnel/internal/publicerror"
)

// VerifySingleKey parses each config in configPaths (resolved against configDir
// when relative) and returns a *publicerror.Error when two or more distinct
// Interface.PrivateKey values are found across the set.
//
// The check is parse-only: no WireGuard device or netstack is created. It is
// cheap to run over hundreds of files.
//
// opLog receives one WARN per config that fails to parse; on a parse failure
// that config is skipped so the remaining configs are still checked. If ALL
// configs fail to parse, the returned error is a plain wrapped error rather than
// a publicerror.
//
// Key material is NEVER logged or included in any error message. The error
// reports only the count of distinct keys seen.
func VerifySingleKey(configPaths []string, configDir string, opLog *slog.Logger) error {
	fingerprints := make(map[string]struct{})
	var parseFailures int
	var lastParseErr error

	for _, cfgPath := range configPaths {
		path := cfgPath
		if !filepath.IsAbs(path) {
			path = filepath.Join(configDir, path)
		}

		parsed, err := wgconf.Parse(path, opLog)
		if err != nil {
			parseFailures++
			lastParseErr = fmt.Errorf("lazy: parse %s: %w", filepath.Base(cfgPath), err)
			opLog.Warn("VerifySingleKey: skipping unparseable config",
				slog.String("config", filepath.Base(cfgPath)),
				slog.String("error", err.Error()),
			)
			continue
		}

		fp := privateKeyFingerprint(parsed.Interface.PrivateKey)
		fingerprints[fp] = struct{}{}
	}

	if parseFailures == len(configPaths) && len(configPaths) > 0 {
		return fmt.Errorf("lazy: all configs failed to parse: %w", lastParseErr)
	}

	if len(fingerprints) > 1 {
		return publicerror.New(fmt.Sprintf(
			"tunnel config: found %d distinct WireGuard private keys across discovered configs; "+
				"all configs must share a single [Interface] PrivateKey "+
				"(one Mullvad device account); verify your config export",
			len(fingerprints),
		))
	}

	return nil
}

// privateKeyFingerprint returns a non-reversible hex fingerprint of a
// WireGuard private key. The fingerprint is used only for equality comparison
// and is never logged or exposed in error messages.
func privateKeyFingerprint(base64Key string) string {
	key, err := wgtypes.ParseKey(base64Key)
	if err != nil {
		// fall back to hashing the raw base64 string; this is a config bug
		// (wgconf.Parse should have caught it), but we must not panic here.
		sum := sha256.Sum256([]byte(base64Key))
		return "raw:" + hex.EncodeToString(sum[:8])
	}
	sum := sha256.Sum256(key[:])
	return hex.EncodeToString(sum[:8])
}
