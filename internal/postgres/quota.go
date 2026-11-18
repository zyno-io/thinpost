package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
)

func (r *Repository) GetRecipientQuota(ctx context.Context, appID string) (admin.RecipientQuota, error) {
	var quota admin.RecipientQuota
	if !opaqueID(appID) {
		return quota, admin.ErrInvalidInput
	}
	err := r.DB.QueryRowContext(ctx, `SELECT recipient_daily_limit,
		CASE WHEN recipient_quota_day=(statement_timestamp() AT TIME ZONE 'UTC')::date THEN recipient_quota_used ELSE 0 END,
		to_char((statement_timestamp() AT TIME ZONE 'UTC')::date,'YYYY-MM-DD') FROM apps WHERE id=$1`, appID).Scan(&quota.DailyLimit, &quota.Used, &quota.Day)
	if errors.Is(err, sql.ErrNoRows) {
		return quota, admin.ErrNotFound
	}
	return quota, err
}

func (r *Repository) SetRecipientQuota(ctx context.Context, actor, appID string, limit int) (admin.RecipientQuota, error) {
	var quota admin.RecipientQuota
	if !adminText(actor, 256) || !opaqueID(appID) || limit < 0 || limit > 1000000 {
		return quota, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return quota, err
	}
	defer tx.Rollback()
	var oldLimit int
	err = tx.QueryRowContext(ctx, `SELECT recipient_daily_limit FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&oldLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return quota, admin.ErrNotFound
	}
	if err != nil {
		return quota, err
	}
	err = tx.QueryRowContext(ctx, `UPDATE apps SET recipient_daily_limit=$2 WHERE id=$1 RETURNING recipient_daily_limit,
		CASE WHEN recipient_quota_day=(statement_timestamp() AT TIME ZONE 'UTC')::date THEN recipient_quota_used ELSE 0 END,
		to_char((statement_timestamp() AT TIME ZONE 'UTC')::date,'YYYY-MM-DD')`, appID, limit).Scan(&quota.DailyLimit, &quota.Used, &quota.Day)
	if err != nil {
		return admin.RecipientQuota{}, err
	}
	auditID, err := newOpaqueID()
	if err != nil {
		return admin.RecipientQuota{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
		VALUES ($1,$2,$3,'app.recipient_quota.updated',$3,jsonb_build_object('previous_daily_limit',$4::integer,'daily_limit',$5::integer))`, auditID, actor, appID, oldLimit, limit)
	if err != nil {
		return admin.RecipientQuota{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.RecipientQuota{}, err
	}
	return quota, nil
}
