// Package bounceaddress derives opaque, per-delivery SMTP return paths.
package bounceaddress

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/delivery"
)

// Repository chooses an existing version, or assigns the active one, and
// durably records its token hash for the current delivery attempt.
type Repository interface {
	AssignBounceAddress(context.Context, delivery.Claim, string, map[string][32]byte) (string, error)
}

// Service requires a shared, stable keyring on all replicas. Old keys must
// remain available for as long as deliveries using them can be retried.
type Service struct {
	Repository    Repository
	ActiveVersion string
	Keys          map[string][]byte
}

// Validate checks configuration without reading or writing a delivery.
func (s *Service) Validate() error {
	if s.Repository == nil || len(s.Keys) == 0 || len(s.Keys) > 32 {
		return errors.New("invalid bounce address configuration")
	}
	for version, key := range s.Keys {
		if len(version) == 0 || len(version) > 64 || len(key) < 32 {
			return errors.New("invalid bounce keyring")
		}
	}
	if _, ok := s.Keys[s.ActiveVersion]; !ok {
		return errors.New("active bounce key unavailable")
	}
	return nil
}

func (s *Service) ForDelivery(ctx context.Context, claim delivery.Claim) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	if claim.DeliveryID == "" {
		return "", errors.New("invalid delivery")
	}
	// The domain is the one the claim carries, the reported name of the
	// address the delivery leaves from; there is no fallback. The token does not
	// depend on it, so a delivery whose domain changes still correlates.
	domain, ok := admin.NormalizeDomain(claim.BounceDomain)
	if !ok || domain != claim.BounceDomain || len(domain) > 219 {
		return "", errors.New("invalid bounce domain")
	}
	tokens := make(map[string]string, len(s.Keys))
	hashes := make(map[string][32]byte, len(s.Keys))
	for version, key := range s.Keys {
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte("thinpost-bounce-v1\x00" + claim.DeliveryID))
		token := hex.EncodeToString(mac.Sum(nil)[:16])
		tokens[version] = token
		hashes[version] = sha256.Sum256([]byte(token))
	}
	version, err := s.Repository.AssignBounceAddress(ctx, claim, s.ActiveVersion, hashes)
	if err != nil {
		return "", err
	}
	token, ok := tokens[version]
	if !ok {
		return "", errors.New("delivery bounce key unavailable")
	}
	return "b+" + token + "@" + domain, nil
}
