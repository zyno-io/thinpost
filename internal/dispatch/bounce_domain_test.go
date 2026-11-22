package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/zyno-io/thinpost/internal/delivery"
)

// A custom bounce domain is accepted only for the exact name the claim verified
// it against, only from a worker that takes bounces at any domain, and never
// when it is one of the worker's own names.
func TestCustomBounceDomain(t *testing.T) {
	const custom = "bounces.customer.example"
	base := delivery.Claim{DeliveryID: "delivery", SourceKey: "source", FinalKey: "final", PreparedFormat: 1, EnhancedProtection: true,
		SourceAddress: primaryAddr.String(), HeloName: primaryHost, BounceDomain: custom, BounceTarget: primaryHost}
	anyDomain := func(c delivery.EdgeCapabilities) delivery.EdgeCapabilities { c.AnyBounceDomain = true; return c }
	for name, tc := range map[string]struct {
		claim delivery.Claim
		caps  delivery.EdgeCapabilities
		sent  bool
	}{
		"custom domain, capable worker, verified target":   {base, anyDomain(primaryCaps()), true},
		"worker without bounce-any-domain/1":               {base, primaryCaps(), false},
		"target is another name than the source's":         {withTarget(base, hostingHost), anyDomain(reportedCaps(primaryAddr, hostingAddr)), false},
		"no target recorded":                               {withTarget(base, ""), anyDomain(primaryCaps()), false},
		"custom domain is the name of another worker IP":   {withBounce(base, hostingHost), anyDomain(reportedCaps(primaryAddr, hostingAddr)), false},
		"custom domain is the source's own name (no-op)":   {withTarget(withBounce(base, primaryHost), ""), primaryCaps(), true},
		"empty bounce domain":                              {withBounce(base, ""), anyDomain(primaryCaps()), false},
		"HELO name still must be the reported name":        {withHelo(base, hostingHost), anyDomain(primaryCaps()), false},
		"worker renamed an address to the custom domain":   {base, renamed(anyDomain(reportedCaps(primaryAddr, hostingAddr)), 1, custom), false},
		"custom domain, pinned source, target is its name": {pinnedCustom(), anyDomain(reportedCaps(primaryAddr, hostingAddr)), true},
		"custom domain, pinned source, target is the main": {withTarget(pinnedCustom(), primaryHost), anyDomain(reportedCaps(primaryAddr, hostingAddr)), false},
	} {
		t.Run(name, func(t *testing.T) {
			p, r, _, lease := setup(t)
			r.claim = tc.claim
			start, _, err := p.Next(context.Background(), lease, tc.caps)
			if tc.sent {
				if err != nil || start == nil || start.HeloName != tc.claim.HeloName {
					t.Fatalf("not sent: %v %v", start, err)
				}
				return
			}
			if !errors.Is(err, delivery.ErrNoWork) || start != nil || len(r.results) != 1 || r.results[0].Outcome != "rejected" {
				t.Fatalf("delivery was not held: %v %+v %v", start, r.results, err)
			}
		})
	}
}

func withTarget(c delivery.Claim, target string) delivery.Claim { c.BounceTarget = target; return c }

func renamed(c delivery.EdgeCapabilities, index int, name string) delivery.EdgeCapabilities {
	sources := append([]delivery.Source(nil), c.Sources...)
	sources[index].Hostname = name
	c.Sources = sources
	return c
}

func pinnedCustom() delivery.Claim {
	return delivery.Claim{DeliveryID: "delivery", SourceKey: "source", FinalKey: "final", PreparedFormat: 1, EnhancedProtection: true,
		SourceAddress: hostingAddr.String(), ResolvedAddress: hostingAddr.String(), HeloName: hostingHost,
		BounceDomain: "bounces.customer.example", BounceTarget: hostingHost}
}
