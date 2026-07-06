package domain

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTunnelID(t *testing.T) {
	t.Parallel()

	key32 := func(b byte) []byte {
		k := make([]byte, 32)
		for i := range k {
			k[i] = b
		}
		return k
	}

	key := key32(0xAB)

	t.Run("deterministic for same key and basename", func(t *testing.T) {
		t.Parallel()
		id1 := TunnelID(key, "se-sto-wg-001")
		id2 := TunnelID(key, "se-sto-wg-001")
		assert.Equal(t, id1, id2)
	})

	t.Run("differs by basename", func(t *testing.T) {
		t.Parallel()
		id1 := TunnelID(key, "se-sto-wg-001")
		id2 := TunnelID(key, "de-fra-wg-001")
		assert.NotEqual(t, id1, id2)
	})

	t.Run("differs by key", func(t *testing.T) {
		t.Parallel()
		keyA := key32(0x01)
		keyB := key32(0x02)
		id1 := TunnelID(keyA, "se-sto-wg-001")
		id2 := TunnelID(keyB, "se-sto-wg-001")
		assert.NotEqual(t, id1, id2)
	})

	t.Run("output is 64 lowercase hex chars", func(t *testing.T) {
		t.Parallel()
		id := TunnelID(key, "se-sto-wg-001")
		require.Len(t, id, 64)
		matched, err := regexp.MatchString(`^[0-9a-f]{64}$`, id)
		require.NoError(t, err)
		assert.True(t, matched, "id %q does not match ^[0-9a-f]{64}$", id)
	})

	t.Run("empty basename still produces 64-char id", func(t *testing.T) {
		t.Parallel()
		id := TunnelID(key, "")
		require.Len(t, id, 64)
		matched, err := regexp.MatchString(`^[0-9a-f]{64}$`, id)
		require.NoError(t, err)
		assert.True(t, matched, "id %q does not match ^[0-9a-f]{64}$", id)
	})

	t.Run("golden value for known key and basename", func(t *testing.T) {
		t.Parallel()
		// key is 32 bytes of 0xAB; basename is "se-sto-wg-001". The expected value
		// is HMAC-SHA256(key, "se-sto-wg-001") hex-encoded. Pinning this catches a
		// silent algorithm or encoding change — the id is published and stable.
		const want = "49562a8a3be810d69c90a31efd305b735b5586181c6f6ccc4012fe64c5150811"
		got := TunnelID(key32(0xAB), "se-sto-wg-001")
		assert.Equal(t, want, got, "TunnelID must match the pre-computed HMAC-SHA256 golden value")
	})
}
