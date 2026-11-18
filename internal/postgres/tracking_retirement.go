package postgres

import (
	"context"
	"errors"
	"time"
)

// RetireExpiredTrackingMappings removes at most limit encrypted-object
// references for expired, accepted or permanently failed deliveries. It
// retains link and webhook history. Queued, in-flight, uncertain and held
// deliveries are never retired by this operation. An independently fenced
// orphan collector is responsible for eventually deleting the ciphertext.
//
// This operation is not scheduled by the runtime. It requires a separate
// maintenance identity with reviewed UPDATE privileges on tracking_objects;
// the normal runtime role must not have them. Gate and audit changes are in
// the same transaction, and retrying after an ambiguous commit is safe.
func (r *Repository) RetireExpiredTrackingMappings(ctx context.Context, actor string, limit int) (int, error) {
	if r == nil || !adminText(actor, 256) || limit < 1 || limit > 100 {
		return 0, errors.New("invalid tracking retirement request")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// The trigger checks this transaction-local context and records the audit
	// even when a maintenance credential writes the retirement columns directly.
	if _, err = tx.ExecContext(ctx, `SELECT set_config('thinpost.retirement_actor',$1,true),set_config('thinpost.retirement_generation',$2,true)`, actor, r.Generation); err != nil {
		return 0, errors.New("tracking retirement unavailable")
	}
	rows, err := tx.QueryContext(ctx, `SELECT o.delivery_id FROM tracking_objects o
		JOIN deliveries d ON d.id=o.delivery_id
		WHERE o.object_key IS NOT NULL AND o.expires_at<=clock_timestamp() AND d.state IN ('accepted','failed')
		ORDER BY o.expires_at,o.delivery_id LIMIT $1 FOR UPDATE OF o,d SKIP LOCKED`, limit)
	if err != nil {
		return 0, errors.New("tracking retirement unavailable")
	}
	var candidates []string
	for rows.Next() {
		var deliveryID string
		if err = rows.Scan(&deliveryID); err != nil {
			rows.Close()
			return 0, errors.New("tracking retirement unavailable")
		}
		candidates = append(candidates, deliveryID)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, errors.New("tracking retirement unavailable")
	}
	rows.Close()
	retired := 0
	for _, deliveryID := range candidates {
		// Recheck after both locks. A backwards clock change or a concurrent
		// status change must not retire a newly ineligible mapping.
		result, updateErr := tx.ExecContext(ctx, `UPDATE tracking_objects o SET object_key=NULL,retired_at=clock_timestamp()
			WHERE o.delivery_id=$1 AND o.object_key IS NOT NULL AND o.expires_at<=clock_timestamp()
			AND EXISTS(SELECT 1 FROM deliveries d WHERE d.id=o.delivery_id AND d.state IN ('accepted','failed'))`, deliveryID)
		if updateErr != nil {
			return 0, errors.New("tracking retirement unavailable")
		}
		n, countErr := result.RowsAffected()
		if countErr != nil || n > 1 {
			return 0, errors.New("tracking retirement unavailable")
		}
		if n == 0 {
			continue
		}
		retired++
	}
	if err = tx.Commit(); err != nil {
		return 0, errors.New("tracking retirement commit not confirmed")
	}
	return retired, nil
}
