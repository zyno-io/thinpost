package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
)

// RetryPolicyHold only requeues an explicitly held delivery. Both Trusted and
// Edge must recheck their current policy before any SMTP start is authorized.
func (r *Repository) RetryPolicyHold(ctx context.Context, actor, appID, messageID, deliveryID string) error {
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(messageID) || !opaqueID(deliveryID) {
		return admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active bool
	err = tx.QueryRowContext(ctx, `SELECT active FROM apps WHERE id=$1 FOR SHARE`, appID).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	if !active {
		return admin.ErrConflict
	}
	var state, messageState string
	var current sql.NullString
	var pending bool
	err = tx.QueryRowContext(ctx, `SELECT d.state,d.current_attempt_id,m.state,m.pending FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE d.id=$1 AND m.id=$2 AND m.app_id=$3 FOR UPDATE OF d`, deliveryID, messageID, appID).Scan(&state, &current, &messageState, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != "recipient_policy_held" || current.Valid || messageState != "prepared" || !pending {
		return admin.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='queued',retry_after=clock_timestamp() WHERE id=$1`, deliveryID); err != nil {
		return err
	}
	if err = recordAudit(ctx, tx, actor, appID, "delivery.recipient_policy.retry", deliveryID); err != nil {
		return err
	}
	return tx.Commit()
}
