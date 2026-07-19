package hmackey

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeriveID(t *testing.T) {
	t.Parallel()

	key32 := func(b byte) []byte {
		k := make([]byte, 32)
		for i := range k {
			k[i] = b
		}
		return k
	}

	key := key32(0xAB)

	t.Run("deterministic for same key and name", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, DeriveID(key, "se-sto-wg-001"), DeriveID(key, "se-sto-wg-001"))
	})

	t.Run("differs by name", func(t *testing.T) {
		t.Parallel()
		assert.NotEqual(t, DeriveID(key, "se-sto-wg-001"), DeriveID(key, "de-fra-wg-001"))
	})

	t.Run("differs by key", func(t *testing.T) {
		t.Parallel()
		assert.NotEqual(t, DeriveID(key32(0x01), "se-sto-wg-001"), DeriveID(key32(0x02), "se-sto-wg-001"))
	})

	t.Run("output is 64 lowercase hex chars", func(t *testing.T) {
		t.Parallel()
		id := DeriveID(key, "se-sto-wg-001")
		require.Len(t, id, 64)
		matched, err := regexp.MatchString(`^[0-9a-f]{64}$`, id)
		require.NoError(t, err)
		assert.True(t, matched, "id %q does not match ^[0-9a-f]{64}$", id)
	})

	t.Run("empty name still produces 64-char id", func(t *testing.T) {
		t.Parallel()
		id := DeriveID(key, "")
		require.Len(t, id, 64)
		matched, err := regexp.MatchString(`^[0-9a-f]{64}$`, id)
		require.NoError(t, err)
		assert.True(t, matched, "id %q does not match ^[0-9a-f]{64}$", id)
	})

	t.Run("golden value for known key and name", func(t *testing.T) {
		t.Parallel()
		// key is 32 bytes of 0xAB; name is "se-sto-wg-001". The expected value is
		// HMAC-SHA256(key, "se-sto-wg-001") hex-encoded. Pinning this catches a
		// silent algorithm or encoding change — the id is published and stable.
		const want = "49562a8a3be810d69c90a31efd305b735b5586181c6f6ccc4012fe64c5150811"
		assert.Equal(t, want, DeriveID(key32(0xAB), "se-sto-wg-001"),
			"DeriveID must match the pre-computed HMAC-SHA256 golden value")
	})
}
