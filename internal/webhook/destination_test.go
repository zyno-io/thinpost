package webhook

import (
	"net/netip"
	"testing"
)

func TestPublicAddressDeniedRanges(t *testing.T) {
	ranges := [][2]string{
		{"0.0.0.0", "0.255.255.255"}, {"10.0.0.0", "10.255.255.255"}, {"100.64.0.0", "100.127.255.255"},
		{"127.0.0.0", "127.255.255.255"}, {"169.254.0.0", "169.254.255.255"}, {"172.16.0.0", "172.31.255.255"},
		{"192.0.0.0", "192.0.0.255"}, {"192.0.2.0", "192.0.2.255"}, {"192.88.99.0", "192.88.99.255"},
		{"192.168.0.0", "192.168.255.255"}, {"198.18.0.0", "198.19.255.255"}, {"198.51.100.0", "198.51.100.255"},
		{"203.0.113.0", "203.0.113.255"}, {"224.0.0.0", "239.255.255.255"}, {"240.0.0.0", "255.255.255.255"},
		{"2001::", "2001:1ff:ffff:ffff:ffff:ffff:ffff:ffff"}, {"2001:db8::", "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff"},
		{"2002::", "2002:ffff:ffff:ffff:ffff:ffff:ffff:ffff"}, {"3fff::", "3fff:fff:ffff:ffff:ffff:ffff:ffff:ffff"},
		{"::", "::"}, {"::1", "::1"}, {"fc00::", "fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"},
		{"fe80::", "febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff"}, {"ff00::", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"},
		{"64:ff9b::", "64:ff9b::ffff:ffff"}, {"64:ff9b::808:808", "64:ff9b::808:808"},
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", "2001:0:4136:e378:8000:63bf:3fff:fdd2"}, // Teredo
		{"2002:c000:204::1", "2002:0808:0808::1"},                                        // 6to4
		{"::ffff:10.0.0.1", "::ffff:127.0.0.1"}, {"::ffff:169.254.169.254", "::ffff:192.168.1.1"},
		{"4000::", "4000::"}, {"1fff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "1fff:ffff:ffff:ffff:ffff:ffff:ffff:ffff"},
	}
	for _, r := range ranges {
		for _, value := range r {
			if PublicAddress(netip.MustParseAddr(value)) {
				t.Errorf("%s allowed", value)
			}
		}
	}
}

func TestPublicAddressAllowed(t *testing.T) {
	for _, value := range []string{
		"45.0.1.61", "1.1.1.1", "8.8.8.8", "9.255.255.255", "11.0.0.0", "100.63.255.255", "100.128.0.0",
		"126.255.255.255", "128.0.0.0", "169.253.255.255", "169.255.0.0", "172.15.255.255", "172.32.0.0",
		"192.0.1.0", "192.0.3.0", "192.88.98.255", "192.88.100.0", "192.167.255.255", "192.169.0.0",
		"198.17.255.255", "198.20.0.0", "198.51.99.255", "198.51.101.0", "203.0.112.255", "203.0.114.0",
		"223.255.255.255", "::ffff:8.8.8.8", "2606:4700:4700::1111", "2001:4860:4860::8888",
		"2000::", "2001:200::", "2001:db7:ffff:ffff:ffff:ffff:ffff:ffff", "2001:db9::", "2003::", "3fff:ffff::1",
	} {
		if !PublicAddress(netip.MustParseAddr(value)) {
			t.Errorf("%s denied", value)
		}
	}
	if PublicAddress(netip.Addr{}) {
		t.Error("invalid address allowed")
	}
	for _, zoned := range []string{"2606:4700:4700::1111%eth0", "::ffff:8.8.8.8%eth0"} {
		if PublicAddress(netip.MustParseAddr(zoned)) {
			t.Errorf("zoned address %s allowed", zoned)
		}
	}
}
