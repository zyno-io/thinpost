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

// ClaimWebhookInbound is ClaimWebhook for the inbound lane: the same attempt
// accounting and at-least-once rules, over lane='inbound' deliveries only, with
// the recorded message loaded in the claim's transaction. It holds no content;
// the dispatcher reads the stored message itself. The outbound claim never sees
// these rows, and this one never sees outbound events.
func (r *Repository) ClaimWebhookInbound(ctx context.Context, token string, ttl time.Duration) (webhook.Claim, error) {
	var claim webhook.Claim
	if !opaqueID(token) || ttl < 20*time.Second || ttl > 5*time.Minute {
		return claim, errors.New("invalid webhook claim")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return claim, err
	}
	defer tx.Rollback()
	if err = sweepExhaustedWebhooks(ctx, tx, "inbound"); err != nil {
		return claim, err
	}
	var previous sql.NullString
	var inboundID string
	var recipientIndex int
	err = tx.QueryRowContext(ctx, `SELECT w.event_id,w.subscription_id,w.app_id,s.endpoint_url,s.secret_ciphertext,w.attempts,w.claim_token,
		e.outcome,e.smtp_code,e.occurred_at,s.format,s.auth_ciphertext,e.inbound_id,e.inbound_recipient_index
		FROM webhook_deliveries w JOIN webhook_subscriptions s ON s.id=w.subscription_id AND s.app_id=w.app_id
		JOIN apps a ON a.id=w.app_id
		JOIN webhook_events e ON e.id=w.event_id AND e.app_id=w.app_id AND e.inbound_id IS NOT NULL
		WHERE w.lane='inbound' AND s.active AND a.active AND w.attempts<$1 AND ((w.state='queued' AND w.next_attempt_at<=clock_timestamp()) OR (w.state='in_flight' AND w.claim_until<=clock_timestamp()))
		ORDER BY w.next_attempt_at,w.event_id FOR UPDATE OF w SKIP LOCKED LIMIT 1`, maxWebhookAttempts).Scan(&claim.Event.ID, &claim.SubscriptionID, &claim.AppID, &claim.Endpoint, &claim.SealedSecret, &claim.Attempt, &previous,
		&claim.Event.Outcome, &claim.Event.SMTPCode, &claim.Event.OccurredAt, &claim.Format, &claim.SealedAuth, &inboundID, &recipientIndex)
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
	// A format that no longer represents the event (or a non-inbound endpoint
	// on this lane) is cancelled without an attempt rather than sent.
	if _, ok := lookupClassify(claim.Format, format.Fact{Outcome: claim.Event.Outcome}); !ok || !format.InboundFormat(claim.Format) {
		if _, err = tx.ExecContext(ctx, `UPDATE webhook_deliveries SET state='cancelled',claim_token=NULL,claim_until=NULL WHERE event_id=$1 AND subscription_id=$2`, claim.Event.ID, claim.SubscriptionID); err != nil {
			return webhook.Claim{}, err
		}
		if err = tx.Commit(); err != nil {
			return webhook.Claim{}, err
		}
		return webhook.Claim{}, webhook.ErrSkipped
	}
	data := &webhook.InboundData{InboundID: inboundID}
	var recipients []byte
	var authentication string
	var fromMatches sql.NullBool
	err = tx.QueryRowContext(ctx, `SELECT r.address,im.received_at,im.object_key,im.size_bytes,im.content_expires_at<=clock_timestamp(),
		COALESCE(im.mail_from,''),COALESCE(host(im.client_ip),''),COALESCE(im.helo,''),COALESCE(im.tls_version,''),COALESCE(im.tls_cipher,''),COALESCE(im.receiver,''),
		COALESCE(im.authentication::text,''),im.spf_result,im.dkim_result,im.dmarc_result,
		COALESCE(im.correlated_message_id,''),COALESCE(im.correlated_delivery_id,''),COALESCE(im.correlation_header,''),im.from_matches_recipient,
		COALESCE((SELECT json_agg(x.address ORDER BY x.recipient_index) FROM inbound_recipients x WHERE x.inbound_id=im.id AND x.app_id=im.app_id),'[]'::json)
		FROM inbound_recipients r JOIN inbound_messages im ON im.id=r.inbound_id AND im.app_id=r.app_id
		WHERE r.inbound_id=$1 AND r.recipient_index=$2 AND r.app_id=$3`, inboundID, recipientIndex, claim.AppID).Scan(
		&data.Recipient, &data.ReceivedAt, &data.ObjectKey, &data.Size, &data.Expired,
		&data.MailFrom, &data.ClientIP, &data.Helo, &data.TLSVersion, &data.TLSCipher, &data.Receiver,
		&authentication, &data.SPF, &data.DKIM, &data.DMARC,
		&data.MessageID, &data.DeliveryID, &data.Header, &fromMatches, &recipients)
	if err != nil {
		return webhook.Claim{}, err
	}
	if err = json.Unmarshal(recipients, &data.Recipients); err != nil {
		return webhook.Claim{}, err
	}
	if authentication != "" {
		data.Authentication = []byte(authentication)
	}
	if fromMatches.Valid {
		data.FromMatches = &fromMatches.Bool
	}
	claim.Inbound = data
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

// FailWebhook ends a claimed delivery permanently with a fixed failure class,
// for a delivery that retrying cannot fix (the message content is gone or past
// retention). Like CommitWebhook it acts only on the current unexpired claim.
// The attempt records the class as its state and no HTTP status; the delivery
// becomes failed, and is not retried (replay refuses expired content).
func (r *Repository) FailWebhook(ctx context.Context, claim webhook.Claim, class string) error {
	if !opaqueID(claim.Token) || !opaqueID(claim.Event.ID) || !leaseID(claim.SubscriptionID) || !leaseID(claim.AppID) || class != webhook.ClassContentUnavailable {
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
	if err = finishWebhookAttempt(ctx, tx, claim.Token, class, nil); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE webhook_deliveries SET state='failed',claim_token=NULL,claim_until=NULL,delivered_at=NULL WHERE event_id=$1 AND subscription_id=$2`, claim.Event.ID, claim.SubscriptionID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeferWebhook returns a claimed delivery to the queue, due after delay, without
// using an attempt: the render budget was busy and nothing was sent. Like
// CommitWebhook it acts only on the current unexpired claim. The attempt row is
// kept as state deferred with no HTTP status.
func (r *Repository) DeferWebhook(ctx context.Context, claim webhook.Claim, delay time.Duration) error {
	if !opaqueID(claim.Token) || !opaqueID(claim.Event.ID) || !leaseID(claim.SubscriptionID) || !leaseID(claim.AppID) || delay < time.Second || delay > time.Hour {
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
	if err = finishWebhookAttempt(ctx, tx, claim.Token, "deferred", nil); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE webhook_deliveries SET state='queued',attempts=GREATEST(attempts-1,0),claim_token=NULL,claim_until=NULL,next_attempt_at=clock_timestamp()+$3*interval '1 millisecond' WHERE event_id=$1 AND subscription_id=$2`, claim.Event.ID, claim.SubscriptionID, delay.Milliseconds()); err != nil {
		return err
	}
	return tx.Commit()
}

var _ webhook.InboundRepository = (*Repository)(nil)
