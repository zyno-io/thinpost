package main

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestRequireLocalAddresses(t *testing.T) {
	have := func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("45.0.1.22"), Mask: net.CIDRMask(24, 32)}, &net.IPAddr{IP: net.ParseIP("45.0.0.10")}}, nil
	}
	a, b, c := netip.MustParseAddr("45.0.1.22"), netip.MustParseAddr("45.0.0.10"), netip.MustParseAddr("45.0.0.11")
	if err := requireLocalAddresses([]netip.Addr{a, b}, have); err != nil {
		t.Fatal(err)
	}
	err := requireLocalAddresses([]netip.Addr{a, c}, have)
	if err == nil {
		t.Fatal("started with a source the host does not own")
	}
	if strings.Contains(err.Error(), c.String()) {
		t.Fatal("error names an address")
	}
	if requireLocalAddresses([]netip.Addr{a}, func() ([]net.Addr, error) { return nil, errors.New("boom") }) == nil {
		t.Fatal("ignored an interface listing failure")
	}
}
