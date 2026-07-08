// Package lazy implements the lazy two-role tunnel manager: the streaming
// supervisor (always-on, one WireGuard device) and the on-demand scheduler
// (at most one device, time-multiplexed across zones).
package lazy

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	mrand "math/rand/v2"
	"path/filepath"
	"strings"
	"sync"

	"vpntunnel/internal/domain"
	"vpntunnel/internal/publicerror"
)

// NewEligibleSet builds the set of eligible config paths from rawConfigs
// filtered by allowed country codes. configDir is used to resolve relative
// paths (same rule as pool.NewPool). allowed is a list of two-letter country
// codes (case-insensitive); an empty slice means "no filter — all configs are
// eligible". Returns a *publicerror.Error when the resulting set is empty.
//
// Input paths in rawConfigs may be relative (resolved against configDir) or
// already absolute (e.g. as returned by DiscoverConfigs). In both cases the
// path is passed through filepath.Abs after joining, which is a no-op for
// already-absolute paths and simply canonicalises any residual ".." or "."
// segments. The double-Abs is intentional and harmless — future callers that
// pass relative paths will be handled correctly without any code change.
//
// A country code that matches no configs is silently dropped (no error).
// A country code that is syntactically invalid will never match anything and is
// also silently dropped; callers should validate codes via config.Load before
// reaching this point.
//
// This filtered view feeds ONLY the streaming supervisor random-pick; the
// on-demand scheduler uses NewFullSet. Keys in this set are basenames (the
// .conf filename without the suffix); Lookup/IsEligible take a basename.
func NewEligibleSet(rawConfigs []string, configDir string, allowed []string) (*EligibleSet, error) {
	return newSet(rawConfigs, configDir, allowed, func(b string) string { return b })
}

// NewFullSet returns the unfiltered view of all discovered configs used by the
// on-demand scheduler and the API ZoneChecker. Country codes are NEVER applied
// here — on-demand may route to any discovered zone regardless of the
// vpnstream.allowed_countries setting.
//
// Returns a *publicerror.Error only when rawConfigs is empty (no .conf
// discovered at all). In that case the process cannot serve any traffic and
// the caller should treat this as a fatal startup error.
//
// Keys in the full set are HMAC ids derived from hmacKey and the basename via
// domain.TunnelID. Lookup/IsEligible take the HMAC id. hmacKey is key material
// and must never be logged; it is not retained on the returned struct.
func NewFullSet(rawConfigs []string, configDir string, hmacKey []byte) (*EligibleSet, error) {
	if len(rawConfigs) == 0 {
		return nil, publicerror.New(
			"config.vpnstream: no tunnel configs discovered in tunnels/; " +
				"drop at least one wg-quick .conf file there",
		)
	}
	return newSet(rawConfigs, configDir, nil, func(b string) string {
		return domain.TunnelID(hmacKey, b)
	})
}

// CatalogEntry is one row from Entries(): the HMAC id (map key), the config
// basename, and the lowercase two-letter country code. Country is "" when the
// basename does not follow a recognised naming convention.
type CatalogEntry struct {
	ID       string
	Basename string
	Country  string
}

// EligibleSet is an immutable view of discovered tunnel configs — either the
// country-filtered streaming pool (NewEligibleSet) or the full on-demand pool
// (NewFullSet). It is safe for concurrent reads and for concurrent calls to
// RandomPath.
//
// The map key is context-dependent: NewEligibleSet uses the basename; NewFullSet
// uses the HMAC id from domain.TunnelID. Lookup, IsEligible, and all callers
// must pass the appropriate key type for the set they hold.
type EligibleSet struct {
	byKey map[string]entry // key → entry (key meaning depends on constructor)
	order []string         // stable insertion order (matches configs order)

	mu  sync.Mutex
	rng *mrand.Rand
}

// Lookup returns the absolute config path for the given key. For a streaming
// set (NewEligibleSet) the key is the config basename; for the full set
// (NewFullSet) the key is the HMAC tunnel id. ok is false when the key is not
// in the eligible set.
func (e *EligibleSet) Lookup(id string) (configPath string, ok bool) {
	ent, ok := e.byKey[id]
	return ent.path, ok
}

