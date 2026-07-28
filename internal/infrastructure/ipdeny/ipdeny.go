// Package ipdeny provides the hardcoded list of CIDR ranges that the v1 API
// refuses to dial through any tunnel. The list is deliberately non-configurable
// — operators MUST NOT widen it via config; tightening (subnet add) would
// require a code change and review. This is the SSRF defence baked into the
// daemon.
package ipdeny

import (
	"net/netip"

	"vpntunnel/internal"
)

// DefaultDeny returns the immutable list of CIDR ranges that block outbound
// proxy dials. The returned slice MUST NOT be modified; it is shared across
// goroutines. The list covers Linux loopback-via-zero, IPv4 loopback,
// RFC1918 private ranges, link-local, IPv6 unique-local, and IPv6 loopback.
// The list is deliberately non-configurable — operators MUST NOT widen it
// via config; tightening (subnet add) requires a code change and review.
func DefaultDeny() []netip.Prefix {
	return internal.DenyCIDRs
}

// Contains reports whether ip is covered by any prefix in the set. The caller
// should pass ip.Unmap() when the address originates from a DNS response that
// may return IPv4-mapped IPv6 addresses (::ffff:x.x.x.x) — Unmap ensures those
// match their IPv4 equivalents in DefaultDeny.
func Contains(prefixes []netip.Prefix, ip netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
