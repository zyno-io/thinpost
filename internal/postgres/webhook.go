package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/zyno-io/thinpost/internal/webhook"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

const maxWebhookAttempts = 12

// ClaimWebhook records an attempt before returning a target. Expired in-flight
// attempts are recorded as unknown, since the receiver may already have
// accepted them. A stable event ID lets consumers deduplicate retries.
func (r *Repository) ClaimWebhook(ctx context.Context, token string, ttl time.Duration) (webhook.Claim, error) {
	var claim webhook.Claim
	if !opaqueID(token) || ttl < 20*time.Second || ttl > 5*time.Minute {
		return claim, errors.New("invalid webhook claim")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return claim, err
	}
	defer tx.Rollback()
	if err = sweepExhaustedWebhooks(ctx, tx, "event"); err != nil {
		return claim, err
	}
	var previous sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT w.event_id,w.subscription_id,w.app_id,s.endpoint_url,s.secret_ciphertext,w.attempts,w.claim_token,
		e.message_id,COALESCE(e.delivery_id,''),COALESCE(e.attempt_id,''),COALESCE(e.bounce_receipt_id,''),e.outcome,e.smtp_code,e.occurred_at,COALESCE(te.link_id,''),
		s.format,s.auth_ciphertext,COALESCE(e.delivery_state,''),COALESCE(e.suppression_reason,'')
		FROM webhook_deliveries w JOIN webhook_subscriptions s ON s.id=w.subscription_id AND s.app_id=w.app_id
		JOIN apps a ON a.id=w.app_id
		JOIN webhook_events e ON e.id=w.event_id AND e.app_id=w.app_id
		LEFT JOIN tracking_events te ON te.id=e.tracking_event_id AND te.delivery_id=e.delivery_id AND te.outcome=e.outcome
		WHERE w.lane='event' AND s.active AND a.active AND w.attempts<$1 AND ((w.state='queued' AND w.next_attempt_at<=clock_timestamp()) OR (w.state='in_flight' AND w.claim_until<=clock_timestamp()))
		ORDER BY w.next_attempt_at,w.event_id FOR UPDATE OF w SKIP LOCKED LIMIT 1`, maxWebhookAttempts).Scan(&claim.Event.ID, &claim.SubscriptionID, &claim.AppID, &claim.Endpoint, &claim.SealedSecret, &claim.Attempt, &previous, &claim.Event.MessageID, &claim.Event.DeliveryID, &claim.Event.AttemptID, &claim.Event.BounceReceiptID, &claim.Event.Outcome, &claim.Event.SMTPCode, &claim.Event.OccurredAt, &claim.Event.TrackingLinkID, &claim.Format, &claim.SealedAuth, &claim.Event.DeliveryState, &claim.Event.SuppressionReason)
	if errors.Is(err, sql.ErrNoRows) {
		if commitErr := tx.Commit(); commitErr != nil {
			return claim, commitErr
		}
		return claim, webhook.ErrNoWork
	}
	if err != nil {
		return claim, err
	}
	if previous.Valid {
		if err = finishWebhookAttempt(ctx, tx, previous.String, "unknown", nil); err != nil {
			return claim, err
		}
	}
	// A mapping change across a deploy can leave a queued delivery its format
	// no longer represents. Cancel it without an attempt rather than send it.
	fact := format.Fact{Outcome: claim.Event.Outcome, SMTPCode: claim.Event.SMTPCode, DeliveryState: claim.Event.DeliveryState}
	if _, ok := lookupClassify(claim.Format, fact); !ok {
		if _, err = tx.ExecContext(ctx, `UPDATE webhook_deliveries SET state='cancelled',claim_token=NULL,claim_until=NULL WHERE event_id=$1 AND subscription_id=$2`, claim.Event.ID, claim.SubscriptionID); err != nil {
			return webhook.Claim{}, err
		}
		if err = tx.Commit(); err != nil {
			return webhook.Claim{}, err
		}
		return webhook.Claim{}, webhook.ErrSkipped
	}
	if claim.Format == format.Thinpost {
		if claim.Event.MessageIDHeader, err = loadMessageIDHeader(ctx, tx, claim.Event.MessageID, claim.AppID, claim.Event.DeliveryID); err != nil {
			return webhook.Claim{}, err
		}
	} else {
		if claim.Context, err = loadWebhookContext(ctx, tx, claim.Event.ID, claim.AppID); err != nil {
			return webhook.Claim{}, err
		}
	}
	claim.Attempt++
	claim.Token = token
	if _, err = tx.ExecContext(ctx, `UPDATE webhook_deliveries SET state='in_flight',attempts=$3,claim_token=$4,claim_until=clock_timestamp()+$5*interval '1 millisecond' WHERE event_id=$1 AND subscription_id=$2`, claim.Event.ID, claim.SubscriptionID, claim.Attempt, token, ttl.Milliseconds()); err != nil {
		return webhook.Claim{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_attempts(id,event_id,subscription_id) VALUES ($1,$2,$3)`, token, claim.Event.ID, claim.SubscriptionID); err != nil {
		return webhook.Claim{}, err
	}
	if err = tx.Commit(); err != nil {
		return webhook.Claim{}, err
	}
	return claim, nil
}

