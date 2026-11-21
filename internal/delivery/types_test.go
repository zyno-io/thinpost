package delivery

import (
	"net/netip"
	"testing"
)

func TestHasHostname(t *testing.T) {
	caps := EdgeCapabilities{Sources: []Source{
		{Address: netip.MustParseAddr("45.0.1.22"), Hostname: "mx.example.net"},
		{Address: netip.MustParseAddr("45.0.0.10"), Hostname: "smtp.example.org"},
	}}
	for name, want := range map[string]bool{"mx.example.net": true, "smtp.example.org": true, "bounces.customer.example": false, "": false, "MX.example.net": false} {
		if caps.HasHostname(name) != want {
			t.Errorf("%q", name)
		}
	}
	if (EdgeCapabilities{}).HasHostname("mx.example.net") {
		t.Error("a worker that reported nothing has no names")
	}
}
