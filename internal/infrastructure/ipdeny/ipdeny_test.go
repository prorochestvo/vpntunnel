package ipdeny_test

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"

	"vpntunnel/internal/infrastructure/ipdeny"
)

func TestContains(t *testing.T) {
	t.Parallel()

	// each of the 8 CIDRs in DefaultDeny must block an in-range address.
	t.Run("this_network_0_0_0_0_8_blocked", func(t *testing.T) {
		t.Parallel()
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("0.0.0.0")),
			"0.0.0.0 must be denied — Linux connect(2) routes 0.x.x.x to loopback")
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("0.255.255.255")))
	})

	t.Run("loopback_127_0_0_0_8_blocked", func(t *testing.T) {
		t.Parallel()
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("127.0.0.1")))
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("127.255.255.255")))
	})

	t.Run("rfc1918_10_0_0_0_8_blocked", func(t *testing.T) {
		t.Parallel()
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("10.0.0.1")))
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("10.255.255.255")))
	})

	t.Run("rfc1918_172_16_0_0_12_blocked", func(t *testing.T) {
		t.Parallel()
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("172.16.0.1")))
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("172.31.255.255")))
	})

	t.Run("rfc1918_192_168_0_0_16_blocked", func(t *testing.T) {
		t.Parallel()
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("192.168.1.1")))
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("192.168.255.255")))
	})

	t.Run("link_local_169_254_0_0_16_blocked", func(t *testing.T) {
		t.Parallel()
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("169.254.0.1")))
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("169.254.169.254")))
	})

	t.Run("ipv6_loopback_1_128_blocked", func(t *testing.T) {
		t.Parallel()
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("::1")))
	})

	t.Run("ipv6_unique_local_fc00_7_blocked", func(t *testing.T) {
		t.Parallel()
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("fc00::1")))
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("fd00::1")))
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff")))
	})

	// public IPs must not be blocked.
	t.Run("public_ips_not_blocked", func(t *testing.T) {
		t.Parallel()
		publicIPs := []string{
			"8.8.8.8",
			"1.1.1.1",
			"9.9.9.9",
			"172.32.0.1",  // just outside 172.16.0.0/12
			"192.167.0.1", // just below 192.168.0.0/16
			"192.169.0.1", // just above 192.168.0.0/16
		}
		for _, s := range publicIPs {
			s := s
			t.Run(s, func(t *testing.T) {
				t.Parallel()
				assert.False(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr(s)))
			})
		}
	})

	t.Run("public_ipv6_not_blocked", func(t *testing.T) {
		t.Parallel()
		assert.False(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("2001:4860:4860::8888")))
		assert.False(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("2606:4700:4700::1111")))
		// fe80:: is link-local multicast; fc00::/7 does not cover it (fe80 is in 0xfe80, mask is 0xfe00 for /7... actually let's verify)
		// fe00::/7 covers fc00:: through fdff::... 0xfe is 11111110, /7 mask = 0xfe << 0 = 11111110...
		// fc00::/7: the top 7 bits of fc are 1111110 = 0x7e; fe is 1111111 = 0x7f — different, so fe80:: is NOT in fc00::/7.
		assert.False(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("fe80::1")))
	})

	// edge cases: exact network and broadcast addresses.
	t.Run("network_address_blocked", func(t *testing.T) {
		t.Parallel()
		// 10.0.0.0 is the network address of 10.0.0.0/8.
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("10.0.0.0")))
		// 127.0.0.0 is the network address of 127.0.0.0/8.
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("127.0.0.0")))
	})

	t.Run("broadcast_address_blocked", func(t *testing.T) {
		t.Parallel()
		// 10.255.255.255 is the broadcast of 10.0.0.0/8.
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), netip.MustParseAddr("10.255.255.255")))
	})

	// IPv4-mapped IPv6 (::ffff:127.0.0.1) does NOT match 127.0.0.0/8 directly
	// because netip.Prefix.Contains compares by address family. The caller must
	// call ip.Unmap() before passing to Contains to canonicalise. This test
	// documents the behaviour: raw IPv4-mapped form misses, Unmap()ed form hits.
	t.Run("ipv4_mapped_ipv6_requires_unmap", func(t *testing.T) {
		t.Parallel()
		mapped := netip.MustParseAddr("::ffff:127.0.0.1")
		assert.True(t, mapped.Is4In6(), "sanity check: address is IPv4-mapped IPv6")
		// without Unmap: does NOT match 127.0.0.0/8 (different family).
		assert.False(t, ipdeny.Contains(ipdeny.DefaultDeny(), mapped),
			"raw IPv4-mapped form must not match the IPv4 prefix — caller must Unmap")
		// with Unmap: matches 127.0.0.0/8 as expected.
		assert.True(t, ipdeny.Contains(ipdeny.DefaultDeny(), mapped.Unmap()),
			"Unmap()ed address must match 127.0.0.0/8")
	})

	// zero-value netip.Addr must not panic and must return false.
	t.Run("zero_addr_returns_false", func(t *testing.T) {
		t.Parallel()
		var zero netip.Addr
		assert.False(t, ipdeny.Contains(ipdeny.DefaultDeny(), zero))
	})

	// empty prefix list returns false for any IP.
	t.Run("empty_prefix_list_returns_false", func(t *testing.T) {
		t.Parallel()
		assert.False(t, ipdeny.Contains(nil, netip.MustParseAddr("10.0.0.1")))
		assert.False(t, ipdeny.Contains([]netip.Prefix{}, netip.MustParseAddr("10.0.0.1")))
	})
}