// sweepExhaustedWebhooks ends, as failed, the lane's in-flight deliveries whose
// claim expired with every attempt used. Bound per claim so exhausted, crashed
// attempts cannot remain indefinitely in flight or monopolize the dispatch
// transaction. The attempt is recorded as unknown, since the receiver may
// already have accepted it.
func sweepExhaustedWebhooks(ctx context.Context, tx *sql.Tx, lane string) error {
	rows, err := tx.QueryContext(ctx, `SELECT event_id,subscription_id,claim_token FROM webhook_deliveries WHERE lane=$2 AND state='in_flight' AND claim_until<=clock_timestamp() AND attempts >= $1 FOR UPDATE SKIP LOCKED LIMIT 16`, maxWebhookAttempts, lane)
	if err != nil {
		return err
	}
	type expired struct{ event, subscription, token string }
	var exhausted []expired
	for rows.Next() {
		var item expired
		if err = rows.Scan(&item.event, &item.subscription, &item.token); err != nil {
			rows.Close()
			return err
		}
		exhausted = append(exhausted, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range exhausted {
		if err = finishWebhookAttempt(ctx, tx, item.token, "unknown", nil); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE webhook_deliveries SET state='failed',claim_token=NULL,claim_until=NULL WHERE event_id=$1 AND subscription_id=$2 AND claim_token=$3`, item.event, item.subscription, item.token); err != nil {
			return err
		}
	}
	return nil
}

// CommitWebhook records an HTTP outcome only for the current unexpired claim.
// Non-2xx and transport failures are retried with bounded exponential backoff.
func (r *Repository) CommitWebhook(ctx context.Context, claim webhook.Claim, status int) error {
	if !opaqueID(claim.Token) || !opaqueID(claim.Event.ID) || !leaseID(claim.SubscriptionID) || !leaseID(claim.AppID) || status < 0 || status > 599 || status > 0 && status < 100 {
		return errors.New("invalid webhook result")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT attempts FROM webhook_deliveries WHERE event_id=$1 AND subscription_id=$2 AND app_id=$3 AND state='in_flight' AND claim_token=$4 AND claim_until>clock_timestamp() FOR UPDATE`, claim.Event.ID, claim.SubscriptionID, claim.AppID, claim.Token).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return webhook.ErrLostClaim
	}
	if err != nil {
		return err
	}
	success := status >= 200 && status < 300
	attemptState := "failed"
	deliveryState := "queued"
	if success {
		attemptState = "succeeded"
		deliveryState = "delivered"
	} else if attempts >= maxWebhookAttempts {
		deliveryState = "failed"
	}
	delay := webhookRetryDelay(attempts)
	if err = finishWebhookAttempt(ctx, tx, claim.Token, attemptState, status); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE webhook_deliveries SET state=$3,claim_token=NULL,claim_until=NULL,next_attempt_at=clock_timestamp()+$4*interval '1 millisecond',delivered_at=CASE WHEN $3='delivered' THEN clock_timestamp() ELSE NULL END WHERE event_id=$1 AND subscription_id=$2`, claim.Event.ID, claim.SubscriptionID, deliveryState, delay.Milliseconds()); err != nil {
		return err
	}
	return tx.Commit()
}

func finishWebhookAttempt(ctx context.Context, tx *sql.Tx, token, state string, status any) error {
	result, err := tx.ExecContext(ctx, `UPDATE webhook_attempts SET state=$2,http_status=$3,finished_at=clock_timestamp() WHERE id=$1 AND state='in_flight'`, token, state, status)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("webhook attempt history is inconsistent")
	}
	return nil
}
func webhookRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		return time.Minute
	}
	delay := time.Minute
	for i := 1; i < attempt && delay < 6*time.Hour; i++ {
		delay *= 2
	}
	if delay > 6*time.Hour {
		return 6 * time.Hour
	}
	return delay
}

func lookupClassify(name string, fact format.Fact) (string, bool) {
	impl, ok := format.Lookup(name)
	if !ok {
		return "", false
	}
	return impl.Classify(fact)
}

// loadWebhookContext reads the read-only message metadata a provider payload
// needs, in the claim's transaction. The subject of an Enhanced Protection
// message is masked in SQL, as in history, so it never reaches Go.
func loadWebhookContext(ctx context.Context, tx *sql.Tx, eventID, appID string) (format.Context, error) {
	var c format.Context
	var recipients []byte
	var subject sql.NullString
	var first sql.NullBool
	err := tx.QueryRowContext(ctx, `SELECT m.accepted_at,m.enhanced_protection,
		CASE WHEN m.enhanced_protection THEN NULL ELSE m.subject END,
		COALESCE(m.sender_domain,''), COALESCE(m.sender_address,''), COALESCE(m.sender_name,''),
		COALESCE((SELECT json_agg(r.address ORDER BY r.recipient_index) FROM message_recipients r
			WHERE r.message_id=m.id AND r.app_id=m.app_id AND ((d.id IS NULL AND r.kind='to') OR r.recipient_index=d.recipient_index)),'[]'::json),
		CASE WHEN e.outcome='opened' THEN NOT EXISTS (
			SELECT 1 FROM tracking_events cur JOIN tracking_events t ON t.link_id=cur.link_id
			WHERE cur.id=e.tracking_event_id AND (t.occurred_at,t.id) < (cur.occurred_at,cur.id)) END
		FROM webhook_events e JOIN messages m ON m.id=e.message_id AND m.app_id=e.app_id
		LEFT JOIN deliveries d ON d.id=e.delivery_id
		WHERE e.id=$1 AND e.app_id=$2`, eventID, appID).Scan(&c.AcceptedAt, &c.EnhancedProtection, &subject, &c.SenderDomain, &c.SenderAddress, &c.SenderName, &recipients, &first)
	if err != nil {
		return format.Context{}, err
	}
	if err = json.Unmarshal(recipients, &c.Recipients); err != nil {
		return format.Context{}, err
	}
	// Defense in depth: the SQL already yields NULL for Enhanced Protection.
	if subject.Valid && !c.EnhancedProtection {
		c.Subject = subject.String
	}
	if first.Valid {
		c.FirstOpen = &first.Bool
	}
	return c, nil
}
