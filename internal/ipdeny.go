package internal

import "net/netip"

// DenyCIDRs is the immutable list of CIDR ranges that the v1 API refuses to dial
// through any tunnel — the SSRF defence baked into the daemon. It covers Linux
// loopback-via-zero, IPv4 loopback, RFC1918 private ranges, link-local, IPv6
// unique-local, and IPv6 loopback.
//
// The slice MUST NOT be modified or appended to; it is shared across goroutines.
// The list is deliberately non-configurable — operators MUST NOT widen it via
// config; tightening (subnet add) requires a code change and review. Consumers
// reach it through the ipdeny package's DefaultDeny accessor.
var DenyCIDRs = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // Linux connect(2) routes 0.x.x.x to loopback
	netip.MustParsePrefix("127.0.0.0/8"),    // IPv4 loopback
	netip.MustParsePrefix("10.0.0.0/8"),     // RFC1918
	netip.MustParsePrefix("172.16.0.0/12"),  // RFC1918
	netip.MustParsePrefix("192.168.0.0/16"), // RFC1918
	netip.MustParsePrefix("169.254.0.0/16"), // link-local
	netip.MustParsePrefix("::1/128"),        // IPv6 loopback
	netip.MustParsePrefix("fc00::/7"),       // IPv6 unique-local
}
