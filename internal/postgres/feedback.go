package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/feedback"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/tracking"
)

// Recipient feedback never stores or logs an address, request body or header.
// The suppressed address is copied inside SQL from message_recipients, so it
// does not pass through Go, an error or a log line.

// RecordUnsubscribe commits a one-click unsubscribe for the delivery in one
// transaction: the recipient_feedback row, the app's suppression
// (unsubscribe/recipient, precedence-ranked) and the unsubscribed webhook event
// with its fan-out. A repeat is a no-op returning nil. The delivery state is
// untouched, and a paused app still records. A missing delivery or recipient
// row (pre-schema-28 messages) is tracking.ErrNotFound; a closed gate or any
// database failure is a non-nil error for the caller to report as 503. The
// caller must already have verified the signed link.
func (r *Repository) RecordUnsubscribe(ctx context.Context, deliveryID string) error {
	if !opaqueID(deliveryID) {
		return tracking.ErrNotFound
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var appID, messageID string
	err = tx.QueryRowContext(ctx, `SELECT m.app_id,m.id FROM deliveries d JOIN messages m ON m.id=d.message_id
		JOIN message_recipients r ON r.message_id=d.message_id AND r.recipient_index=d.recipient_index
		WHERE d.id=$1`, deliveryID).Scan(&appID, &messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return tracking.ErrNotFound
	}
	if err != nil {
		return errors.New("unsubscribe unavailable")
	}
	id, err := newOpaqueID()
	if err != nil {
		return errors.New("unsubscribe unavailable")
	}
	var occurred sql.NullTime
	err = tx.QueryRowContext(ctx, `INSERT INTO recipient_feedback(id,delivery_id,kind,channel) VALUES ($1,$2,'unsubscribe','one_click')
		ON CONFLICT (delivery_id,kind) DO NOTHING RETURNING received_at`, id, deliveryID).Scan(&occurred)
	if errors.Is(err, sql.ErrNoRows) {
		// Already recorded (a repeat, or a concurrent request that committed
		// first): no second feedback row or event. The suppression upsert still
		// runs, so a console removal since the first request is undone by the
		// recipient's latest request.
		if err = upsertRecipientSuppression(ctx, tx, deliveryID, "unsubscribe"); err != nil {
			return errors.New("unsubscribe unavailable")
		}
		if err = countFeedback(ctx, tx, "one_click", feedback.Duplicate); err != nil {
			return errors.New("unsubscribe unavailable")
		}
		// Normally already queued by the first request; this covers an
		// unsubscribe recorded some other way before the recipient's POST.
		queued, queueErr := queueForward(ctx, tx, deliveryID)
		if queueErr != nil {
			return errors.New("unsubscribe unavailable")
		}
		if err = tx.Commit(); err != nil {
			return errors.New("unsubscribe commit not confirmed")
		}
		if queued {
			oplog.UnsubscribeForward(appID, messageID, deliveryID, "queued")
		}
		return nil
	}
	if err != nil {
		return errors.New("unsubscribe unavailable")
	}
	if err = upsertRecipientSuppression(ctx, tx, deliveryID, "unsubscribe"); err != nil {
		return errors.New("unsubscribe unavailable")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,outcome,smtp_code,occurred_at,feedback_id)
		VALUES ($1,$2,$3,$4,'unsubscribed',0,$5,$1)`, id, appID, messageID, deliveryID, occurred.Time); err != nil {
		return errors.New("unsubscribe unavailable")
	}
	// A legacy app over the active-subscription cap still records the
	// unsubscribe and its event; only the fan-out is skipped.
	if err = enqueueWebhookDeliveries(ctx, tx, id, appID); err != nil && !errors.Is(err, errWebhookFanoutExceeded) {
		return errors.New("unsubscribe unavailable")
	}
	if err = countFeedback(ctx, tx, "one_click", feedback.Accepted); err != nil {
		return errors.New("unsubscribe unavailable")
	}
	// Also call the sender's own unsubscribe link when the message had one that
	// can take a one-click POST. Queued in this transaction, so the unsubscribe
	// and its forward commit together; the POST itself is sent later by a
	// worker, and its failure never affects the recipient's unsubscribe.
	queued, err := queueForward(ctx, tx, deliveryID)
	if err != nil {
		return errors.New("unsubscribe unavailable")
	}
	if err = tx.Commit(); err != nil {
		return errors.New("unsubscribe commit not confirmed")
	}
	if queued {
		oplog.UnsubscribeForward(appID, messageID, deliveryID, "queued")
	}
	return nil
}

// upsertRecipientSuppression copies the delivery's recipient into the app's
// suppression list with the given reason (unsubscribe or spam_complaint) and
// origin recipient. A recipient-originated entry replaces a weaker one
// (spam_complaint > unsubscribe > hard_bounce > manual) and never a stronger or
// equal one. The address stays inside SQL.
func upsertRecipientSuppression(ctx context.Context, tx *sql.Tx, deliveryID, reason string) error {
	suppressionID, err := newOpaqueID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO suppressions(id,app_id,address,address_lower,domain_lower,reason,origin,source_message_id)
		SELECT $1,r.app_id,r.address,r.address_lower,r.domain_lower,$3::text,'recipient',r.message_id
		FROM deliveries d JOIN message_recipients r ON r.message_id=d.message_id AND r.recipient_index=d.recipient_index WHERE d.id=$2
		ON CONFLICT (app_id,address_lower) DO UPDATE SET reason=EXCLUDED.reason,origin=EXCLUDED.origin,source_message_id=EXCLUDED.source_message_id
		WHERE `+suppressionRank("suppressions.reason")+` < `+suppressionRank("EXCLUDED.reason"), suppressionID, deliveryID, reason)
	return err
}

