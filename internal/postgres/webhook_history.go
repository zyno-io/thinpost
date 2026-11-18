package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
)

const maxVisibleWebhookAttempts = 50

// webhookEventVisible limits history to events whose source still exists: an
// outbound event's message (and delivery, when it has one), or an inbound
// message. Queries alias webhook_events e, messages m and deliveries d.
const webhookEventVisible = `(e.inbound_id IS NOT NULL OR (m.id IS NOT NULL AND (e.delivery_id IS NULL OR d.id IS NOT NULL)))`

type webhookCursor struct {
	At             time.Time `json:"at"`
	EventID        string    `json:"event_id"`
	SubscriptionID string    `json:"subscription_id"`
}

func parseWebhookCursor(value string) (webhookCursor, error) {
	var cursor webhookCursor
	if value == "" {
		return cursor, nil
	}
	if len(value) > 512 {
		return cursor, admin.ErrInvalidInput
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor, admin.ErrInvalidInput
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&cursor); err != nil {
		return webhookCursor{}, admin.ErrInvalidInput
	}
	if err = decoder.Decode(new(any)); err != io.EOF || cursor.At.IsZero() || !opaqueID(cursor.EventID) || !opaqueID(cursor.SubscriptionID) {
		return webhookCursor{}, admin.ErrInvalidInput
	}
	return cursor, nil
}

