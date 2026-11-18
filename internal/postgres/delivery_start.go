package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/journal"
)

// AuthorizeDeliveryStart durably records that this exact descriptor may reach
// SMTP. It holds the current lease and attempt locks through commit. A caller
// must authenticate the stream and compare the descriptor before invoking it.
func (r *Repository) AuthorizeDeliveryStart(ctx context.Context, lease ControllerLease, attemptID, deliveryID, sessionID, journalID string, descriptor [32]byte, expectedCutoff int64) error {
	if !leaseID(lease.WorkerID) || !leaseID(lease.ControllerID) || lease.Epoch == 0 || !opaqueID(attemptID) || !opaqueID(deliveryID) || !opaqueID(sessionID) || !journal.ValidIdentity(journalID) {
		return errors.New("invalid start authorization")
	}
	if expectedCutoff < 0 {
		return errors.New("invalid start authorization")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	held, err := lockLiveControllerLease(ctx, tx, lease)
	if err != nil {
		return err
	}
	if !held {
		return delivery.ErrLeaseInvalid
	}
	// Serialize new permits with deactivation. A permit committed before the
	// app is disabled may already have reached Edge and cannot be recalled.
	var active bool
	err = tx.QueryRowContext(ctx, `SELECT app.active FROM apps app JOIN messages m ON m.app_id=app.id JOIN deliveries d ON d.message_id=m.id WHERE d.id=$1 FOR SHARE OF app`, deliveryID).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return delivery.ErrAttemptSuperseded
	}
	if err != nil {
		return err
	}
	if !active {
		return delivery.ErrAppInactive
	}
	var authorized bool
	var stored []byte
	var session, storedJournal sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT a.start_authorized,a.start_descriptor_hash,a.start_session_id,a.start_journal_id FROM delivery_attempts a JOIN deliveries d ON d.id=a.delivery_id
		WHERE a.id=$1 AND a.delivery_id=$2 AND a.worker_id=$3 AND a.epoch=$4 AND a.state='in_flight' AND d.state='in_flight' AND d.current_attempt_id=a.id FOR UPDATE OF a,d`, attemptID, deliveryID, lease.WorkerID, lease.Epoch).Scan(&authorized, &stored, &session, &storedJournal)
	if errors.Is(err, sql.ErrNoRows) {
		return delivery.ErrAttemptSuperseded
	}
	if err != nil {
		return err
	}
	held, err = lockLiveControllerLease(ctx, tx, lease)
	if err != nil {
		return err
	}
	if !held {
		return delivery.ErrLeaseInvalid
	}
	// Recheck under the attempt lock: a mapping may have expired or been
	// revoked after claim. No permit is issued for stale tracking links.
	var trackingUsable bool
	var cutoff sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT object_key IS NOT NULL AND revoked_at IS NULL AND (expires_at IS NULL OR (post_send_validity_seconds IS NOT NULL AND expires_at-post_send_validity_seconds*interval '1 second'>clock_timestamp()+$2*interval '1 millisecond')),expires_at-post_send_validity_seconds*interval '1 second' FROM tracking_objects WHERE delivery_id=$1 FOR SHARE`, deliveryID, delivery.TrackingMinimumRemaining.Milliseconds()).Scan(&trackingUsable, &cutoff)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && !trackingUsable {
		return delivery.ErrTrackingUnavailable
	}
	actualCutoff := int64(0)
	if cutoff.Valid {
		actualCutoff = cutoff.Time.UnixMilli()
	}
	if expectedCutoff != actualCutoff {
		return delivery.ErrResultConflict
	}
	if authorized {
		if !session.Valid || session.String != sessionID || !storedJournal.Valid || storedJournal.String != journalID || !bytes.Equal(stored, descriptor[:]) {
			return delivery.ErrResultConflict
		}
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE delivery_attempts SET start_authorized=true,start_descriptor_hash=$2,start_session_id=$3,start_journal_id=$4 WHERE id=$1`, attemptID, descriptor[:], sessionID, journalID); err != nil {
		return err
	}
	return tx.Commit()
}
