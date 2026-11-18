package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
)

// AbandonTrackingHold records a terminal failure without resending a signed
// message whose tracking capabilities cannot safely be used. Only a delivery
// held before SMTP can be abandoned here; uncertain attempts need separate
// evidence and disposition. The audit, event and queue accounting are atomic.
func (r *Repository) AbandonTrackingHold(ctx context.Context, actor, appID, messageID, deliveryID string) error {
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(messageID) || !opaqueID(deliveryID) {
		return admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var appExists bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1 FOR SHARE`, appID).Scan(&appExists)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
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
	if state != "tracking_held" || current.Valid || messageState != "prepared" || !pending {
		return admin.ErrConflict
	}
	// A state label alone is not sufficient evidence that this was a local,
	// pre-SMTP hold. Every earlier attempt must also have a recorded,
	// retryable outcome and matching event; uncertainty or incomplete history
	// cannot be resolved by abandoning this hold.
	var safe bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM delivery_attempts a JOIN webhook_events e ON e.attempt_id=a.id
		WHERE a.delivery_id=$1 AND a.state='completed' AND a.outcome='tracking_unavailable'
		AND a.smtp_code=0 AND NOT a.start_authorized AND a.start_journal_id IS NULL
		AND e.delivery_id=$1 AND e.app_id=$2 AND e.message_id=$3
		AND e.outcome='tracking_unavailable' AND e.smtp_code=0
	) AND NOT EXISTS (
		SELECT 1 FROM delivery_attempts a WHERE a.delivery_id=$1 AND (
			(a.state='completed' AND (
				(a.outcome IN ('tracking_unavailable','recipient_policy_denied') AND a.smtp_code=0 AND NOT a.start_authorized AND a.start_journal_id IS NULL)
				OR (a.outcome='rejected' AND (a.smtp_code=0 OR (a.start_authorized AND a.smtp_code BETWEEN 400 AND 499)))
				OR (a.outcome='route_rejected' AND a.start_authorized AND a.smtp_code BETWEEN 400 AND 599)
			) AND (NOT a.start_authorized OR a.start_journal_id IS NOT NULL)) IS NOT TRUE
			OR NOT EXISTS (SELECT 1 FROM webhook_events e WHERE e.attempt_id=a.id
				AND e.delivery_id=$1 AND e.app_id=$2 AND e.message_id=$3
				AND e.outcome=a.outcome AND e.smtp_code=a.smtp_code)
		)
	)`, deliveryID, appID, messageID).Scan(&safe)
	if err != nil {
		return err
	}
	if !safe {
		return admin.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='failed' WHERE id=$1`, deliveryID); err != nil {
		return err
	}
	if err = clearPendingIfComplete(ctx, tx, deliveryID); err != nil {
		return err
	}
	if err = recordAudit(ctx, tx, actor, appID, "delivery.tracking.abandon", deliveryID); err != nil {
		return err
	}
	eventID, err := newOpaqueID()
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,outcome,smtp_code,delivery_state) VALUES ($1,$2,$3,$4,'tracking_abandoned',0,'failed')`, eventID, appID, messageID, deliveryID); err != nil {
		return err
	}
	if err = enqueueWebhookDeliveries(ctx, tx, eventID, appID); err != nil {
		return err
	}
	return tx.Commit()
}
