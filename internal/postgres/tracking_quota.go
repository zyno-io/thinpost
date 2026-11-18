package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
)

// The UTC date never moves backward for an existing counter. A clock rollback
// therefore cannot grant a fresh budget until time advances past its stored day.
const trackingQuotaProjection = `tracking_daily_limit,
	CASE WHEN tracking_quota_day<(statement_timestamp() AT TIME ZONE 'UTC')::date THEN 0 ELSE tracking_quota_used END,
	CASE WHEN tracking_quota_day<(statement_timestamp() AT TIME ZONE 'UTC')::date THEN 0 ELSE tracking_quota_suppressed END,
	to_char(GREATEST(tracking_quota_day,(statement_timestamp() AT TIME ZONE 'UTC')::date),'YYYY-MM-DD')`

func (r *Repository) GetTrackingQuota(ctx context.Context, appID string) (admin.TrackingQuota, error) {
	var quota admin.TrackingQuota
	if !opaqueID(appID) {
		return quota, admin.ErrInvalidInput
	}
	err := r.DB.QueryRowContext(ctx, `SELECT `+trackingQuotaProjection+` FROM apps WHERE id=$1`, appID).Scan(&quota.DailyLimit, &quota.Used, &quota.Suppressed, &quota.Day)
	if errors.Is(err, sql.ErrNoRows) {
		return quota, admin.ErrNotFound
	}
	return quota, err
}

func (r *Repository) SetTrackingQuota(ctx context.Context, actor, appID string, limit int) (admin.TrackingQuota, error) {
	var quota admin.TrackingQuota
	if !adminText(actor, 256) || !opaqueID(appID) || limit < 0 || limit > 1000000 {
		return quota, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return quota, err
	}
	defer tx.Rollback()
	var oldLimit int
	err = tx.QueryRowContext(ctx, `SELECT tracking_daily_limit FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&oldLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return quota, admin.ErrNotFound
	}
	if err != nil {
		return quota, err
	}
	err = tx.QueryRowContext(ctx, `UPDATE apps SET tracking_daily_limit=$2 WHERE id=$1 RETURNING `+trackingQuotaProjection, appID, limit).Scan(&quota.DailyLimit, &quota.Used, &quota.Suppressed, &quota.Day)
	if err != nil {
		return admin.TrackingQuota{}, err
	}
	auditID, err := newOpaqueID()
	if err != nil {
		return admin.TrackingQuota{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
		VALUES ($1,$2,$3,'app.tracking_quota.updated',$3,jsonb_build_object('previous_daily_limit',$4::integer,'daily_limit',$5::integer))`, auditID, actor, appID, oldLimit, limit)
	if err != nil {
		return admin.TrackingQuota{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.TrackingQuota{}, err
	}
	return quota, nil
}
