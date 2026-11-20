package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
)

const maxPendingMessages = 100000

// Count messages rather than recipients. The pending flag starts true and is
// cleared transactionally only when all deliveries are terminal. A prepared
// message missing deliveries stays pending. LIMIT bounds counted rows;
// concurrent completions can make the count temporarily conservative.
// Messages submitted with the console limits override are excluded: they were
// never counted against the limit, so they must not consume it for later
// submissions either. The operations backlog still counts them.
// Any future operation that changes a terminal message back to pending must
// hold the same application lock and enforce the limit before doing so.
func countPendingMessages(ctx context.Context, q rowQueryer, appID string, limit int) (int, error) {
	var pending int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM (
		SELECT 1 FROM messages WHERE app_id=$1 AND pending AND NOT limits_override LIMIT $2
	) pending_messages`, appID, limit).Scan(&pending)
	return pending, err
}

func readPendingQuota(ctx context.Context, q rowQueryer, appID string, limit int) (admin.PendingQuota, error) {
	pending, err := countPendingMessages(ctx, q, appID, maxPendingMessages+1)
	if err != nil {
		return admin.PendingQuota{}, err
	}
	quota := admin.PendingQuota{Limit: limit, Pending: pending}
	if pending > maxPendingMessages {
		quota.Pending = maxPendingMessages
		quota.Truncated = true
	}
	return quota, nil
}

func (r *Repository) GetPendingQuota(ctx context.Context, appID string) (admin.PendingQuota, error) {
	if !opaqueID(appID) {
		return admin.PendingQuota{}, admin.ErrInvalidInput
	}
	var limit int
	err := r.DB.QueryRowContext(ctx, `SELECT pending_message_limit FROM apps WHERE id=$1`, appID).Scan(&limit)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.PendingQuota{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.PendingQuota{}, err
	}
	return readPendingQuota(ctx, r.DB, appID, limit)
}

func (r *Repository) SetPendingQuota(ctx context.Context, actor, appID string, limit int) (admin.PendingQuota, error) {
	if !adminText(actor, 256) || !opaqueID(appID) || limit < 0 || limit > maxPendingMessages {
		return admin.PendingQuota{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.PendingQuota{}, err
	}
	defer tx.Rollback()
	var oldLimit int
	err = tx.QueryRowContext(ctx, `SELECT pending_message_limit FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&oldLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.PendingQuota{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.PendingQuota{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE apps SET pending_message_limit=$2 WHERE id=$1`, appID, limit); err != nil {
		return admin.PendingQuota{}, err
	}
	auditID, err := newOpaqueID()
	if err != nil {
		return admin.PendingQuota{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
		VALUES ($1,$2,$3,'app.pending_quota.updated',$3,jsonb_build_object('previous_limit',$4::integer,'limit',$5::integer))`, auditID, actor, appID, oldLimit, limit); err != nil {
		return admin.PendingQuota{}, err
	}
	quota, err := readPendingQuota(ctx, tx, appID, limit)
	if err != nil {
		return admin.PendingQuota{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.PendingQuota{}, err
	}
	return quota, nil
}
