package dispatch

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/zyno-io/thinpost/internal/delivery"
)

const (
	primaryHost = "mx.example"
	hostingHost = "smtp.hosting.example"
)

var (
	primaryAddr = netip.MustParseAddr("45.0.1.22")
	hostingAddr = netip.MustParseAddr("45.0.0.10")
)

// primaryCaps is a worker that reported one source and its name.
func primaryCaps() delivery.EdgeCapabilities { return reportedCaps(primaryAddr) }

// reportedCaps is a worker that reported the given sources, primary first, with
// their names.
func reportedCaps(addresses ...netip.Addr) delivery.EdgeCapabilities {
	names := map[netip.Addr]string{primaryAddr: primaryHost, hostingAddr: hostingHost}
	caps := delivery.EdgeCapabilities{}
	for _, address := range addresses {
		caps.Sources = append(caps.Sources, delivery.Source{Address: address, Hostname: names[address]})
	}
	return caps
}

// Control must never hand a worker a delivery whose source or bounce domain
// does not match what it reported. The claim already addresses deliveries from
// the report; this is the assertion behind it, and the only guard for an old
// worker that would sign off on fields it does not know.
func TestSourceFieldsAreSetOnlyForAddressesTheWorkerReported(t *testing.T) {
	base := delivery.Claim{DeliveryID: "delivery", SourceKey: "source", FinalKey: "final", PreparedFormat: 1, EnhancedProtection: true}
	pinned := base
	pinned.SourceAddress, pinned.HeloName, pinned.BounceDomain, pinned.SourceName, pinned.ResolvedAddress = hostingAddr.String(), hostingHost, hostingHost, "hosting", hostingAddr.String()
	unpinned := base
	unpinned.SourceAddress, unpinned.HeloName, unpinned.BounceDomain = primaryAddr.String(), primaryHost, primaryHost
	unpinnedEmpty := base
	unpinnedEmpty.BounceDomain = primaryHost
	for name, tc := range map[string]struct {
		claim delivery.Claim
		caps  delivery.EdgeCapabilities
		want  string // "sent" = with the expected fields, "held" = committed as a local rejection
	}{
		"pinned app, address reported":                       {pinned, reportedCaps(primaryAddr, hostingAddr), "sent"},
		"pinned app, address not reported":                   {pinned, reportedCaps(primaryAddr), "held"},
		"pinned app, worker reported nothing":                {pinned, delivery.EdgeCapabilities{}, "held"},
		"source without a HELO name":                         {withHelo(pinned, ""), reportedCaps(primaryAddr, hostingAddr), "held"},
		"HELO name without a source":                         {withSource(pinned, ""), reportedCaps(primaryAddr, hostingAddr), "held"},
		"HELO name that is not the reported name":            {withHelo(pinned, primaryHost), reportedCaps(primaryAddr, hostingAddr), "held"},
		"bounce domain that is not the source's name":        {withBounce(pinned, primaryHost), reportedCaps(primaryAddr, hostingAddr), "held"},
		"bounce domain the worker did not report":            {withBounce(pinned, "bounce.other.example"), reportedCaps(primaryAddr, hostingAddr), "held"},
		"claim with no bounce domain":                        {withBounce(pinned, ""), reportedCaps(primaryAddr, hostingAddr), "held"},
		"unpinned app, on the primary":                       {unpinned, reportedCaps(primaryAddr, hostingAddr), "sent"},
		"unpinned app without source fields":                 {unpinnedEmpty, reportedCaps(primaryAddr, hostingAddr), "held"},
		"unpinned app, no bounce domain":                     {withBounce(unpinned, ""), reportedCaps(primaryAddr), "held"},
		"unpinned app, bounce domain of a non-primary":       {withBounce(unpinned, hostingHost), reportedCaps(primaryAddr, hostingAddr), "held"},
		"unpinned app, worker reported nothing":              {unpinned, delivery.EdgeCapabilities{}, "held"},
		"unpinned app, global-style domain the worker lacks": {withBounce(unpinned, "bounce.example"), reportedCaps(primaryAddr), "held"},
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
				if err != nil || start.SourceAddress != tc.claim.SourceAddress || start.HeloName != tc.claim.HeloName || start.SourceAddress == "" || start.HeloName == "" {
					t.Fatalf("start %v err %v", start, err)
				}
			default:
				t.Fatalf("unknown expectation %q", tc.want)
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
