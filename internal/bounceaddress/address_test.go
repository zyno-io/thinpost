package bounceaddress

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/delivery"
)

type repository struct {
	hashes map[string][32]byte
	err    error
}

func (r *repository) AssignBounceAddress(_ context.Context, _ delivery.Claim, active string, hashes map[string][32]byte) (string, error) {
	r.hashes = hashes
	return active, r.err
}

func TestOpaqueAddressIsBoundToDeliveryAndPersistedHash(t *testing.T) {
	r := &repository{}
	s := &Service{Repository: r, ActiveVersion: "v1", Keys: map[string][]byte{"v1": []byte("0123456789abcdef0123456789abcdef")}}
	claim := delivery.Claim{DeliveryID: "00000000000000000000000000000001", BounceDomain: "mx.example.com"}
	address, err := s.ForDelivery(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	local, domain, ok := strings.Cut(address, "@")
	if !ok || domain != claim.BounceDomain || len(local) != 34 || !strings.HasPrefix(local, "b+") || strings.Contains(local, claim.DeliveryID) {
		t.Fatalf("invalid address %q", address)
	}
	if r.hashes["v1"] != sha256.Sum256([]byte(local[2:])) {
		t.Fatal("token hash mismatch")
	}
	claim.DeliveryID = "00000000000000000000000000000002"
	other, err := s.ForDelivery(context.Background(), claim)
	if err != nil || other == address {
		t.Fatal("delivery addresses not distinct", err)
	}
	r.err = errors.New("storage failure")
	if _, err = s.ForDelivery(context.Background(), claim); err == nil {
		t.Fatal("address returned without durable assignment")
	}
	claim.BounceDomain = "BAD.example.com"
	if _, err = s.ForDelivery(context.Background(), claim); err == nil {
		t.Fatal("noncanonical domain accepted")
	}
}

// There is no service-wide domain to fall back on: a claim without one gets no
// return path, and nothing is assigned.
func TestClaimWithoutBounceDomainGetsNoAddress(t *testing.T) {
	r := &repository{}
	s := &Service{Repository: r, ActiveVersion: "v1", Keys: map[string][]byte{"v1": []byte("0123456789abcdef0123456789abcdef")}}
	if address, err := s.ForDelivery(context.Background(), delivery.Claim{DeliveryID: "00000000000000000000000000000001"}); err == nil || address != "" || r.hashes != nil {
		t.Fatalf("address %q without a bounce domain: %v", address, err)
	}
}
