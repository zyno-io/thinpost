package dispatch

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/zyno-io/thinpost/internal/delivery"
)

var (
	primaryAddr = netip.MustParseAddr("45.0.1.22")
	hostingAddr = netip.MustParseAddr("45.0.0.10")
)

func explicitCaps(addresses ...netip.Addr) delivery.EdgeCapabilities {
	return delivery.EdgeCapabilities{Explicit: true, Addresses: addresses, BounceDomains: []string{"bounce.example", "bounce.hosting.example"}}
}

// Control must never hand a worker a delivery whose source it did not report.
// The claim query already filters; this is the assertion behind it, and the
// only guard for an old worker that would sign off on fields it does not know.
func TestSourceFieldsAreSetOnlyForAddressesTheWorkerReported(t *testing.T) {
	hosting := delivery.Claim{DeliveryID: "delivery", SourceKey: "source", FinalKey: "final", EnhancedProtection: true, SourceAddress: hostingAddr.String(), HeloName: "smtp.hosting.example", SourceName: "hosting", BounceDomain: "bounce.hosting.example"}
	// The real claim shape for a legacy worker: the fields are cleared, only the
	// resolved row (a pinned hosting IP that shares the global bounce domain) remains.
	legacyShape := delivery.Claim{DeliveryID: "delivery", SourceKey: "source", FinalKey: "final", EnhancedProtection: true, ResolvedAddress: hostingAddr.String(), SourceName: "hosting", BounceDomain: "bounce.example"}
	for name, tc := range map[string]struct {
		claim delivery.Claim
		caps  delivery.EdgeCapabilities
		want  string // "" = sent with empty fields, "sent" = with fields, "held" = committed as a local rejection
	}{
		"explicit worker with the address":                            {withResolved(hosting), explicitCaps(primaryAddr, hostingAddr), "sent"},
		"explicit worker without the address":                         {hosting, explicitCaps(primaryAddr), "held"},
		"legacy worker, hosting row sharing the global bounce domain": {legacyShape, delivery.EdgeCapabilities{Addresses: []netip.Addr{primaryAddr}, BounceDomains: []string{"bounce.example"}}, "held"},
		"legacy worker, claim carries a source":                       {hosting, delivery.EdgeCapabilities{Addresses: []netip.Addr{primaryAddr}, BounceDomains: []string{"bounce.example"}}, "held"},
		"legacy worker with zero capabilities":                        {hosting, delivery.EdgeCapabilities{}, "held"},
		"source without a HELO name":                                  {withHelo(hosting, ""), explicitCaps(primaryAddr, hostingAddr), "held"},
		"HELO name without a source":                                  {withSource(hosting, ""), explicitCaps(primaryAddr, hostingAddr), "held"},
		"bounce domain the worker did not report":                     {withBounce(hosting, "bounce.other.example"), explicitCaps(primaryAddr, hostingAddr), "held"},
		"legacy worker, unassigned app":                               {delivery.Claim{DeliveryID: "delivery", SourceKey: "source", FinalKey: "final", EnhancedProtection: true}, delivery.EdgeCapabilities{Addresses: []netip.Addr{primaryAddr}, BounceDomains: []string{"bounce.example"}}, ""},
		"explicit worker, unassigned app":                             {delivery.Claim{DeliveryID: "delivery", SourceKey: "source", FinalKey: "final", EnhancedProtection: true}, explicitCaps(primaryAddr), ""},
		"legacy worker, global bounce domain":                         {delivery.Claim{DeliveryID: "delivery", SourceKey: "source", FinalKey: "final", EnhancedProtection: true, BounceDomain: "bounce.example"}, delivery.EdgeCapabilities{Addresses: []netip.Addr{primaryAddr}, BounceDomains: []string{"bounce.example"}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			p, r, _, lease := setup(t)
			r.claim = tc.claim
			start, mime, err := p.Next(context.Background(), lease, tc.caps)
			switch tc.want {
			case "held":
				if !errors.Is(err, delivery.ErrNoWork) || start != nil || mime != nil || len(r.results) != 1 || r.results[0].Outcome != "rejected" || r.results[0].SMTPCode != 0 || r.results[0].JournalID != "" {
					t.Fatalf("delivery with an unreported source was not held: start=%v results=%+v err=%v", start, r.results, err)
				}
			case "sent":
				if err != nil || start.SourceAddress != hostingAddr.String() || start.HeloName != "smtp.hosting.example" {
					t.Fatalf("start %v err %v", start, err)
				}
			default:
				if err != nil || start.SourceAddress != "" || start.HeloName != "" {
					t.Fatalf("a delivery that needs no source carried one: %v %v", start, err)
				}
			}
		})
	}
}

func withHelo(c delivery.Claim, helo string) delivery.Claim   { c.HeloName = helo; return c }
func withSource(c delivery.Claim, addr string) delivery.Claim { c.SourceAddress = addr; return c }
func withBounce(c delivery.Claim, domain string) delivery.Claim {
	c.BounceDomain = domain
	return c
}

func withResolved(c delivery.Claim) delivery.Claim { c.ResolvedAddress = c.SourceAddress; return c }
