package tunnelpool

import (
	"errors"
	mrand "math/rand/v2"
	"testing"

	"github.com/prorochestvo/loginjector"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/domain"
	"vpntunnel/internal/tools/hmackey"
)

// sampleConfigs is a helper that returns a slice of fake .conf filenames
// for use in eligible-set tests. The names follow Mullvad naming convention
// so countryFromBasename can extract the country code.
func sampleConfigs() []string {
	return []string{
		"mullvad-us-nyc-wg-001.conf",
		"mullvad-us-lax-wg-001.conf",
		"mullvad-gb-lon-wg-001.conf",
		"mullvad-de-fra-wg-001.conf",
		"mullvad-ua-kiv-wg-001.conf",
		"mullvad-se-sto-wg-001.conf",
	}
}

// testHMACKey is a fixed 32-byte key for deterministic HMAC id tests.
var testHMACKey = []byte("00000000000000000000000000000000")

func TestNewEligibleSet(t *testing.T) {
	t.Parallel()

	t.Run("filter selects expected subset", func(t *testing.T) {
		t.Parallel()
		cfgs := sampleConfigs()
		set, err := NewEligibleSet(cfgs, "/fake", []string{"us", "gb"})
		require.NoError(t, err)
		assert.Equal(t, 3, set.Len()) // us-nyc, us-lax, gb-lon

		_, ok := set.Lookup("mullvad-us-nyc-wg-001")
		assert.True(t, ok)
		_, ok = set.Lookup("mullvad-us-lax-wg-001")
		assert.True(t, ok)
		_, ok = set.Lookup("mullvad-gb-lon-wg-001")
		assert.True(t, ok)
		_, ok = set.Lookup("mullvad-de-fra-wg-001")
		assert.False(t, ok)
	})

	t.Run("empty allowed list means all configs eligible", func(t *testing.T) {
		t.Parallel()
		cfgs := sampleConfigs()
		set, err := NewEligibleSet(cfgs, "/fake", []string{})
		require.NoError(t, err)
		assert.Equal(t, len(cfgs), set.Len())
	})

	t.Run("nil allowed list means all configs eligible", func(t *testing.T) {
		t.Parallel()
		cfgs := sampleConfigs()
		set, err := NewEligibleSet(cfgs, "/fake", nil)
		require.NoError(t, err)
		assert.Equal(t, len(cfgs), set.Len())
	})

	t.Run("country with no matching configs is silently dropped", func(t *testing.T) {
		t.Parallel()
		cfgs := sampleConfigs()
		// "fr" has no matching configs; "us" has two
		set, err := NewEligibleSet(cfgs, "/fake", []string{"us", "fr"})
		require.NoError(t, err)
		assert.Equal(t, 2, set.Len()) // us-nyc, us-lax
	})

	t.Run("all countries have no matching configs returns publicerror", func(t *testing.T) {
		t.Parallel()
		cfgs := sampleConfigs()
		_, err := NewEligibleSet(cfgs, "/fake", []string{"xx", "yy"})
		require.Error(t, err)
		var pe loginjector.PublicDetailsError
		ok := errors.As(err, &pe)
		require.True(t, ok, "expected publicerror, got: %T %v", err, err)
		assert.Contains(t, pe.Details(), "eligible")
	})

	t.Run("empty config list returns publicerror", func(t *testing.T) {
		t.Parallel()
		_, err := NewEligibleSet([]string{}, "/fake", nil)
		require.Error(t, err)
		ok := errors.As(err, new(loginjector.PublicDetailsError))
		require.True(t, ok)
	})

	t.Run("relative configs resolved against configDir", func(t *testing.T) {
		t.Parallel()
		cfgs := []string{"mullvad-se-sto-wg-001.conf"}
		set, err := NewEligibleSet(cfgs, "/opt/vpntunnel/configs", nil)
		require.NoError(t, err)

		path, ok := set.Lookup("mullvad-se-sto-wg-001")
		require.True(t, ok)
		assert.Equal(t, "/opt/vpntunnel/configs/mullvad-se-sto-wg-001.conf", path)
	})

	t.Run("absolute configs passed through unchanged", func(t *testing.T) {
		t.Parallel()
		cfgs := []string{"/abs/mullvad-gb-lon-wg-001.conf"}
		set, err := NewEligibleSet(cfgs, "/irrelevant", nil)
		require.NoError(t, err)

		path, ok := set.Lookup("mullvad-gb-lon-wg-001")
		require.True(t, ok)
		assert.Equal(t, "/abs/mullvad-gb-lon-wg-001.conf", path)
	})

	t.Run("still keyed by basename", func(t *testing.T) {
		t.Parallel()
		cfgs := []string{"mullvad-se-sto-wg-001.conf", "mullvad-de-fra-wg-001.conf"}
		set, err := NewEligibleSet(cfgs, "/fake", nil)
		require.NoError(t, err)

		// the streaming set key is the basename, not an HMAC id
		path, ok := set.Lookup("mullvad-se-sto-wg-001")
		require.True(t, ok)
		assert.Equal(t, "/fake/mullvad-se-sto-wg-001.conf", path)

		// an HMAC id must NOT be found
		hmacID := hmackey.DeriveID(testHMACKey, "mullvad-se-sto-wg-001")
		_, ok = set.Lookup(hmacID)
		assert.False(t, ok, "streaming set must not accept HMAC id as key")
	})

	t.Run("country filter still drops non-matching", func(t *testing.T) {
		t.Parallel()
		cfgs := sampleConfigs()
		set, err := NewEligibleSet(cfgs, "/fake", []string{"se"})
		require.NoError(t, err)
		assert.Equal(t, 1, set.Len())
		_, ok := set.Lookup("mullvad-se-sto-wg-001")
		assert.True(t, ok)
		_, ok = set.Lookup("mullvad-us-nyc-wg-001")
		assert.False(t, ok)
	})
}

