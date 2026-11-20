package postgres

import (
	"context"
	"errors"
)

// CleanupExpiredIdempotency removes at most limit expired markers. Competing
// replicas skip locked rows; an active 24-hour or 10-minute marker is never removed.
func (r *Repository) CleanupExpiredIdempotency(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 || limit > 1000 {
		return 0, errors.New("invalid idempotency cleanup limit")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `WITH expired AS (
		SELECT app_id,adapter,key FROM idempotency_keys WHERE expires_at<=now()
		ORDER BY expires_at,app_id,adapter,key FOR UPDATE SKIP LOCKED LIMIT $1
	) DELETE FROM idempotency_keys i USING expired e WHERE i.app_id=e.app_id AND i.adapter=e.adapter AND i.key=e.key`, limit)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return rows, nil
}
