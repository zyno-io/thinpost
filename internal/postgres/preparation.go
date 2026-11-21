package postgres

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/preparation"
)

const maxPreparationAttempts = 12

func (r *Repository) ClaimPreparation(ctx context.Context, token string, ttl time.Duration) (preparation.Claim, error) {
	var claim preparation.Claim
	if !opaqueID(token) || ttl < 5*time.Second || ttl > 5*time.Minute {
		return claim, errors.New("invalid preparation claim")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return claim, err
	}
	defer tx.Rollback()
	// A crashed worker leaves its claim until expiry. Reschedule it under a
	// row lock and return; no second worker can finalize or claim it at once.
	var expiredID, expiredApp, expiredToken string
	var expiredAttempts int
	err = tx.QueryRowContext(ctx, `SELECT id,app_id,prepare_token,prepare_attempts FROM messages WHERE state='preparing' AND prepare_until<=clock_timestamp() ORDER BY prepare_until,id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&expiredID, &expiredApp, &expiredToken, &expiredAttempts)
	if err == nil {
		terminal, failErr := finishPreparationFailure(ctx, tx, expiredID, expiredToken, expiredAttempts, "lease_expired")
		if failErr != nil {
			return claim, failErr
		}
		if err = tx.Commit(); err != nil {
			return claim, err
		}
		if terminal {
			oplog.PreparationFailed(expiredApp, expiredID, "lease_expired")
		}
		return claim, preparation.ErrNoWork
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return claim, err
	}
	err = tx.QueryRowContext(ctx, `UPDATE messages SET state='preparing',prepare_token=$1,prepare_until=clock_timestamp()+$2*interval '1 millisecond',prepare_attempts=prepare_attempts+1
		WHERE id=(SELECT m.id FROM messages m JOIN apps app ON app.id=m.app_id WHERE app.active AND m.state='queued' AND m.prepare_next_at<=clock_timestamp() AND m.prepare_attempts<$3 ORDER BY m.prepare_next_at,m.accepted_at,m.id FOR UPDATE OF m SKIP LOCKED LIMIT 1)
		RETURNING id,app_id,object_key,accepted_at,COALESCE(hold_resolved_at,accepted_at),enhanced_protection,(SELECT a.list_unsubscribe FROM apps a WHERE a.id=messages.app_id),COALESCE(sender_domain,''),
		COALESCE((SELECT t.domain FROM tracking_domains t
			WHERE t.app_id=messages.app_id AND t.state='active' AND t.last_ok_at>clock_timestamp()-`+trackingFreshness+`
			AND (t.sender_domain=messages.sender_domain OR right(messages.sender_domain,length(t.sender_domain)+1)='.'||t.sender_domain)
			AND EXISTS (SELECT 1 FROM sender_domains s WHERE s.app_id=t.app_id AND s.domain=t.sender_domain AND s.active)
			ORDER BY length(t.sender_domain) DESC LIMIT 1),'')`, token, ttl.Milliseconds(), maxPreparationAttempts).Scan(&claim.MessageID, &claim.AppID, &claim.SourceKey, &claim.AcceptedAt, &claim.Date, &claim.EnhancedProtection, &claim.ListUnsubscribe, &claim.SenderDomain, &claim.TrackingDomain)
	if errors.Is(err, sql.ErrNoRows) {
		return claim, preparation.ErrNoWork
	}
	if err != nil {
		return claim, err
	}
	if err = tx.Commit(); err != nil {
		return preparation.Claim{}, err
	}
	claim.Token = token
	return claim, nil
}

// RenewPreparation extends the exact, still-live claim to now+ttl. An expired,
// reassigned or finished claim yields ErrLostLease and is never revived.
func (r *Repository) RenewPreparation(ctx context.Context, claim preparation.Claim, ttl time.Duration) error {
	if !opaqueID(claim.Token) || claim.MessageID == "" || claim.AppID == "" || ttl < 5*time.Second || ttl > 5*time.Minute {
		return errors.New("invalid preparation renewal")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE messages SET prepare_until=clock_timestamp()+$4*interval '1 millisecond' WHERE id=$1 AND app_id=$2 AND state='preparing' AND prepare_token=$3 AND prepare_until>clock_timestamp()`, claim.MessageID, claim.AppID, claim.Token, ttl.Milliseconds())
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return preparation.ErrLostLease
	}
	return tx.Commit()
}

