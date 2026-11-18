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
	s := &Service{Repository: r, Domain: "bounce.example.com", ActiveVersion: "v1", Keys: map[string][]byte{"v1": []byte("0123456789abcdef0123456789abcdef")}}
	claim := delivery.Claim{DeliveryID: "00000000000000000000000000000001"}
	address, err := s.ForDelivery(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	local, domain, ok := strings.Cut(address, "@")
	if !ok || domain != s.Domain || len(local) != 34 || !strings.HasPrefix(local, "b+") || strings.Contains(local, claim.DeliveryID) {
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
	s.Domain = "BAD.example.com"
	if _, err = s.ForDelivery(context.Background(), claim); err == nil {
		t.Fatal("noncanonical domain accepted")
	}
}
