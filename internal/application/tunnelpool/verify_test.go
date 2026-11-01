package tunnelpool

import (
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/prorochestvo/loginjector"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// confWithKey returns a minimal wg-quick .conf body using the given base64
// private key. The key must be a valid 32-byte WireGuard key in base64.
func confWithKey(privateKey string) string {
	return "[Interface]\n" +
		"PrivateKey = " + privateKey + "\n" +
		"Address = 10.64.0.2/32\n" +
		"DNS = 10.64.0.1\n\n" +
		"[Peer]\n" +
		"PublicKey = 6K+9LAzCccccccccccccccccccccccccccccccccccY=\n" +
		"Endpoint = 1.2.3.4:51820\n" +
		"AllowedIPs = 0.0.0.0/0\n"
}

// writeConf writes content to a file named "<name>.conf" in dir.
func writeConf(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := dir + "/" + name + ".conf"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// two syntactically-valid but distinct WireGuard private keys (base64, 44 chars).
// generated offline — they will not be accepted by a real WireGuard peer; only
// used to test the fingerprint-equality logic without opening a device.
const (
	keyA = "6K+9LAzCccccccccccccccccccccccccccccccccccY="
	keyB = "mPpPM1OtYXLFbcMFn8h5Ke9mWcLqPvFd3cccccccXXU="
)

func TestVerifySingleKey(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)

	t.Run("all configs share one key returns nil", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		pathA := writeConf(t, dir, "mullvad-us-nyc-wg-001", confWithKey(keyA))
		pathB := writeConf(t, dir, "mullvad-us-nyc-wg-002", confWithKey(keyA))

		err := VerifySingleKey([]string{pathA, pathB}, dir, log)
		assert.NoError(t, err)
	})

	t.Run("single config returns nil", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeConf(t, dir, "mullvad-se-sto-wg-001", confWithKey(keyA))

		err := VerifySingleKey([]string{path}, dir, log)
		assert.NoError(t, err)
	})

	t.Run("two distinct keys returns publicerror with count", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		pathA := writeConf(t, dir, "mullvad-us-nyc-wg-001", confWithKey(keyA))
		pathB := writeConf(t, dir, "mullvad-gb-lon-wg-001", confWithKey(keyB))

		err := VerifySingleKey([]string{pathA, pathB}, dir, log)
		require.Error(t, err)
		var pe loginjector.PublicDetailsError
		ok := errors.As(err, &pe)
		require.True(t, ok, "expected publicerror, got: %T %v", err, err)
		// must mention the count of distinct keys
		assert.Contains(t, pe.Details(), "2")
		// must never mention key material
		assert.NotContains(t, pe.Details(), keyA)
		assert.NotContains(t, pe.Details(), keyB)
	})

	t.Run("three configs two distinct keys returns publicerror", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		pathA := writeConf(t, dir, "mullvad-us-nyc-wg-001", confWithKey(keyA))
		pathB := writeConf(t, dir, "mullvad-us-nyc-wg-002", confWithKey(keyA))
		pathC := writeConf(t, dir, "mullvad-gb-lon-wg-001", confWithKey(keyB))

		err := VerifySingleKey([]string{pathA, pathB, pathC}, dir, log)
		require.Error(t, err)
		var pe loginjector.PublicDetailsError
		ok := errors.As(err, &pe)
		require.True(t, ok, "expected publicerror")
		assert.Contains(t, pe.Details(), "2")
	})

	t.Run("unparseable config returns wrapped plain error when all fail", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		badPath := dir + "/bad.conf"
		require.NoError(t, os.WriteFile(badPath, []byte("not a valid wg conf\n"), 0o600))

		err := VerifySingleKey([]string{badPath}, dir, log)
		require.Error(t, err)
		isPublic := errors.As(err, new(loginjector.PublicDetailsError))
		assert.False(t, isPublic, "all-parse-failure should be a plain error, got publicerror")
	})

	t.Run("one parseable and one unparseable still checks the parseable one", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		goodPath := writeConf(t, dir, "mullvad-us-nyc-wg-001", confWithKey(keyA))
		badPath := dir + "/bad.conf"
		require.NoError(t, os.WriteFile(badPath, []byte("not a valid wg conf\n"), 0o600))

		// only one valid key found — should return nil (no violation)
		err := VerifySingleKey([]string{goodPath, badPath}, dir, log)
		assert.NoError(t, err)
	})

	t.Run("empty config list returns nil", func(t *testing.T) {
		t.Parallel()
		err := VerifySingleKey([]string{}, "", log)
		assert.NoError(t, err)
	})

	t.Run("relative paths resolved against configDir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeConf(t, dir, "mullvad-se-sto-wg-001", confWithKey(keyA))

		// pass the relative name; configDir resolves it
		err := VerifySingleKey([]string{"mullvad-se-sto-wg-001.conf"}, dir, log)
		assert.NoError(t, err)
	})
}
