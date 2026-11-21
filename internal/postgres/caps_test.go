package postgres

import (
	"net/netip"

	"github.com/zyno-io/thinpost/internal/delivery"
)

// claimCaps is a worker that reported one source and its name: what an edge
// reports in Ready. An app on the primary is claimed with its bounce domain.
func claimCaps() delivery.EdgeCapabilities {
	return delivery.EdgeCapabilities{Sources: []delivery.Source{{Address: netip.MustParseAddr("45.0.1.22"), Hostname: "mx.example.net"}}}
}