// suppressionRank is the SQL precedence of a suppression reason.
func suppressionRank(column string) string {
	return `(CASE ` + column + ` WHEN 'spam_complaint' THEN 4 WHEN 'unsubscribe' THEN 3 WHEN 'hard_bounce' THEN 2 ELSE 1 END)`
}

func (r *Repository) GetListUnsubscribe(ctx context.Context, appID string) (admin.ListUnsubscribe, error) {
	if !opaqueID(appID) {
		return admin.ListUnsubscribe{}, admin.ErrInvalidInput
	}
	var config admin.ListUnsubscribe
	err := r.DB.QueryRowContext(ctx, `SELECT list_unsubscribe FROM apps WHERE id=$1`, appID).Scan(&config.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ListUnsubscribe{}, admin.ErrNotFound
	}
	return config, err
}

// SetListUnsubscribe changes the app's one-click opt-in under the open gate and
// audits a change as app.list_unsubscribe.updated. An identical value is a
// no-op with no audit row. The caller enforces the tracking-runtime check.
func (r *Repository) SetListUnsubscribe(ctx context.Context, actor, appID string, enabled bool) (admin.ListUnsubscribe, error) {
	if !adminText(actor, 256) || !opaqueID(appID) {
		return admin.ListUnsubscribe{}, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.ListUnsubscribe{}, err
	}
	defer tx.Rollback()
	var previous bool
	err = tx.QueryRowContext(ctx, `SELECT list_unsubscribe FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ListUnsubscribe{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.ListUnsubscribe{}, err
	}
	if previous != enabled {
		if _, err = tx.ExecContext(ctx, `UPDATE apps SET list_unsubscribe=$2 WHERE id=$1`, appID, enabled); err != nil {
			return admin.ListUnsubscribe{}, err
		}
		auditID, idErr := newOpaqueID()
		if idErr != nil {
			return admin.ListUnsubscribe{}, idErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
			VALUES ($1,$2,$3,'app.list_unsubscribe.updated',$3,jsonb_build_object('previous',$4::boolean,'enabled',$5::boolean))`, auditID, actor, appID, previous, enabled); err != nil {
			return admin.ListUnsubscribe{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return admin.ListUnsubscribe{}, err
	}
	return admin.ListUnsubscribe{Enabled: enabled}, nil
}
