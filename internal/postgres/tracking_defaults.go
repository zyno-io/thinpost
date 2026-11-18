package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
)

// GetTrackingDefaults reads the app's tracking defaults without the gate, like
// GetListUnsubscribe.
func (r *Repository) GetTrackingDefaults(ctx context.Context, appID string) (admin.TrackingDefaults, error) {
	if !opaqueID(appID) {
		return admin.TrackingDefaults{}, admin.ErrInvalidInput
	}
	var defaults admin.TrackingDefaults
	err := r.DB.QueryRowContext(ctx, `SELECT default_track_opens,default_track_clicks FROM apps WHERE id=$1`, appID).Scan(&defaults.Opens, &defaults.Clicks)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.TrackingDefaults{}, admin.ErrNotFound
	}
	return defaults, err
}

// SetTrackingDefaults changes the app's defaults under the open gate and audits
// a change as app.tracking_defaults.updated with the previous and new values.
// An identical value is a no-op with no audit row. When canEnable is false,
// turning a default on relative to the locked stored value is ErrConflict.
func (r *Repository) SetTrackingDefaults(ctx context.Context, actor, appID string, v admin.TrackingDefaults, canEnable bool) (admin.TrackingDefaults, error) {
	if !adminText(actor, 256) || !opaqueID(appID) {
		return admin.TrackingDefaults{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.TrackingDefaults{}, err
	}
	defer tx.Rollback()
	var previous admin.TrackingDefaults
	err = tx.QueryRowContext(ctx, `SELECT default_track_opens,default_track_clicks FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&previous.Opens, &previous.Clicks)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.TrackingDefaults{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.TrackingDefaults{}, err
	}
	if !canEnable && (v.Opens && !previous.Opens || v.Clicks && !previous.Clicks) {
		return admin.TrackingDefaults{}, admin.ErrConflict
	}
	if v != previous {
		if _, err = tx.ExecContext(ctx, `UPDATE apps SET default_track_opens=$2,default_track_clicks=$3 WHERE id=$1`, appID, v.Opens, v.Clicks); err != nil {
			return admin.TrackingDefaults{}, err
		}
		auditID, idErr := newOpaqueID()
		if idErr != nil {
			return admin.TrackingDefaults{}, idErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
			VALUES ($1,$2,$3,'app.tracking_defaults.updated',$3,jsonb_build_object('previous',jsonb_build_object('opens',$4::boolean,'clicks',$5::boolean),'opens',$6::boolean,'clicks',$7::boolean))`,
			auditID, actor, appID, previous.Opens, previous.Clicks, v.Opens, v.Clicks); err != nil {
			return admin.TrackingDefaults{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return admin.TrackingDefaults{}, err
	}
	return v, nil
}
