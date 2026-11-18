package egress

import (
	"net/netip"
	"testing"
)

// These are independently transcribed from the IANA special-purpose address
// registries last updated 2025-10-09. They are a release-review snapshot, not
// a live registry update or a complete inventory of operator infrastructure.
// https://www.iana.org/assignments/iana-ipv4-special-registry
// https://www.iana.org/assignments/iana-ipv6-special-registry
func TestIANASpecialPurposeDestinationsAreNotDialable(t *testing.T) {
	registry := []string{
		"0.0.0.0/8", "0.0.0.0/32", "10.0.0.0/8", "100.64.0.0/10",
		"127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24",
		"192.0.0.0/29", "192.0.0.8/32", "192.0.0.9/32", "192.0.0.10/32",
		"192.0.0.170/32", "192.0.0.171/32", "192.0.2.0/24", "192.31.196.0/24",
		"192.52.193.0/24", "192.88.99.0/24", "192.88.99.2/32", "192.168.0.0/16",
		"192.175.48.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"240.0.0.0/4", "255.255.255.255/32",
		"::1/128", "::/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64",
		"100:0:0:1::/64", "2001::/23", "2001::/32", "2001:1::1/128", "2001:1::2/128",
		"2001:1::3/128", "2001:2::/48", "2001:3::/32", "2001:4:112::/48",
		"2001:10::/28", "2001:20::/28", "2001:30::/28", "2001:db8::/32",
		"2002::/16", "2620:4f:8000::/48", "3fff::/20", "5f00::/16",
		"fc00::/7", "fe80::/10",
	}
	policy, err := NewPolicy(
		[]netip.Prefix{netip.MustParsePrefix("45.0.1.0/24"), netip.MustParsePrefix("2606:4700:400:a100::/56")},
		[]netip.Addr{netip.MustParseAddr("45.0.1.62"), netip.MustParseAddr("2606:4700:400:a110::100")},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range registry {
		prefix := netip.MustParsePrefix(entry).Masked()
		for _, address := range prefixSamples(prefix) {
			if policy.Allowed(address) {
				t.Fatalf("special-purpose destination %s in %s was allowed", address, entry)
			}
			if address.Is4() {
				mapped := netip.MustParseAddr("::ffff:" + address.String())
				if policy.Allowed(mapped) {
					t.Fatalf("mapped special-purpose destination %s was allowed", mapped)
				}
			}
		}
	}
	// IPv4-mapped IPv6 represents an IPv4 destination rather than a separate
	// route. Verify both public and denied IPv4 addresses retain their policy.
	for _, address := range []string{"8.8.8.8", "45.0.1.62", "127.0.0.1"} {
		v4 := netip.MustParseAddr(address)
		mapped := netip.MustParseAddr("::ffff:" + address)
		if policy.Allowed(v4) != policy.Allowed(mapped) {
			t.Fatalf("mapped IPv4 policy differs for %s", address)
		}
	}
}

// prefixSamples exercises both boundaries and an interior address, including
// broad prefixes that cannot be enumerated exhaustively (especially IPv6).
func prefixSamples(prefix netip.Prefix) []netip.Addr {
	start := prefix.Addr().As16()
	last, middle := start, start
	begin := prefix.Bits()
	if prefix.Addr().Is4() {
		begin += 96
	}
	for bit := begin; bit < 128; bit++ {
		last[bit/8] |= byte(1 << uint(7-bit%8))
	}
	if begin < 128 {
		middle[begin/8] |= byte(1 << uint(7-begin%8))
	}
	return []netip.Addr{netip.AddrFrom16(start).Unmap(), netip.AddrFrom16(middle).Unmap(), netip.AddrFrom16(last).Unmap()}
}
