package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCountry(t *testing.T) {
	t.Parallel()

	t.Run("valid two-letter codes normalize to lowercase", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			in   string
			want Country
		}{
			{"se", "se"},
			{"SE", "se"},
			{"Ch", "ch"},
			{"uS", "us"},
		}
		for _, tc := range cases {
			got, err := ParseCountry(tc.in)
			require.NoError(t, err, "ParseCountry(%q)", tc.in)
			assert.Equal(t, tc.want, got)
		}
	})

	t.Run("rejects input that is not two ASCII letters", func(t *testing.T) {
		t.Parallel()
		for _, in := range []string{"", "a", "abc", "12", "s1", "a-", " s", "s "} {
			_, err := ParseCountry(in)
			assert.Error(t, err, "ParseCountry(%q) must fail", in)
		}
	})
}
