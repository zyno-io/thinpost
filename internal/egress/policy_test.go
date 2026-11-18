package egress

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDestinationPolicy(t *testing.T) {
	p, err := NewPolicy([]netip.Prefix{netip.MustParsePrefix("45.0.1.0/24"), netip.MustParsePrefix("2606:4700::/32")}, []netip.Addr{netip.MustParseAddr("45.0.1.22"), netip.MustParseAddr("2606:4700::1")})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		address string
		allowed bool
	}{
		{"8.8.8.8", true}, {"1.1.1.1", true}, {"45.0.1.22", false}, {"127.0.0.1", false}, {"10.0.0.1", false}, {"169.254.169.254", false}, {"100.64.0.1", false}, {"192.0.2.1", false}, {"198.18.0.1", false}, {"224.0.0.1", false}, {"255.255.255.255", false},
		{"::1", false}, {"::ffff:127.0.0.1", false}, {"::ffff:45.0.1.22", false}, {"64:ff9b::808:808", false}, {"2001:db8::1", false}, {"2002:c000:201::1", false}, {"2606:4700::1111", false}, {"2607:f8b0:4000::1", true}, {"fe80::1%en0", false},
	} {
		t.Run(tc.address, func(t *testing.T) {
			got := p.Allowed(netip.MustParseAddr(tc.address))
			if got != tc.allowed {
				t.Fatalf("allowed %v", got)
			}
		})
	}
	if _, err = NewPolicy(nil, nil); err == nil {
		t.Fatal("accepted missing infrastructure policy")
	}
	if _, err = NewPolicy([]netip.Prefix{netip.MustParsePrefix("::ffff:192.168.0.0/112")}, nil); err == nil {
		t.Fatal("accepted ambiguous mapped prefix")
	}
}

func TestInfrastructureExceptionRequiresExactRoute(t *testing.T) {
	allow := Exception{Recipient: "Test@mailhost.example", MX: "mail.mailhost.example", Address: netip.MustParseAddr("45.0.1.21")}
	p, err := NewPolicy([]netip.Prefix{netip.MustParsePrefix("45.0.1.0/24")}, []netip.Addr{netip.MustParseAddr("45.0.1.62")}, allow)
	if err != nil {
		t.Fatal(err)
	}
	withoutException, err := NewPolicy(p.local, []netip.Addr{p.source4})
	if err != nil || withoutException.AllowedFor(allow.Address, allow.Recipient, allow.MX) {
		t.Fatal("a peer without the explicit exception allowed the route", err)
	}
	for _, tc := range []struct {
		name, recipient, mx, ip string
		want                    bool
	}{
		{"exact", "Test@mailhost.example", "mail.mailhost.example", "45.0.1.21", true},
		{"domain case", "Test@MAILHOST.EXAMPLE", "MAIL.MAILHOST.EXAMPLE", "45.0.1.21", true},
		{"local part case", "test@mailhost.example", "mail.mailhost.example", "45.0.1.21", false},
		{"other recipient", "other@mailhost.example", "mail.mailhost.example", "45.0.1.21", false},
		{"other mx", "Test@mailhost.example", "else.mailhost.example", "45.0.1.21", false},
		{"other address", "Test@mailhost.example", "mail.mailhost.example", "45.0.1.22", false},
		{"source address", "Test@mailhost.example", "mail.mailhost.example", "45.0.1.62", false},
		{"mapped address", "Test@mailhost.example", "mail.mailhost.example", "::ffff:45.0.1.21", false},
		{"loopback", "Test@mailhost.example", "mail.mailhost.example", "127.0.0.1", false},
		{"ordinary public route", "other@example.com", "mx.example.com", "8.8.8.8", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := p.AllowedFor(netip.MustParseAddr(tc.ip), tc.recipient, tc.mx)
			if got != tc.want {
				t.Fatalf("AllowedFor = %v, want %v", got, tc.want)
			}
		})
	}
	called := false
	dial := func(_ context.Context, source, destination netip.Addr) (net.Conn, error) {
		called = true
		if source.String() != "45.0.1.62" || destination != allow.Address {
			t.Fatal("exception did not pin source and destination")
		}
		return nil, nil
	}
	if _, err = p.dial(context.Background(), netip.Addr{}, allow.Address, "other@mailhost.example", allow.MX, dial); err == nil || called {
		t.Fatal("unapproved recipient reached the dialer")
	}
	if _, err = p.dial(context.Background(), netip.Addr{}, allow.Address, allow.Recipient, allow.MX, dial); err != nil || !called {
		t.Fatal("approved route was not passed to the dialer", err)
	}
	for _, invalid := range []Exception{
		{Recipient: allow.Recipient, MX: allow.MX, Address: netip.MustParseAddr("127.0.0.1")},
		{Recipient: allow.Recipient, MX: allow.MX, Address: netip.MustParseAddr("45.0.1.62")},
		{Recipient: allow.Recipient, MX: allow.MX, Address: netip.MustParseAddr("8.8.8.8")},
		{Recipient: allow.Recipient, MX: allow.MX, Address: netip.MustParseAddr("::ffff:45.0.1.21")},
		{Recipient: "name <Test@mailhost.example>", MX: allow.MX, Address: allow.Address},
	} {
		if _, err = NewPolicy(p.local, []netip.Addr{p.source4}, invalid); err == nil {
			t.Fatal("accepted an unsafe exception")
		}
	}
	if _, err = NewPolicy(p.local, []netip.Addr{p.source4}, allow, allow); err == nil {
		t.Fatal("accepted duplicate exceptions")
	}
}

