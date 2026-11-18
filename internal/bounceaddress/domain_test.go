package bounceaddress

import (
	"context"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/delivery"
)

// A sending IP's bounce domain replaces the global one in the return path; the
// token (and its stored hash) depends only on the delivery, so it is unchanged.
func TestClaimBounceDomainIsUsedAndTokenIsUnchanged(t *testing.T) {
	r := &repository{}
	s := &Service{Repository: r, Domain: "bounce.example.com", ActiveVersion: "v1", Keys: map[string][]byte{"v1": []byte("0123456789abcdef0123456789abcdef")}}
	claim := delivery.Claim{DeliveryID: "00000000000000000000000000000001"}
	global, err := s.ForDelivery(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	globalHash := r.hashes["v1"]
	claim.BounceDomain = "bounce.hosting.example.org"
	own, err := s.ForDelivery(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	localGlobal, _, _ := strings.Cut(global, "@")
	localOwn, domain, _ := strings.Cut(own, "@")
	if domain != "bounce.hosting.example.org" || localOwn != localGlobal || r.hashes["v1"] != globalHash {
		t.Fatalf("own %q global %q", own, global)
	}
	for _, bad := range []string{"Bounce.Hosting.example.org", "localhost", "a b.example.org", "45.0.0.10"} {
		claim.BounceDomain = bad
		if _, err = s.ForDelivery(context.Background(), claim); err == nil {
			t.Fatalf("accepted bounce domain %q", bad)
		}
	}
}
