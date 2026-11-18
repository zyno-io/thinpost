package tracking

import (
	"context"
	"errors"
	"time"
)

// Repository performs app-bound reference lookups and atomically records a
// permitted visit and its webhook effects. The second operation must recheck
// the reference after object storage was read, including expiry and revocation.
type Repository interface {
	LookupTrackingObject(context.Context, [32]byte, string) (string, error)
	RecordTrackingVisit(context.Context, [32]byte, string, string, bool) error
}

// Service composes committed references and encrypted objects for analytics.
// Encrypted link responses can succeed independently of this service.
type Service struct {
	Repository Repository
	Objects    *ProtectedStore
}

func (s *Service) Visit(ctx context.Context, hash [32]byte, kind string, record bool) error {
	if s == nil || s.Repository == nil || s.Objects == nil || (kind != "open" && kind != "click") {
		return errors.New("tracking unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	key, err := s.Repository.LookupTrackingObject(ctx, hash, kind)
	if err != nil {
		return err
	}
	// The object must still decrypt and match the reference, but the redirect
	// destination always comes from the link, never from storage.
	if _, err = s.Objects.Lookup(ctx, key, hash, kind); err != nil {
		// A committed reference to a missing or inconsistent encrypted object
		// is a storage failure, not evidence that the token never existed.
		return errors.New("tracking object unavailable")
	}
	return s.Repository.RecordTrackingVisit(ctx, hash, kind, key, record)
}
