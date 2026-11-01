package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTunnelID(t *testing.T) {
	t.Parallel()

	valid := strings.Repeat("a", 64)

	t.Run("accepts 64 lowercase hex", func(t *testing.T) {
		t.Parallel()
		id, err := ParseTunnelID(valid)
		require.NoError(t, err)
		assert.Equal(t, valid, id.String())
	})

	t.Run("rejects malformed inputs", func(t *testing.T) {
		t.Parallel()
		for _, s := range []string{
			"",                            // empty
			strings.Repeat("a", 63),       // too short
			strings.Repeat("a", 65),       // too long
			strings.Repeat("A", 64),       // uppercase
			strings.Repeat("g", 64),       // non-hex letter
			strings.Repeat("a", 63) + "-", // non-hex symbol
		} {
			_, err := ParseTunnelID(s)
			assert.Error(t, err, "expected %q to be rejected", s)
		}
	})
}
