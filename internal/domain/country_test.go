package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCountryFromID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		id   string
		want string
	}{
		{"se-sto-wg-001", "SE"},
		{"mullvad-ch-zrh-wg-001", "CH"},
		{"mullvad-us-nyc-wg-501", "US"},
		{"12-foo", ""},
		{"XY-foo", ""},
		{"se", "SE"},
		{"", ""},
		{"a-b", ""},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, CountryFromID(tc.id))
		})
	}
}
