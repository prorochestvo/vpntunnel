package domain

import "strings"

// CountryFromID extracts the two-letter country code from a Mullvad-style or
// legacy-style config basename. The returned code is uppercase.
//
// Mullvad naming: "mullvad-<cc>-<city>-wg-<N>" — the second hyphen-separated
// segment is the country code.
// Legacy naming: "<cc>-<city>-wg-<N>" — the first segment is the country code.
//
// In both cases the segment that carries the code must be exactly two lowercase
// ASCII letters; anything else returns "". A first segment that is exactly two
// lowercase ASCII letters is used directly (legacy form). When the first segment
// is not two lowercase letters, the second segment is examined.
//
// Examples:
//
//	"se-sto-wg-001"          → "SE"
//	"mullvad-ch-zrh-wg-001"  → "CH"
//	"mullvad-us-nyc-wg-501"  → "US"
//	"12-foo"                 → ""
//	"XY-foo"                 → ""
func CountryFromID(id string) string {
	parts := strings.SplitN(id, "-", 3)
	if cc := twoLowerLetters(parts[0]); cc != "" {
		return cc
	}
	if len(parts) < 2 {
		return ""
	}
	return twoLowerLetters(parts[1])
}

// twoLowerLetters returns the uppercase form of s when s is exactly two
// lowercase ASCII letters, and "" otherwise.
func twoLowerLetters(s string) string {
	if len(s) != 2 {
		return ""
	}
	a, b := s[0], s[1]
	if a < 'a' || a > 'z' || b < 'a' || b > 'z' {
		return ""
	}
	return strings.ToUpper(s)
}
