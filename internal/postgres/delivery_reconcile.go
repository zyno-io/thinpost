package postgres

import (
	"context"
	"errors"

	"github.com/zyno-io/thinpost/internal/delivery"
)

const reconciliationBatchSize = 128

// ReconcileOrphanedAttempts conservatively holds one bounded batch of older
// attempts. The caller must first verify the authenticated edge drained the
// previous session and acknowledged every journal result. It must not admit
// work until complete is true. This is not safe after an uncoordinated DB or
// journal restore, or if another edge process can still use this worker ID.
func (r *Repository) ReconcileOrphanedAttempts(ctx context.Context, lease ControllerLease) (complete bool, err error) {
	if !leaseID(lease.WorkerID) || !leaseID(lease.ControllerID) || lease.Epoch == 0 {
		return false, errors.New("invalid reconciliation lease")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var locked bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM controller_leases WHERE worker_id=$1 FOR UPDATE`, lease.WorkerID).Scan(&locked)
	if err != nil {
		return false, err
	}
	live, err := lockLiveControllerLease(ctx, tx, lease)
	if err != nil {
		return false, err
	}
	if !live {
		return false, delivery.ErrLeaseInvalid
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.delivery_id,a.epoch FROM delivery_attempts a
		WHERE a.worker_id=$1 AND a.epoch<$2 AND a.state='in_flight'
		AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.id=a.delivery_id AND d.state='recovery_hold')
		ORDER BY a.id LIMIT $3 FOR UPDATE OF a`, lease.WorkerID, lease.Epoch, reconciliationBatchSize)
	if err != nil {
		return false, err
	}
	var results []delivery.Result
	for rows.Next() {
		result := delivery.Result{WorkerID: lease.WorkerID, Outcome: "uncertain"}
		if err = rows.Scan(&result.AttemptID, &result.DeliveryID, &result.Epoch); err != nil {
			rows.Close()
			return false, err
		}
		results = append(results, result)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err = rows.Close(); err != nil {
		return false, err
	}
	for _, result := range results {
		appDeleted, lockErr := lockAppDeleted(ctx, tx, result.DeliveryID)
		if lockErr != nil {
			return false, lockErr
		}
		if err = commitDeliveryResultForTx(ctx, tx, result, true, appDeleted, delivery.DefaultRetryPolicy()); err != nil {
			return false, err
		}
	}
	live, err = lockLiveControllerLease(ctx, tx, lease)
	if err != nil {
		return false, err
	}
	if !live {
		return false, delivery.ErrLeaseInvalid
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return len(results) < reconciliationBatchSize, nil
}
