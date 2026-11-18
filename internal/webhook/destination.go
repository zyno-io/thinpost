package webhook

import "net/netip"

var deniedV4 = prefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
)

var (
	globalV6 = netip.MustParsePrefix("2000::/3")
	deniedV6 = prefixes("2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20")
)

func prefixes(values ...string) []netip.Prefix {
	result := make([]netip.Prefix, len(values))
	for i, value := range values {
		result[i] = netip.MustParsePrefix(value)
	}
	return result
}

// PublicAddress reports whether a destination address is public unicast. IPv4
// mapped addresses are judged as IPv4; zoned addresses are never public.
func PublicAddress(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	denied := deniedV4
	if addr.Is6() {
		if !globalV6.Contains(addr) {
			return false
		}
		denied = deniedV6
	}
	for _, prefix := range denied {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}