// RandomPath picks one config path uniformly at random from the eligible set.
// It is safe for concurrent callers. The returned path is absolute.
func (e *EligibleSet) RandomPath() string {
	e.mu.Lock()
	idx := e.rng.IntN(len(e.order))
	e.mu.Unlock()
	key := e.order[idx]
	return e.byKey[key].path
}

// IsEligible reports whether the given key is in the eligible set. For a
// streaming set the key is the basename; for the full set the key is the HMAC
// tunnel id. Safe for concurrent callers.
func (e *EligibleSet) IsEligible(id string) bool {
	_, ok := e.byKey[id]
	return ok
}

// Len returns the number of eligible configs.
func (e *EligibleSet) Len() int { return len(e.order) }

// Entries returns a freshly allocated slice of CatalogEntry values in stable
// insertion order (same order as the rawConfigs slice passed to the
// constructor). Each entry carries ID (the map key — HMAC id for the full set,
// basename for the streaming set), Basename, and Country (lowercase two-letter
// code, or "" when unparseable).
func (e *EligibleSet) Entries() []CatalogEntry {
	out := make([]CatalogEntry, 0, len(e.order))
	for _, key := range e.order {
		ent := e.byKey[key]
		out = append(out, CatalogEntry{
			ID:       key,
			Basename: ent.basename,
			Country:  ent.country,
		})
	}
	return out
}

// entry holds the per-config fields stored in byKey.
type entry struct {
	basename string
	country  string // lowercase two-letter code, or "" when unparseable
	path     string // absolute config path
}

// newSet is the shared constructor used by both NewEligibleSet and NewFullSet.
// keyFn maps a config basename to the map key to use (basename identity for the
// streaming set; domain.TunnelID for the full set). allowed is an optional
// country filter; nil/empty means accept all.
func newSet(rawConfigs []string, configDir string, allowed []string, keyFn func(basename string) string) (*EligibleSet, error) {
	// build a lookup set of allowed codes (normalised to lowercase for matching).
	allowAll := len(allowed) == 0
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, cc := range allowed {
		allowedSet[strings.ToLower(cc)] = struct{}{}
	}

	byKey := make(map[string]entry)
	var order []string

	for _, cfgPath := range rawConfigs {
		path := cfgPath
		if !filepath.IsAbs(path) {
			path = filepath.Join(configDir, path)
		}
		// resolve to an absolute path so RandomPath/Lookup honour their
		// documented "absolute" contract; otherwise a relative configDir leaves
		// path relative and BuildDialer re-joins configDir, doubling the prefix.
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("lazy: resolve config path %q: %w", cfgPath, err)
		}
		path = abs

		basename := strings.TrimSuffix(filepath.Base(path), ".conf")

		if !allowAll {
			cc := strings.ToLower(domain.CountryFromID(basename))
			if _, ok := allowedSet[cc]; !ok {
				continue
			}
		}

		key := keyFn(basename)
		if _, dup := byKey[key]; !dup {
			byKey[key] = entry{
				basename: basename,
				// CountryFromID returns UPPERCASE; store lowercase so catalog
				// keys and the ?country= filter (which lowercases its tokens)
				// are on the same case plane. The streaming filter above uses
				// the same strings.ToLower wrap.
				country: strings.ToLower(domain.CountryFromID(basename)),
				path:    path,
			}
			order = append(order, key)
		}
	}

	if len(byKey) == 0 {
		if len(allowed) == 0 {
			return nil, publicerror.New(
				"config.vpnstream: no eligible tunnel configs discovered in tunnels/; " +
					"ensure at least one wg-quick .conf file is present",
			)
		}
		return nil, publicerror.New(fmt.Sprintf(
			"config.upstream: no eligible tunnel configs after country filter %s; "+
				"check that allowed_countries matches at least one of the discovered configs in tunnels/",
			strings.Join(allowed, ", "),
		))
	}

	return &EligibleSet{byKey: byKey, order: order, rng: newCryptoSeededRand()}, nil
}

// newCryptoSeededRand seeds a PCG-backed Rand from two crypto/rand uint64s.
// It panics if crypto/rand is unavailable, matching the project's pattern for
// unrecoverable entropy failures.
func newCryptoSeededRand() *mrand.Rand {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	seed1 := binary.LittleEndian.Uint64(buf[:8])
	seed2 := binary.LittleEndian.Uint64(buf[8:])
	return mrand.New(mrand.NewPCG(seed1, seed2))
}
