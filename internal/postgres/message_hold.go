package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/submission"
)

// A message submitted on hold is inserted in state on_hold with
// hold_expires_at set. Preparation claims only state queued, so an on_hold
// message is never prepared and has no deliveries to dispatch. It leaves the
// hold exactly once, under the message row lock:
//
//	on_hold -> queued        release (hold_resolved_at = release time)
//	on_hold -> cancelled     cancel  (pending cleared; terminal)
//	on_hold -> hold_expired  expiry  (pending cleared; terminal)
//
// A released message then follows the normal path. Each transition, and the
// acceptance on hold, creates a message-level webhook event.
const (
	holdEventHeld      = "held"
	holdEventReleased  = "released"
	holdEventCancelled = "cancelled"
	holdEventExpired   = "hold_expired"
)

// initialMessageState is the state and hold lifetime (milliseconds, or nil
// for no hold) of a newly accepted message.
func initialMessageState(hold bool) (string, any) {
	if !hold {
		return "queued", nil
	}
	return "on_hold", submission.MaxHold.Milliseconds()
}

// insertMessageEvent records a message-level event (no delivery, SMTP code 0)
// and queues it for the app's endpoints in the caller's transaction.
func insertMessageEvent(ctx context.Context, tx *sql.Tx, appID, messageID, outcome string) error {
	eventID, err := newOpaqueID()
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code) VALUES ($1,$2,$3,$4,0)`, eventID, appID, messageID, outcome); err != nil {
		return err
	}
	return enqueueWebhookDeliveries(ctx, tx, eventID, appID)
}

// ReleaseHeldMessage queues a message submitted on hold for preparation. See
// admin.AppHoldStore.
func (r *Repository) ReleaseHeldMessage(ctx context.Context, appID, keyID, messageID string) error {
	return r.resolveHold(ctx, appID, keyID, messageID, true)
}

// CancelHeldMessage ends a message submitted on hold without sending it. See
// admin.AppHoldStore.
func (r *Repository) CancelHeldMessage(ctx context.Context, appID, keyID, messageID string) error {
	return r.resolveHold(ctx, appID, keyID, messageID, false)
}

func (r *Repository) resolveHold(ctx context.Context, appID, keyID, messageID string, release bool) error {
	if !opaqueID(appID) || !adminText(keyID, 256) || !opaqueID(messageID) {
		return admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The same authorization as submission, rechecked under lock: a paused app
	// or a key revoked after authentication cannot act. FOR SHARE makes a
	// concurrent pause or revocation wait for this transaction.
	var allowed bool
	err = tx.QueryRowContext(ctx, `SELECT a.active AND k.active FROM apps a JOIN api_keys k ON k.app_id=a.id WHERE a.id=$1 AND k.id=$2 FOR SHARE OF a,k`, appID, keyID).Scan(&allowed)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !allowed {
		return submission.ErrInvalidKey
	}
	if err != nil {
		return err
	}
	// The row lock serializes release, cancel, expiry and repeats of each. NO
	// KEY UPDATE is enough (the key never changes) and does not block
	// concurrent event inserts that reference the message.
	var state string
	var held, resolved, expired bool
	err = tx.QueryRowContext(ctx, `SELECT state,hold_expires_at IS NOT NULL,hold_resolved_at IS NOT NULL,COALESCE(hold_expires_at<=clock_timestamp(),false)
		FROM messages WHERE id=$1 AND app_id=$2 FOR NO KEY UPDATE`, messageID, appID).Scan(&state, &held, &resolved, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	switch {
	case !held:
		return admin.HoldNotHeld
	case state == "on_hold" && expired:
		// The periodic expiry has not reached it yet. Expire it here so the
		// refusal and the stored state agree.
		if err = expireHoldTx(ctx, tx, appID, messageID); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		oplog.Message(appID, messageID, oplog.StatusHoldExpired)
		return admin.HoldHasExpired
	case state == "on_hold":
		return r.commitHoldResolution(ctx, tx, appID, keyID, messageID, release)
	case state == "hold_expired":
		return admin.HoldHasExpired
	case state == "cancelled":
		if release {
			return admin.HoldWasCancelled
		}
		return nil
	case state == "recovery_hold" && !resolved:
		return admin.HoldInRecovery
	}
	// Released earlier; the message has moved on (and may since have been
	// quarantined by a recovery). A repeat release is a no-op.
	if release {
		return nil
	}
	return admin.HoldWasReleased
}

// commitHoldResolution applies a release or cancel to the locked on_hold row,
// records its event and audit, and commits.
func (r *Repository) commitHoldResolution(ctx context.Context, tx *sql.Tx, appID, keyID, messageID string, release bool) error {
	query := `UPDATE messages SET state='queued',prepare_next_at=clock_timestamp(),hold_resolved_at=clock_timestamp() WHERE id=$1 AND state='on_hold'`
	event, action, status := holdEventReleased, "message.hold.released", oplog.StatusReleased
	if !release {
		query = `UPDATE messages SET state='cancelled',pending=false,hold_resolved_at=clock_timestamp() WHERE id=$1 AND state='on_hold'`
		event, action, status = holdEventCancelled, "message.hold.cancelled", oplog.StatusCancelled
	}
	result, err := tx.ExecContext(ctx, query, messageID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.New("held message changed under lock")
	}
	if err = insertMessageEvent(ctx, tx, appID, messageID, event); err != nil {
		return err
	}
	if err = recordAudit(ctx, tx, "key:"+keyID, appID, action, messageID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	oplog.Message(appID, messageID, status)
	return nil
}

// expireHoldTx ends one locked, expired on_hold message. The resolution time
// is the expiry time, not the time the expiry was processed.
func expireHoldTx(ctx context.Context, tx *sql.Tx, appID, messageID string) error {
	result, err := tx.ExecContext(ctx, `UPDATE messages SET state='hold_expired',pending=false,hold_resolved_at=hold_expires_at
		WHERE id=$1 AND state='on_hold' AND hold_expires_at<=clock_timestamp()`, messageID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.New("held message changed under lock")
	}
	return insertMessageEvent(ctx, tx, appID, messageID, holdEventExpired)
}

// ExpireHeldMessages ends at most limit messages whose hold expired, oldest
// first, in one transaction. Competing replicas skip rows another holds, and
// a release or cancel racing the expiry waits for the row lock and then sees
// the terminal state. It returns how many it expired.
func (r *Repository) ExpireHeldMessages(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 1000 {
		return 0, errors.New("invalid hold expiry limit")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,app_id FROM messages WHERE state='on_hold' AND hold_expires_at<=clock_timestamp()
		ORDER BY hold_expires_at,id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type expiredHold struct{ id, app string }
	var expired []expiredHold
	for rows.Next() {
		var item expiredHold
		if err = rows.Scan(&item.id, &item.app); err != nil {
			rows.Close()
			return 0, err
		}
		expired = append(expired, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, item := range expired {
		if err = expireHoldTx(ctx, tx, item.app, item.id); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	for _, item := range expired {
		oplog.Message(item.app, item.id, oplog.StatusHoldExpired)
	}
	return len(expired), nil
}

// readMessageHold returns the hold of a message submitted on hold, or nil.
// The resolution follows from the state read with it: a resolved hold whose
// message is not cancelled or expired was released.
func readMessageHold(ctx context.Context, tx *sql.Tx, appID, messageID string) (*admin.MessageHold, error) {
	var state string
	var expires, resolved sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT state,hold_expires_at,hold_resolved_at FROM messages WHERE id=$1 AND app_id=$2`, messageID, appID).Scan(&state, &expires, &resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, admin.ErrNotFound
	}
	if err != nil || !expires.Valid {
		return nil, err
	}
	hold := &admin.MessageHold{ExpiresAt: expires.Time}
	if resolved.Valid {
		at := resolved.Time
		hold.ResolvedAt = &at
		switch state {
		case "cancelled":
			hold.Resolution = admin.HoldCancelled
		case "hold_expired":
			hold.Resolution = admin.HoldExpired
		default:
			hold.Resolution = admin.HoldReleased
		}
	}
	return hold, nil
}
