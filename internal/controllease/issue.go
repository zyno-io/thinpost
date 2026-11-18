// Package controllease obtains database leases before signing edge proofs.
package controllease

import (
	"context"
	"crypto"
	"errors"
	"log"
	"time"

	"github.com/zyno-io/thinpost/internal/journal"
	"github.com/zyno-io/thinpost/internal/leaseproof"
	"github.com/zyno-io/thinpost/internal/postgres"
)

type Repository interface {
	AcquireControllerLease(context.Context, string, string, time.Duration) (postgres.ControllerLease, error)
	RenewControllerLease(context.Context, postgres.ControllerLease, time.Duration) (postgres.ControllerLease, error)
	ReleaseControllerLease(context.Context, postgres.ControllerLease) (bool, error)
}

// Release relinquishes only the acquired epoch. A superseding lease is left
// untouched, and edge ownership still requires the worker's drain barrier.
func (i *Issuer) Release(ctx context.Context, lease postgres.ControllerLease) error {
	if i.Repository == nil {
		return errors.New("lease issuer is not configured")
	}
	_, err := i.Repository.ReleaseControllerLease(ctx, lease)
	return err
}

type Issuer struct {
	Repository Repository
	Signer     crypto.Signer
	Identity   string
	JournalID  string
}

// Acquire allocates a new stream epoch before signing its exact expiry.
func (i *Issuer) Acquire(ctx context.Context, worker, controller string, ttl time.Duration) (postgres.ControllerLease, []byte, []byte, error) {
	if i.Repository == nil || i.Signer == nil || i.Identity == "" || !journal.ValidIdentity(i.JournalID) {
		return postgres.ControllerLease{}, nil, nil, errors.New("lease issuer is not configured")
	}
	lease, err := i.Repository.AcquireControllerLease(ctx, worker, controller, ttl)
	if err != nil {
		return postgres.ControllerLease{}, nil, nil, err
	}
	signed, payload, signature, signErr := i.sign(lease, worker, controller)
	if signErr != nil && lease.WorkerID == worker && lease.ControllerID == controller && lease.Epoch > 0 {
		// Acquisition committed before signing. Avoid retaining an unusable
		// lease until expiry when the signer or proof is unavailable.
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if _, releaseErr := i.Repository.ReleaseControllerLease(cleanup, lease); releaseErr != nil {
			log.Print("failed to release unsigned controller lease; waiting for expiry")
		}
	}
	return signed, payload, signature, signErr
}

// Renew signs only an extension of the exact epoch already in use.
func (i *Issuer) Renew(ctx context.Context, current postgres.ControllerLease, ttl time.Duration) (postgres.ControllerLease, []byte, []byte, error) {
	if i.Repository == nil || i.Signer == nil || i.Identity == "" || !journal.ValidIdentity(i.JournalID) {
		return postgres.ControllerLease{}, nil, nil, errors.New("lease issuer is not configured")
	}
	lease, err := i.Repository.RenewControllerLease(ctx, current, ttl)
	if err != nil {
		return postgres.ControllerLease{}, nil, nil, err
	}
	if lease.Epoch != current.Epoch {
		return postgres.ControllerLease{}, nil, nil, errors.New("database changed lease epoch during renewal")
	}
	return i.sign(lease, current.WorkerID, current.ControllerID)
}

func (i *Issuer) sign(lease postgres.ControllerLease, worker, controller string) (postgres.ControllerLease, []byte, []byte, error) {
	if lease.WorkerID != worker || lease.ControllerID != controller || lease.Epoch == 0 || lease.ExpiresAt.IsZero() {
		return postgres.ControllerLease{}, nil, nil, errors.New("database returned an invalid lease")
	}
	payload, signature, err := leaseproof.Sign(leaseproof.Lease{WorkerID: lease.WorkerID, ControllerID: lease.ControllerID, JournalID: i.JournalID, Identity: i.Identity, Epoch: lease.Epoch, ExpiresUnixMilli: lease.ExpiresAt.UnixMilli()}, i.Signer)
	if err != nil {
		return postgres.ControllerLease{}, nil, nil, err
	}
	return lease, payload, signature, nil
}
