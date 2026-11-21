package egress

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDestinationPolicy(t *testing.T) {
	p, err := NewPolicy([]netip.Prefix{netip.MustParsePrefix("45.0.1.0/24"), netip.MustParsePrefix("2606:4700::/32")})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		address string
		allowed bool
	}{
		{"8.8.8.8", true}, {"1.1.1.1", true}, {"45.0.1.22", false}, {"127.0.0.1", false}, {"10.0.0.1", false}, {"169.254.169.254", false}, {"100.64.0.1", false}, {"192.0.2.1", false}, {"198.18.0.1", false}, {"224.0.0.1", false}, {"255.255.255.255", false},
		{"::1", false}, {"::ffff:127.0.0.1", false}, {"::ffff:45.0.1.22", false}, {"64:ff9b::808:808", false}, {"2001:db8::1", false}, {"2002:c000:201::1", false}, {"2607:f8b0:4000::1", false}, {"fe80::1%en0", false},
	} {
		t.Run(tc.address, func(t *testing.T) {
			got := p.Allowed(netip.MustParseAddr(tc.address))
			if got != tc.allowed {
				t.Fatalf("allowed %v", got)
			}
		})
	}
	if _, err = NewPolicy(nil); err == nil {
		t.Fatal("accepted missing infrastructure policy")
	}
	if _, err = NewPolicy([]netip.Prefix{netip.MustParsePrefix("::ffff:192.168.0.0/112")}); err == nil {
		t.Fatal("accepted ambiguous mapped prefix")
	}
}

func TestInfrastructureExceptionRequiresExactRoute(t *testing.T) {
	allow := Exception{Recipient: "Test@mailhost.example", MX: "mail.mailhost.example", Address: netip.MustParseAddr("45.0.1.21")}
	p, err := NewPolicy([]netip.Prefix{netip.MustParsePrefix("45.0.1.0/24")}, allow)
	if err != nil {
		t.Fatal(err)
	}
	withoutException, err := NewPolicy(p.local)
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
	for _, invalid := range []Exception{
		{Recipient: allow.Recipient, MX: allow.MX, Address: netip.MustParseAddr("127.0.0.1")},
		{Recipient: allow.Recipient, MX: allow.MX, Address: netip.MustParseAddr("8.8.8.8")},
		{Recipient: allow.Recipient, MX: allow.MX, Address: netip.MustParseAddr("::ffff:45.0.1.21")},
		{Recipient: "name <Test@mailhost.example>", MX: allow.MX, Address: allow.Address},
	} {
		if _, err = NewPolicy(p.local, invalid); err == nil {
			t.Fatal("accepted an unsafe exception")
		}
	}
	if _, err = NewPolicy(p.local, allow, allow); err == nil {
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

// Sources are not configured: only the Edge's report names them. The policy
// still allows public IPv4 and denies infrastructure ranges and IPv6.
func TestPolicyWithoutSourcesDialsPublicIPv4Only(t *testing.T) {
	p, err := NewPolicy([]netip.Prefix{netip.MustParsePrefix("45.0.1.0/24")})
	if err != nil {
		t.Fatal(err)
	}
	for address, want := range map[string]bool{"8.8.8.8": true, "45.0.1.22": false, "10.0.0.1": false, "2607:f8b0:4000::1": false, "::ffff:8.8.8.8": true} {
		if got := p.Allowed(netip.MustParseAddr(address)); got != want {
			t.Errorf("Allowed(%s)=%v", address, got)
		}
	}
}