func encodeWebhookCursor(item admin.WebhookDelivery) (string, error) {
	data, err := json.Marshal(webhookCursor{At: item.OccurredAt, EventID: item.EventID, SubscriptionID: item.SubscriptionID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// ListWebhookDeliveries returns bounded, audited app-scoped metadata. It does
// not expose endpoints, signing references, payloads, or claim tokens.
func (r *Repository) ListWebhookDeliveries(ctx context.Context, actor, appID, cursorText string, limit int) (admin.WebhookDeliveryPage, error) {
	var page admin.WebhookDeliveryPage
	if !adminText(actor, 256) || !opaqueID(appID) || limit < 1 || limit > 100 {
		return page, admin.ErrInvalidInput
	}
	cursor, err := parseWebhookCursor(cursorText)
	if err != nil {
		return page, err
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var found bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1`, appID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return page, admin.ErrNotFound
	}
	if err != nil {
		return page, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "webhook.delivery.list", appID); err != nil {
		return page, err
	}
	var before any
	if !cursor.At.IsZero() {
		before = cursor.At
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.id,w.subscription_id,COALESCE(e.message_id,''),COALESCE(e.inbound_id,''),COALESCE(e.delivery_id,''),e.outcome,w.state,w.attempts,e.occurred_at,
		CASE WHEN w.state='queued' THEN w.next_attempt_at ELSE NULL END,w.delivered_at
		FROM webhook_events e JOIN webhook_deliveries w ON w.event_id=e.id AND w.app_id=e.app_id
		LEFT JOIN messages m ON m.id=e.message_id AND m.app_id=e.app_id
		LEFT JOIN deliveries d ON d.id=e.delivery_id AND d.message_id=m.id
		WHERE e.app_id=$1 AND `+webhookEventVisible+`
		AND ($2::timestamptz IS NULL OR (e.occurred_at,e.id,w.subscription_id)<($2,$3,$4))
		ORDER BY e.occurred_at DESC,e.id DESC,w.subscription_id DESC LIMIT $5`, appID, before, cursor.EventID, cursor.SubscriptionID, limit+1)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var item admin.WebhookDelivery
		var next, delivered sql.NullTime
		if err = rows.Scan(&item.EventID, &item.SubscriptionID, &item.MessageID, &item.InboundID, &item.DeliveryID, &item.Outcome, &item.State, &item.Attempts, &item.OccurredAt, &next, &delivered); err != nil {
			rows.Close()
			return admin.WebhookDeliveryPage{}, err
		}
		if next.Valid {
			item.NextAttemptAt = &next.Time
		}
		if delivered.Valid {
			item.DeliveredAt = &delivered.Time
		}
		page.Deliveries = append(page.Deliveries, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return admin.WebhookDeliveryPage{}, err
	}
	rows.Close()
	if len(page.Deliveries) > limit {
		page.Deliveries = page.Deliveries[:limit]
		page.NextCursor, err = encodeWebhookCursor(page.Deliveries[len(page.Deliveries)-1])
		if err != nil {
			return admin.WebhookDeliveryPage{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return admin.WebhookDeliveryPage{}, err
	}
	return page, nil
}

// ListWebhookAttempts returns at most the latest 50 attempts. It deliberately
// omits their IDs, which are also live claim tokens.
func (r *Repository) ListWebhookAttempts(ctx context.Context, actor, appID, eventID, subscriptionID string) (admin.WebhookAttemptPage, error) {
	var page admin.WebhookAttemptPage
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(eventID) || !opaqueID(subscriptionID) {
		return page, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var found bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM webhook_deliveries w
		JOIN webhook_events e ON e.id=w.event_id AND e.app_id=w.app_id
		LEFT JOIN messages m ON m.id=e.message_id AND m.app_id=e.app_id
		LEFT JOIN deliveries d ON d.id=e.delivery_id AND d.message_id=m.id
		WHERE w.app_id=$1 AND w.event_id=$2 AND w.subscription_id=$3 AND `+webhookEventVisible, appID, eventID, subscriptionID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return page, admin.ErrNotFound
	}
	if err != nil {
		return page, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "webhook.delivery.attempts", eventID+":"+subscriptionID); err != nil {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT state,http_status,started_at,finished_at FROM webhook_attempts
		WHERE event_id=$1 AND subscription_id=$2 ORDER BY started_at DESC,id DESC LIMIT $3`, eventID, subscriptionID, maxVisibleWebhookAttempts+1)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var item admin.WebhookAttempt
		var status sql.NullInt64
		var finished sql.NullTime
		if err = rows.Scan(&item.State, &status, &item.StartedAt, &finished); err != nil {
			rows.Close()
			return admin.WebhookAttemptPage{}, err
		}
		if status.Valid {
			value := int(status.Int64)
			item.HTTPStatus = &value
		}
		if finished.Valid {
			item.FinishedAt = &finished.Time
		}
		page.Attempts = append(page.Attempts, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return admin.WebhookAttemptPage{}, err
	}
	rows.Close()
	if len(page.Attempts) > maxVisibleWebhookAttempts {
		page.Attempts = page.Attempts[:maxVisibleWebhookAttempts]
		page.Truncated = true
	}
	if err = tx.Commit(); err != nil {
		return admin.WebhookAttemptPage{}, err
	}
	return page, nil
}

// ReplayWebhookDelivery requeues only a failed delivery to an active,
// undeleted endpoint. It keeps the stable event ID and previous attempts;
// the receiver may already have accepted an earlier attempt and must dedupe.
func (r *Repository) ReplayWebhookDelivery(ctx context.Context, actor, appID, eventID, subscriptionID string) error {
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(eventID) || !opaqueID(subscriptionID) {
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
	err = tx.QueryRowContext(ctx, `SELECT active FROM webhook_subscriptions WHERE app_id=$1 AND id=$2 FOR SHARE`, appID, subscriptionID).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	if !active {
		return admin.ErrConflict
	}
	var state string
	var expired bool
	err = tx.QueryRowContext(ctx, `SELECT w.state,COALESCE(im.content_expires_at<=clock_timestamp(),false) FROM webhook_deliveries w
		JOIN webhook_events e ON e.id=w.event_id AND e.app_id=w.app_id
		LEFT JOIN messages m ON m.id=e.message_id AND m.app_id=e.app_id
		LEFT JOIN deliveries d ON d.id=e.delivery_id AND d.message_id=m.id
		LEFT JOIN inbound_messages im ON im.id=e.inbound_id AND im.app_id=e.app_id
		WHERE w.app_id=$1 AND w.event_id=$2 AND w.subscription_id=$3 AND `+webhookEventVisible+`
		FOR UPDATE OF w`, appID, eventID, subscriptionID).Scan(&state, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	// An inbound message past its retention is gone for good: sending it again
	// could only fail, so it is refused rather than queued.
	if state != "failed" || expired {
		return admin.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE webhook_deliveries SET state='queued',attempts=0,next_attempt_at=clock_timestamp(),claim_token=NULL,claim_until=NULL,delivered_at=NULL WHERE app_id=$1 AND event_id=$2 AND subscription_id=$3`, appID, eventID, subscriptionID); err != nil {
		return err
	}
	if err = recordAudit(ctx, tx, actor, appID, "webhook.delivery.replay", eventID+":"+subscriptionID); err != nil {
		return err
	}
	return tx.Commit()
}
