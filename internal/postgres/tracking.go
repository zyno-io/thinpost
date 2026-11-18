package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/zyno-io/thinpost/internal/tracking"
)

// LookupTrackingObject only exposes the encrypted object key for a committed,
// unexpired, unrevoked capability of the requested kind. It never receives a
// raw bearer token or a destination URL.
func (r *Repository) LookupTrackingObject(ctx context.Context, hash [32]byte, kind string) (string, error) {
	if hash == ([32]byte{}) || (kind != "open" && kind != "click") {
		return "", tracking.ErrNotFound
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var key string
	err = tx.QueryRowContext(ctx, `SELECT o.object_key FROM tracking_links l JOIN tracking_objects o ON o.delivery_id=l.delivery_id
		JOIN deliveries d ON d.id=o.delivery_id JOIN messages m ON m.id=d.message_id
		WHERE l.token_hash=$1 AND l.kind=$2 AND o.object_key IS NOT NULL AND o.revoked_at IS NULL AND (o.expires_at IS NULL OR o.expires_at>clock_timestamp())`, hash[:], kind).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", tracking.ErrNotFound
	}
	if err != nil {
		return "", errors.New("tracking lookup unavailable")
	}
	if err = tx.Commit(); err != nil {
		return "", errors.New("tracking lookup unavailable")
	}
	return key, nil
}

// RecordTrackingVisit rechecks the exact reference after object decryption and
// records at most the snapshotted per-link event limit. Events and all active
// subscription outbox rows commit together. The app's daily event budget is
// shared across replicas. At either limit links still resolve, but an aggregate
// suppressed counter increases instead of creating an event. A legacy excess
// of active webhook subscriptions is suppressed the same way. HTTP and ingress
// rate controls remain required independently. HEAD does not spend budget.
func (r *Repository) RecordTrackingVisit(ctx context.Context, hash [32]byte, kind, objectKey string, record bool) error {
	if hash == ([32]byte{}) || (kind != "open" && kind != "click") || objectKey == "" {
		return tracking.ErrNotFound
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var linkID, deliveryID, messageID, appID string
	var count, limit int
	var occurred time.Time
	var expires, revoked sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT l.id,l.delivery_id,m.id,m.app_id,l.event_count,o.event_limit,o.expires_at,o.revoked_at
		FROM tracking_links l JOIN tracking_objects o ON o.delivery_id=l.delivery_id
		JOIN deliveries d ON d.id=o.delivery_id JOIN messages m ON m.id=d.message_id
		WHERE l.token_hash=$1 AND l.kind=$2 AND o.object_key=$3
		FOR UPDATE OF o,l`, hash[:], kind, objectKey).Scan(&linkID, &deliveryID, &messageID, &appID, &count, &limit, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return tracking.ErrNotFound
	}
	if err != nil {
		return errors.New("tracking visit unavailable")
	}
	// A lock wait can cross expiry, so read the database clock after acquiring
	// both locks rather than trusting a pre-lock WHERE timestamp.
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&occurred); err != nil {
		return errors.New("tracking visit unavailable")
	}
	if revoked.Valid || (expires.Valid && !expires.Time.After(occurred)) {
		return tracking.ErrNotFound
	}
	if !record {
		if err = tx.Commit(); err != nil {
			return errors.New("tracking visit unavailable")
		}
		return nil
	}
	// Every event and its webhook fanout spend a single bounded app counter.
	// Serialize across links and replicas; do not retain request identities.
	var dailyLimit, used, suppressed int
	var quotaDay time.Time
	err = tx.QueryRowContext(ctx, `SELECT tracking_daily_limit,tracking_quota_day,tracking_quota_used,tracking_quota_suppressed FROM apps WHERE id=$1 FOR UPDATE`, appID).Scan(&dailyLimit, &quotaDay, &used, &suppressed)
	if err != nil {
		return errors.New("tracking budget unavailable")
	}
	// This second lock can also cross the link's expiry or a UTC day boundary.
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&occurred); err != nil {
		return errors.New("tracking visit unavailable")
	}
	if revoked.Valid || (expires.Valid && !expires.Time.After(occurred)) {
		return tracking.ErrNotFound
	}
	day := occurred.UTC().Format("2006-01-02")
	storedDay := quotaDay.Format("2006-01-02")
	if day > storedDay {
		storedDay, used, suppressed = day, 0, 0
	}
	// On a backwards clock change, retain the newer day and its spent budget.
	var activeSubscriptions int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM webhook_subscriptions WHERE app_id=$1 AND active LIMIT $2) bounded`, appID, maxActiveWebhooks+1).Scan(&activeSubscriptions)
	if err != nil {
		return errors.New("tracking budget unavailable")
	}
	if count >= limit || used >= dailyLimit || activeSubscriptions > maxActiveWebhooks {
		if suppressed < 1000000000 {
			suppressed++
		}
		if _, err = tx.ExecContext(ctx, `UPDATE apps SET tracking_quota_day=$2::date,tracking_quota_used=$3,tracking_quota_suppressed=$4 WHERE id=$1`, appID, storedDay, used, suppressed); err != nil {
			return errors.New("tracking budget unavailable")
		}
		if err = tx.Commit(); err != nil {
			return errors.New("tracking visit commit not confirmed")
		}
		return nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE apps SET tracking_quota_day=$2::date,tracking_quota_used=$3,tracking_quota_suppressed=$4 WHERE id=$1`, appID, storedDay, used+1, suppressed); err != nil {
		return errors.New("tracking budget unavailable")
	}
	id, err := newOpaqueID()
	if err != nil {
		return errors.New("tracking visit unavailable")
	}
	outcome := "opened"
	if kind == "click" {
		outcome = "clicked"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tracking_links SET event_count=event_count+1 WHERE id=$1`, linkID); err != nil {
		return errors.New("tracking visit unavailable")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tracking_events(id,link_id,delivery_id,kind,outcome,occurred_at) VALUES ($1,$2,$3,$4,$5,$6)`, id, linkID, deliveryID, kind, outcome, occurred); err != nil {
		return errors.New("tracking visit unavailable")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,outcome,smtp_code,occurred_at,tracking_event_id) VALUES ($1,$2,$3,$4,$5,0,$6,$1)`, id, appID, messageID, deliveryID, outcome, occurred); err != nil {
		return errors.New("tracking visit unavailable")
	}
	if err = enqueueWebhookDeliveries(ctx, tx, id, appID); err != nil {
		return errors.New("tracking visit unavailable")
	}
	if err = tx.Commit(); err != nil {
		return errors.New("tracking visit commit not confirmed")
	}
	return nil
}
