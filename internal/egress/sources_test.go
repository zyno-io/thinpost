package egress

import (
	"context"
	"net"
	"net/netip"
	"testing"
)

func TestMultipleSourcesPerFamily(t *testing.T) {
	infra := []netip.Prefix{netip.MustParsePrefix("45.0.1.0/24"), netip.MustParsePrefix("45.0.0.0/24")}
	primary, second := netip.MustParseAddr("45.0.1.22"), netip.MustParseAddr("45.0.0.10")
	if _, err := NewPolicy(infra, []netip.Addr{primary, primary}); err == nil {
		t.Fatal("accepted a duplicate source")
	}
	p, err := NewPolicy(infra, []netip.Addr{primary, second})
	if err != nil {
		t.Fatal(err)
	}
	var seenSource netip.Addr
	dial := func(_ context.Context, source, _ netip.Addr) (net.Conn, error) {
		seenSource = source
		return nil, nil
	}
	dest := netip.MustParseAddr("8.8.8.8")
	// A zero source is the primary, never a later source.
	if _, err = p.dial(context.Background(), netip.Addr{}, dest, "", "", dial); err != nil || seenSource != primary {
		t.Fatalf("zero source used %s, error %v", seenSource, err)
	}
	if _, err = p.dial(context.Background(), second, dest, "", "", dial); err != nil || seenSource != second {
		t.Fatalf("named source used %s, error %v", seenSource, err)
	}
	// An unknown source, or a wrong-family one, never dials.
	seenSource = netip.Addr{}
	for _, source := range []netip.Addr{netip.MustParseAddr("45.0.1.23"), netip.MustParseAddr("2607:f8b0:4000::5")} {
		if _, err = p.dial(context.Background(), source, dest, "", "", dial); err == nil || seenSource.IsValid() {
			t.Fatalf("dialed from unknown source %s", source)
		}
	}
	if _, err = p.dial(context.Background(), second, netip.MustParseAddr("2607:f8b0:4000::1"), "", "", dial); err == nil || seenSource.IsValid() {
		t.Fatal("dialed an IPv6 destination from an IPv4 source")
	}
	// Every source is excluded from route exceptions, not only the primary.
	for _, source := range []netip.Addr{primary, second} {
		if p.isExceptionAddress(source) || p.Allowed(source) {
			t.Fatalf("source %s is reachable as a destination", source)
		}
		if _, err = NewPolicy(infra, []netip.Addr{primary, second}, Exception{Recipient: "a@mailhost.example", MX: "mx.mailhost.example", Address: source}); err == nil {
			t.Fatalf("accepted a route exception for source %s", source)
		}
	}
	if !p.isExceptionAddress(netip.MustParseAddr("45.0.1.41")) {
		t.Fatal("an infrastructure address that is not a source must stay excludable")
	}
	var tooMany []netip.Addr
	for i := 0; i <= MaxSources; i++ {
		tooMany = append(tooMany, netip.AddrFrom4([4]byte{45, 0, 0, byte(i + 1)}))
	}
	if _, err = NewPolicy(infra, tooMany); err == nil {
		t.Fatal("accepted too many sources")
	}
}

func TestPublicSource(t *testing.T) {
	for value, want := range map[string]bool{"45.0.1.22": true, "8.8.8.8": true, "10.0.0.1": false, "127.0.0.1": false, "203.0.113.5": false, "2607:f8b0:4000::1": false, "::ffff:8.8.8.8": false} {
		if got := PublicSource(netip.MustParseAddr(value)); got != want {
			t.Errorf("PublicSource(%s)=%v", value, got)
		}
	}
	if PublicSource(netip.Addr{}) {
		t.Fatal("zero address is not a public source")
	}
}