func TestLoadExceptionsIsBoundedAndStrict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exceptions")
	if got, err := LoadExceptions(""); err != nil || len(got) != 0 {
		t.Fatal("optional exceptions unexpectedly required")
	}
	for _, tc := range []struct {
		name, content string
		valid         bool
	}{
		{"valid", "# comment\nTest@mailhost.example mail.mailhost.example 45.0.1.21\n", true},
		{"empty", "# empty\n", false},
		{"extra field", "a@example.com mx.example.com 45.0.1.21 extra\n", false},
		{"mapped", "a@example.com mx.example.com ::ffff:45.0.1.21\n", false},
		{"invalid mailbox", "invalid mx.example.com 45.0.1.21\n", false},
		{"bad MX", "a@example.com mx..example.com 45.0.1.21\n", false},
		{"too large", strings.Repeat("#1234567890\n", 1500), false},
		{"too many", strings.Repeat("a@example.com mx.example.com 45.0.1.21\n", maxExceptions+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			entries, err := LoadExceptions(path)
			if (err == nil) != tc.valid || tc.valid && len(entries) != 1 {
				t.Fatalf("got %d entries, error %v", len(entries), err)
			}
		})
	}
	if _, err := LoadExceptions(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("accepted a missing configured file")
	}
}
func TestDialPinsAddressAndSMTPPort(t *testing.T) {
	p, err := NewPolicy([]netip.Prefix{netip.MustParsePrefix("45.0.1.0/24")}, []netip.Addr{netip.MustParseAddr("45.0.1.22")})
	if err != nil {
		t.Fatal(err)
	}
	var seenSource, seenDestination netip.Addr
	dial := func(_ context.Context, source, destination netip.Addr) (net.Conn, error) {
		seenSource, seenDestination = source, destination
		return nil, nil
	}
	if _, err = p.dial(context.Background(), netip.Addr{}, netip.MustParseAddr("8.8.8.8"), "", "", dial); err != nil || seenSource.String() != "45.0.1.22" || seenDestination.String() != "8.8.8.8" {
		t.Fatalf("unexpected source %s destination %s error %v", seenSource, seenDestination, err)
	}
	seenSource = netip.Addr{}
	if _, err = p.dial(context.Background(), netip.Addr{}, netip.MustParseAddr("127.0.0.1"), "", "", dial); err == nil || seenSource.IsValid() {
		t.Fatal("dialed blocked address")
	}
	if _, err = p.dial(context.Background(), netip.Addr{}, netip.MustParseAddr("2607:f8b0:4000::1"), "", "", dial); err == nil {
		t.Fatal("dialed without configured IPv6 source")
	}
	if p.Allowed(netip.MustParseAddr("2607:f8b0:4000::1")) {
		t.Fatal("selected an address family without a source")
	}
	if _, err = NewPolicy([]netip.Prefix{netip.MustParsePrefix("45.0.1.0/24")}, nil); err == nil {
		t.Fatal("accepted missing source")
	}
}