// FailPreparation only changes the still-live, exact claim. A failed or
// ambiguous commit may already have published deliveries, in which case this
// update cannot undo them or queue a second copy. It reports whether the
// failure was terminal (attempts exhausted) rather than requeued for retry.
func (r *Repository) FailPreparation(ctx context.Context, claim preparation.Claim, reason string) (bool, error) {
	if !opaqueID(claim.Token) || claim.MessageID == "" || claim.AppID == "" || claim.SourceKey == "" || !preparationFailureReason(reason) || reason == "lease_expired" {
		return false, errors.New("invalid preparation failure")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT prepare_attempts FROM messages WHERE id=$1 AND app_id=$2 AND object_key=$3 AND state='preparing' AND prepare_token=$4 AND prepare_until>clock_timestamp() FOR UPDATE`, claim.MessageID, claim.AppID, claim.SourceKey, claim.Token).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, preparation.ErrLostLease
	}
	if err != nil {
		return false, err
	}
	terminal, err := finishPreparationFailure(ctx, tx, claim.MessageID, claim.Token, attempts, reason)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return terminal, nil
}

func preparationFailureReason(reason string) bool {
	switch reason {
	case "source_read", "source_invalid", "signing_key", "mime_build", "object_write", "queue_commit", "internal", "lease_expired":
		return true
	default:
		return false
	}
}

func finishPreparationFailure(ctx context.Context, tx *sql.Tx, id, token string, attempts int, reason string) (bool, error) {
	if attempts < 1 || attempts > maxPreparationAttempts || !preparationFailureReason(reason) {
		return false, errors.New("invalid persisted preparation claim")
	}
	state := "queued"
	if attempts == maxPreparationAttempts {
		state = "preparation_failed"
	}
	delay := preparationRetryDelay(attempts, token)
	result, err := tx.ExecContext(ctx, `UPDATE messages SET state=$3,prepare_token=NULL,prepare_until=NULL,prepare_failure=$4,prepare_next_at=clock_timestamp()+$5*interval '1 millisecond' WHERE id=$1 AND state='preparing' AND prepare_token=$2`, id, token, state, reason, delay.Milliseconds())
	if err != nil {
		return false, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return false, preparation.ErrLostLease
	}
	if state != "preparation_failed" {
		return false, nil
	}
	var eventID, appID string
	err = tx.QueryRowContext(ctx, `INSERT INTO webhook_events(id,app_id,message_id,outcome,smtp_code)
		SELECT replace(pg_catalog.gen_random_uuid()::text,'-',''),app_id,id,'preparation_failed',0 FROM messages WHERE id=$1 RETURNING id,app_id`, id).Scan(&eventID, &appID)
	if err != nil {
		return false, err
	}
	return true, enqueueWebhookDeliveries(ctx, tx, eventID, appID)
}

// RetryPreparation is an audited operator action. Only a terminally held
// message is reset; prepared or in-flight messages cannot be requeued.
func (r *Repository) RetryPreparation(ctx context.Context, actor, appID, messageID string) error {
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(messageID) {
		return admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM messages WHERE id=$1 AND app_id=$2 FOR UPDATE`, messageID, appID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != "preparation_failed" {
		return admin.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET state='queued',prepare_attempts=0,prepare_next_at=clock_timestamp(),prepare_failure=NULL,prepare_token=NULL,prepare_until=NULL WHERE id=$1`, messageID); err != nil {
		return err
	}
	if err = recordAudit(ctx, tx, actor, appID, "message.preparation.retry", messageID); err != nil {
		return err
	}
	return tx.Commit()
}

// CommitPreparation makes all recipient queue records visible atomically only
// while the exact claim remains valid. The final objects already exist; stale
// or failed transactions leave them for safe orphan collection.
func (r *Repository) CommitPreparation(ctx context.Context, claim preparation.Claim, deliveries []preparation.Delivery) error {
	domain, ok := admin.NormalizeDomain(claim.SigningDomain)
	if !opaqueID(claim.Token) || !ok || domain != claim.SigningDomain || len(deliveries) == 0 || len(deliveries) > 100 {
		return errors.New("invalid prepared deliveries")
	}
	for i, delivery := range deliveries {
		object, err := objectkey.Parse(delivery.ObjectKey)
		if err != nil || object.Kind != "final" || object.ID != delivery.ID || delivery.RecipientIndex != i {
			return errors.New("invalid prepared delivery")
		}
		if delivery.Tracking != nil {
			object, err = objectkey.Parse(delivery.Tracking.ObjectKey)
			validLifetime := delivery.Tracking.Indefinite && delivery.Tracking.ExpiresAt.IsZero() && delivery.Tracking.PostSendValiditySeconds == 0 || !delivery.Tracking.Indefinite && !delivery.Tracking.ExpiresAt.IsZero() && delivery.Tracking.PostSendValiditySeconds >= 1 && delivery.Tracking.PostSendValiditySeconds <= 365*24*60*60
			if err != nil || object.Kind != "tracking" || object.ID != delivery.ID || !validLifetime || delivery.Tracking.EventLimit < 1 || delivery.Tracking.EventLimit > 1000 || len(delivery.Tracking.Links) == 0 || len(delivery.Tracking.Links) > 101 {
				return errors.New("invalid prepared tracking references")
			}
			seenIDs := make(map[string]bool, len(delivery.Tracking.Links))
			seenHashes := make(map[[32]byte]bool, len(delivery.Tracking.Links))
			var opens, clicks int
			for _, link := range delivery.Tracking.Links {
				if !opaqueID(link.ID) || link.Hash == ([32]byte{}) || seenIDs[link.ID] || seenHashes[link.Hash] {
					return errors.New("invalid prepared tracking references")
				}
				seenIDs[link.ID], seenHashes[link.Hash] = true, true
				switch link.Kind {
				case "open":
					opens++
				case "click":
					clicks++
				default:
					return errors.New("invalid prepared tracking references")
				}
			}
			if opens > 1 || clicks > 100 {
				return errors.New("invalid prepared tracking references")
			}
		}
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keys := make([]string, 0, len(deliveries))
	for _, item := range deliveries {
		keys = append(keys, item.ObjectKey)
		if item.Tracking != nil {
			keys = append(keys, item.Tracking.ObjectKey)
		}
	}
	if err = r.lockAndVerifyObjects(ctx, tx, keys); err != nil {
		return err
	}
	var authorized bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM sender_domains d JOIN apps a ON a.id=d.app_id WHERE d.app_id=$1 AND d.domain=$2 AND d.active AND d.verified_at IS NOT NULL AND a.active FOR SHARE OF d,a`, claim.AppID, claim.SigningDomain).Scan(&authorized)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("sender domain is not authorized")
	}
	if err != nil {
		return err
	}
	// A rotation switch locks the domain row FOR UPDATE, which the share lock
	// above waits for, so this sees the switch's result: a message signed by a
	// key that is no longer active is not committed and is prepared again.
	if claim.SigningKeyID != "" {
		var keyActive bool
		err = tx.QueryRowContext(ctx, `SELECT true FROM dkim_keys WHERE id=$1 AND app_id=$2 AND domain=$3 AND state='active'`, claim.SigningKeyID, claim.AppID, claim.SigningDomain).Scan(&keyActive)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("signing key is no longer active")
		}
		if err != nil {
			return err
		}
	}
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	for _, item := range deliveries {
		if item.Tracking != nil && !item.Tracking.Indefinite && (!item.Tracking.ExpiresAt.After(now.Add(time.Duration(item.Tracking.PostSendValiditySeconds)*time.Second+delivery.TrackingMinimumRemaining)) || item.Tracking.ExpiresAt.After(now.Add(366*24*time.Hour))) {
			return errors.New("invalid tracking expiry")
		}
	}
	var id string
	err = tx.QueryRowContext(ctx, `UPDATE messages SET state='prepared',prepare_token=NULL,prepare_until=NULL,prepare_failure=NULL,shared_message_id=true
		WHERE id=$1 AND app_id=$2 AND object_key=$3 AND enhanced_protection=$4 AND state='preparing' AND prepare_token=$5 AND prepare_until>clock_timestamp() AND sender_domain=$6 RETURNING id`, claim.MessageID, claim.AppID, claim.SourceKey, claim.EnhancedProtection, claim.Token, claim.SigningDomain).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return preparation.ErrLostLease
	}
	if err != nil {
		return err
	}
	for _, delivery := range deliveries {
		if _, err = tx.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,$3,$4)`, delivery.ID, claim.MessageID, delivery.RecipientIndex, delivery.ObjectKey); err != nil {
			return err
		}
		if delivery.Tracking != nil {
			var expires, validity any
			if !delivery.Tracking.Indefinite {
				expires, validity = delivery.Tracking.ExpiresAt, delivery.Tracking.PostSendValiditySeconds
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO tracking_objects(delivery_id,object_key,expires_at,event_limit,post_send_validity_seconds) VALUES ($1,$2,$3,$4,$5)`, delivery.ID, delivery.Tracking.ObjectKey, expires, delivery.Tracking.EventLimit, validity); err != nil {
				return err
			}
			for _, link := range delivery.Tracking.Links {
				if _, err = tx.ExecContext(ctx, `INSERT INTO tracking_links(id,token_hash,delivery_id,kind) VALUES ($1,$2,$3,$4)`, link.ID, link.Hash[:], delivery.ID, link.Kind); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}
func opaqueID(id string) bool {
	if len(id) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16
}