func TestEligibleSet_Lookup(t *testing.T) {
	t.Parallel()

	set, err := NewEligibleSet(sampleConfigs(), "/fake/dir", []string{"us"})
	require.NoError(t, err)

	t.Run("known zone returns ok true and absolute path", func(t *testing.T) {
		t.Parallel()
		path, ok := set.Lookup("mullvad-us-nyc-wg-001")
		assert.True(t, ok)
		assert.Equal(t, "/fake/dir/mullvad-us-nyc-wg-001.conf", path)
	})

	t.Run("unknown zone returns ok false", func(t *testing.T) {
		t.Parallel()
		_, ok := set.Lookup("mullvad-de-fra-wg-001")
		assert.False(t, ok)
	})

	t.Run("empty zone id returns ok false", func(t *testing.T) {
		t.Parallel()
		_, ok := set.Lookup("")
		assert.False(t, ok)
	})
}

func TestNewFullSet(t *testing.T) {
	t.Parallel()

	t.Run("keys by hmac id", func(t *testing.T) {
		t.Parallel()
		cfgs := []string{"mullvad-se-sto-wg-001.conf"}
		set, err := NewFullSet(cfgs, "/fake", testHMACKey)
		require.NoError(t, err)

		hmacID := hmackey.DeriveID(testHMACKey, "mullvad-se-sto-wg-001")
		path, ok := set.Lookup(hmacID)
		require.True(t, ok, "full set must be found by HMAC id")
		assert.Equal(t, "/fake/mullvad-se-sto-wg-001.conf", path)
	})

	t.Run("basename NOT found on full set", func(t *testing.T) {
		t.Parallel()
		cfgs := []string{"mullvad-se-sto-wg-001.conf"}
		set, err := NewFullSet(cfgs, "/fake", testHMACKey)
		require.NoError(t, err)

		_, ok := set.Lookup("mullvad-se-sto-wg-001")
		assert.False(t, ok, "full set must not accept basename as key")
	})

	t.Run("IsEligible matches hmac id only", func(t *testing.T) {
		t.Parallel()
		cfgs := []string{"mullvad-se-sto-wg-001.conf"}
		set, err := NewFullSet(cfgs, "/fake", testHMACKey)
		require.NoError(t, err)

		hmacID := hmackey.DeriveID(testHMACKey, "mullvad-se-sto-wg-001")
		assert.True(t, set.IsEligible(hmacID), "HMAC id must be eligible")
		assert.False(t, set.IsEligible("mullvad-se-sto-wg-001"), "basename must not be eligible on full set")
	})

	t.Run("Entries returns id+basename+country for all", func(t *testing.T) {
		t.Parallel()
		// include one unparseable-country config alongside parseable ones
		cfgs := []string{
			"mullvad-se-sto-wg-001.conf",
			"mullvad-de-fra-wg-001.conf",
			"unparseable-123-wg-001.conf", // no valid country code
		}
		set, err := NewFullSet(cfgs, "/fake", testHMACKey)
		require.NoError(t, err)

		entries := set.Entries()
		require.Len(t, entries, 3, "must have one entry per discovered config")

		// build a map for easier assertion
		byBasename := make(map[string]CatalogEntry, len(entries))
		for _, e := range entries {
			byBasename[e.Basename] = e
		}

		seEntry := byBasename["mullvad-se-sto-wg-001"]
		assert.Equal(t, hmackey.DeriveID(testHMACKey, "mullvad-se-sto-wg-001"), seEntry.ID)
		assert.Equal(t, "se", seEntry.Country)

		deEntry := byBasename["mullvad-de-fra-wg-001"]
		assert.Equal(t, hmackey.DeriveID(testHMACKey, "mullvad-de-fra-wg-001"), deEntry.ID)
		assert.Equal(t, "de", deEntry.Country)

		unparseable := byBasename["unparseable-123-wg-001"]
		assert.Equal(t, hmackey.DeriveID(testHMACKey, "unparseable-123-wg-001"), unparseable.ID)
		assert.Equal(t, "", unparseable.Country, "unparseable basename must yield empty Country")

		// verify stable order: must match rawConfigs order
		assert.Equal(t, "mullvad-se-sto-wg-001", entries[0].Basename)
		assert.Equal(t, "mullvad-de-fra-wg-001", entries[1].Basename)
		assert.Equal(t, "unparseable-123-wg-001", entries[2].Basename)
	})

	t.Run("returns all configs regardless of country", func(t *testing.T) {
		t.Parallel()
		cfgs := sampleConfigs() // includes de-fra, ua-kiv which a {us,gb} filter would drop
		set, err := NewFullSet(cfgs, "/fake", testHMACKey)
		require.NoError(t, err)
		assert.Equal(t, len(cfgs), set.Len(), "full set must contain every discovered config")

		// these zones would be excluded by a {us,gb} country filter but must be
		// present in the full set so on-demand can route to any discovered zone.
		deID := hmackey.DeriveID(testHMACKey, "mullvad-de-fra-wg-001")
		uaID := hmackey.DeriveID(testHMACKey, "mullvad-ua-kiv-wg-001")
		_, ok := set.Lookup(deID)
		assert.True(t, ok, "de-fra zone must be routable via the full set")
		_, ok = set.Lookup(uaID)
		assert.True(t, ok, "ua-kiv zone must be routable via the full set")

		// zones that are also in {us,gb} must still be present.
		usID := hmackey.DeriveID(testHMACKey, "mullvad-us-nyc-wg-001")
		gbID := hmackey.DeriveID(testHMACKey, "mullvad-gb-lon-wg-001")
		_, ok = set.Lookup(usID)
		assert.True(t, ok)
		_, ok = set.Lookup(gbID)
		assert.True(t, ok)
	})

	t.Run("empty rawConfigs returns publicerror with vpnstream guidance", func(t *testing.T) {
		t.Parallel()
		_, err := NewFullSet([]string{}, "/fake", testHMACKey)
		require.Error(t, err)
		var pe loginjector.PublicDetailsError
		ok := errors.As(err, &pe)
		require.True(t, ok, "expected publicerror for empty config list, got: %T %v", err, err)
		assert.Contains(t, pe.Details(), "config.vpnstream")
		assert.Contains(t, pe.Details(), "tunnels/")
		// must not show the country-filter guidance (wrong context for this error)
		assert.NotContains(t, pe.Details(), "allowed_countries")
	})

	t.Run("relative configs resolved against configDir", func(t *testing.T) {
		t.Parallel()
		cfgs := []string{"mullvad-se-sto-wg-001.conf"}
		set, err := NewFullSet(cfgs, "/opt/vpntunnel/configs", testHMACKey)
		require.NoError(t, err)

		hmacID := hmackey.DeriveID(testHMACKey, "mullvad-se-sto-wg-001")
		path, ok := set.Lookup(hmacID)
		require.True(t, ok)
		assert.Equal(t, "/opt/vpntunnel/configs/mullvad-se-sto-wg-001.conf", path)
	})

	t.Run("Entries order is stable across multiple calls", func(t *testing.T) {
		t.Parallel()
		cfgs := sampleConfigs()
		set, err := NewFullSet(cfgs, "/fake", testHMACKey)
		require.NoError(t, err)

		first := set.Entries()
		second := set.Entries()
		require.Len(t, second, len(first))
		for i := range first {
			assert.Equal(t, first[i].ID, second[i].ID, "order must be stable across calls")
			assert.Equal(t, first[i].Basename, second[i].Basename)
			assert.Equal(t, first[i].Country, second[i].Country)
		}
	})
}

