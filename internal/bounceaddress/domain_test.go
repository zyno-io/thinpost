package bounceaddress

import (
	"context"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/delivery"
)

// The return path uses the claim's domain; the token (and its stored hash)
// depends only on the delivery, so a delivery whose domain changes still
// correlates.
func TestClaimBounceDomainIsUsedAndTokenIsUnchanged(t *testing.T) {
	r := &repository{}
	s := &Service{Repository: r, ActiveVersion: "v1", Keys: map[string][]byte{"v1": []byte("0123456789abcdef0123456789abcdef")}}
	claim := delivery.Claim{DeliveryID: "00000000000000000000000000000001", BounceDomain: "mx.example.com"}
	first, err := s.ForDelivery(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	firstHash := r.hashes["v1"]
	claim.BounceDomain = "smtp.hosting.example.org"
	own, err := s.ForDelivery(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	localFirst, firstDomain, _ := strings.Cut(first, "@")
	localOwn, domain, _ := strings.Cut(own, "@")
	if firstDomain != "mx.example.com" || domain != "smtp.hosting.example.org" || localOwn != localFirst || r.hashes["v1"] != firstHash {
		t.Fatalf("own %q first %q", own, first)
	}
	for _, bad := range []string{"", "Bounce.Hosting.example.org", "localhost", "a b.example.org", "45.0.0.10"} {
		claim.BounceDomain = bad
		if _, err = s.ForDelivery(context.Background(), claim); err == nil {
			t.Fatalf("accepted bounce domain %q", bad)
		}
	}
}
