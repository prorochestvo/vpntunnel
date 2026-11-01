package domain

import (
	"fmt"
	"strings"
)

// ParseCountry validates that s is exactly two ASCII letters (either case) and
// returns it normalized to lowercase. It returns an error otherwise, so an
// ill-formed Country cannot be constructed. The two-ASCII-letter rule is the
// only invariant enforced; ParseCountry does not check the code against any ISO
// registry.
func ParseCountry(s string) (Country, error) {
	if len(s) != 2 || !isASCIILetter(s[0]) || !isASCIILetter(s[1]) {
		return "", fmt.Errorf("domain: %q is not a two-letter country code", s)
	}
	return Country(strings.ToLower(s)), nil
}

// Country is a validated two-letter country code, normalized to lowercase.
// The zero value "" means "no/unknown country". Non-empty values are
// constructed only via ParseCountry, so a Country is always either empty or a
// well-formed two-letter lowercase code — callers never need to re-validate or
// re-lowercase it.
type Country string

// isASCIILetter reports whether b is an ASCII letter (a–z or A–Z).
func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
