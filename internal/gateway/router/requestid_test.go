package router

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var uuidv7Regex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestNewRequestID(t *testing.T) {
	t.Parallel()

	t.Run("format_is_canonical_uuid_8_4_4_4_12", func(t *testing.T) {
		t.Parallel()
		id := newRequestID()
		assert.Regexp(t, uuidv7Regex, id, "UUIDv7 must match 8-4-4-4-12 canonical hex format")
	})

	t.Run("version_is_7", func(t *testing.T) {
		t.Parallel()
		id := newRequestID()
		// canonical form: xxxxxxxx-xxxx-Mxxx-xxxx-xxxxxxxxxxxx
		// M is at index 14 (0-indexed): "00000000-0000-7xxx-..."
		require.Len(t, id, 36)
		assert.Equal(t, byte('7'), id[14], "version nibble at index 14 must be '7'")
	})

	t.Run("variant_is_rfc9562", func(t *testing.T) {
		t.Parallel()
		id := newRequestID()
		// variant byte is at index 19: "xxxxxxxx-xxxx-xxxx-Nxxx-..."
		// RFC 9562 variant means top 2 bits are 0b10, so hex digit is 8, 9, a, or b.
		require.Len(t, id, 36)
		v := id[19]
		assert.Contains(t, "89ab", string(v), "variant nibble at index 19 must be one of 8, 9, a, b")
	})

	t.Run("unique_across_many_calls", func(t *testing.T) {
		t.Parallel()
		const n = 1000
		seen := make(map[string]struct{}, n)
		for range n {
			id := newRequestID()
			_, dup := seen[id]
			assert.False(t, dup, "duplicate UUIDv7 generated: %s", id)
			seen[id] = struct{}{}
		}
	})

	t.Run("time_prefix_is_monotonic_within_millis", func(t *testing.T) {
		t.Parallel()
		// the first 12 hex chars encode the 48-bit millisecond timestamp.
		// two IDs generated in succession must have lhs <= rhs lexicographically
		// (same millisecond → equal prefixes; later millisecond → larger prefix).
		id1 := newRequestID()
		id2 := newRequestID()

		prefix1 := id1[0:8] + id1[9:13] // remove the first hyphen
		prefix2 := id2[0:8] + id2[9:13]

		assert.LessOrEqual(t, prefix1, prefix2,
			"first 12 timestamp hex chars of id1 (%s) must be <= id2 (%s)", id1, id2)
	})
}
