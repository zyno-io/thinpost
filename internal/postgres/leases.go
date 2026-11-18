package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrLeaseHeld = errors.New("worker controller lease is held")

type ControllerLease struct {
	WorkerID     string
	ControllerID string
	Epoch        uint64
	ExpiresAt    time.Time
}

// AcquireControllerLease allocates a fresh epoch for every stream, including a
// reconnect by the same controller. It waits for transactions holding the old
// lease row before advancing the epoch, fencing their delivery claims.
// Controller IDs must identify process instances, and callers must bind them
// to the authenticated control connection before activating an edge fence.
func (r *Repository) AcquireControllerLease(ctx context.Context, workerID, controllerID string, ttl time.Duration) (ControllerLease, error) {
	if !leaseID(workerID) || !leaseID(controllerID) || ttl < 5*time.Second || ttl > 5*time.Minute {
		return ControllerLease{}, errors.New("invalid controller lease request")
	}
	var lease ControllerLease
	lease.WorkerID, lease.ControllerID = workerID, controllerID
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return ControllerLease{}, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `INSERT INTO controller_leases (worker_id, controller_id, epoch, expires_at)
		VALUES ($1,$2,1,clock_timestamp()+$3*interval '1 millisecond')
		ON CONFLICT (worker_id) DO UPDATE SET
		controller_id=EXCLUDED.controller_id,
		epoch=controller_leases.epoch+1,
		expires_at=clock_timestamp()+$3*interval '1 millisecond'
		WHERE (controller_leases.controller_id=EXCLUDED.controller_id OR controller_leases.expires_at<=clock_timestamp()) AND controller_leases.epoch<9223372036854775807
		RETURNING epoch,expires_at`, workerID, controllerID, ttl.Milliseconds()).Scan(&lease.Epoch, &lease.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ControllerLease{}, ErrLeaseHeld
	}
	if err != nil {
		return ControllerLease{}, err
	}
	if err = tx.Commit(); err != nil {
		return ControllerLease{}, err
	}
	return lease, nil
}

// RenewControllerLease extends only the exact still-live epoch. It never
// acquires a lease or revives one that has expired or been superseded.
func (r *Repository) RenewControllerLease(ctx context.Context, lease ControllerLease, ttl time.Duration) (ControllerLease, error) {
	if !leaseID(lease.WorkerID) || !leaseID(lease.ControllerID) || lease.Epoch == 0 || ttl < 5*time.Second || ttl > 5*time.Minute {
		return ControllerLease{}, errors.New("invalid controller lease renewal")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return ControllerLease{}, err
	}
	defer tx.Rollback()
	var locked bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM controller_leases WHERE worker_id=$1 FOR UPDATE`, lease.WorkerID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return ControllerLease{}, ErrLeaseHeld
	}
	if err != nil {
		return ControllerLease{}, err
	}
	var expiresAt time.Time
	err = tx.QueryRowContext(ctx, `UPDATE controller_leases SET expires_at=clock_timestamp()+$4*interval '1 millisecond'
		WHERE worker_id=$1 AND controller_id=$2 AND epoch=$3 AND expires_at>clock_timestamp()
		RETURNING expires_at`, lease.WorkerID, lease.ControllerID, lease.Epoch, ttl.Milliseconds()).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ControllerLease{}, ErrLeaseHeld
	}
	if err != nil {
		return ControllerLease{}, err
	}
	if err = tx.Commit(); err != nil {
		return ControllerLease{}, err
	}
	lease.ExpiresAt = expiresAt
	return lease, nil
}

// lockLiveControllerLease checks expiry after acquiring the row lock. A
// predicate evaluated before waiting for a lock can otherwise become stale.
func lockLiveControllerLease(ctx context.Context, tx *sql.Tx, lease ControllerLease) (bool, error) {
	var locked bool
	err := tx.QueryRowContext(ctx, `SELECT true FROM controller_leases WHERE worker_id=$1 FOR SHARE`, lease.WorkerID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var live bool
	err = tx.QueryRowContext(ctx, `SELECT controller_id=$2 AND epoch=$3 AND expires_at>clock_timestamp() FROM controller_leases WHERE worker_id=$1`, lease.WorkerID, lease.ControllerID, lease.Epoch).Scan(&live)
	return live, err
}

// ReleaseControllerLease cannot release a newer owner's lease. A relinquished
// lease still requires the edge to observe the next epoch before old streams
// are fenced; in-flight attempts must be reconciled separately.
func (r *Repository) ReleaseControllerLease(ctx context.Context, lease ControllerLease) (bool, error) {
	if !leaseID(lease.WorkerID) || !leaseID(lease.ControllerID) || lease.Epoch == 0 {
		return false, errors.New("invalid controller lease")
	}
	result, err := r.DB.ExecContext(ctx, `UPDATE controller_leases SET expires_at=clock_timestamp() WHERE worker_id=$1 AND controller_id=$2 AND epoch=$3 AND expires_at>clock_timestamp()`, lease.WorkerID, lease.ControllerID, lease.Epoch)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
func leaseID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