func TestEligibleSet_RandomPath(t *testing.T) {
	t.Parallel()

	t.Run("random pick stays within eligible set", func(t *testing.T) {
		t.Parallel()
		cfgs := sampleConfigs()
		set, err := NewEligibleSet(cfgs, "/fake", []string{"us"})
		require.NoError(t, err)
		set.setRNG(mrand.New(mrand.NewPCG(42, 0)))

		for range 50 {
			path := set.RandomPath()
			assert.NotEmpty(t, path)
			// path must end with a US config
			assert.Contains(t, path, "mullvad-us-")
		}
	})

	t.Run("single-entry set always returns same path", func(t *testing.T) {
		t.Parallel()
		cfgs := []string{"mullvad-ua-kiv-wg-001.conf"}
		set, err := NewEligibleSet(cfgs, "/fake", nil)
		require.NoError(t, err)
		set.setRNG(mrand.New(mrand.NewPCG(1, 0)))

		path := set.RandomPath()
		assert.Equal(t, "/fake/mullvad-ua-kiv-wg-001.conf", path)
	})

	t.Run("multi-entry set distributes picks", func(t *testing.T) {
		t.Parallel()
		cfgs := sampleConfigs()
		set, err := NewEligibleSet(cfgs, "/fake", nil)
		require.NoError(t, err)
		set.setRNG(mrand.New(mrand.NewPCG(7, 13)))

		seen := make(map[string]int)
		const iterations = 200
		for range iterations {
			seen[set.RandomPath()]++
		}
		// with 6 configs and 200 picks, each should appear at least once
		assert.GreaterOrEqual(t, len(seen), 2, "expected multiple distinct picks")
	})

	t.Run("full set random pick returns absolute path", func(t *testing.T) {
		t.Parallel()
		cfgs := []string{
			"mullvad-se-sto-wg-001.conf",
			"mullvad-de-fra-wg-001.conf",
		}
		set, err := NewFullSet(cfgs, "/fake", testHMACKey)
		require.NoError(t, err)
		set.setRNG(mrand.New(mrand.NewPCG(99, 0)))

		validPaths := map[string]bool{
			"/fake/mullvad-se-sto-wg-001.conf": true,
			"/fake/mullvad-de-fra-wg-001.conf": true,
		}
		for range 20 {
			path := set.RandomPath()
			assert.True(t, validPaths[path], "RandomPath must return one of the input paths, got %q", path)
		}
	})
}

// setRNG replaces the PRNG used by RandomPath. It is intended for use in tests
// only, to inject a deterministic source. Must not be called concurrently with
// RandomPath.
func (e *EligibleSet) setRNG(r *mrand.Rand) {
	e.rng = r
}

func TestCountryFromBasename(t *testing.T) {
	t.Parallel()

	cases := []struct {
		basename string
		want     domain.Country
	}{
		{"se-sto-wg-001", "se"},         // legacy form: first segment
		{"mullvad-ch-zrh-wg-001", "ch"}, // Mullvad form: second segment
		{"mullvad-us-nyc-wg-501", "us"}, //
		{"se", "se"},                    // bare code
		{"XY-foo", "xy"},                // uppercase segment is normalized
		{"12-foo", ""},                  // first segment not letters, second too long
		{"", ""},                        // empty basename
		{"a-b", ""},                     // single-letter segments
		{"unparseable-123-wg-001", ""},  // no two-letter segment in first two positions
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.basename, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, countryFromBasename(tc.basename))
		})
	}
}
