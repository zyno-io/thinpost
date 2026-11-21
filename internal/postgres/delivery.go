package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"time"

	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/journal"
	"github.com/zyno-io/thinpost/internal/oplog"
)

// ClaimDelivery records an attempt before it can be sent to an edge worker.
// Expired or disconnected in-flight attempts are not automatically retried:
// journal reconciliation must first establish what the worker did.
//
// caps is what the connected worker reported it can send from; each claim is
// addressed from it. A delivery whose pinned sending IP the worker did not
// report is never claimed: it stays queued rather than being sent from another
// address. A worker that reported no sources is given no work at all.
func (r *Repository) ClaimDelivery(ctx context.Context, lease ControllerLease, attemptID string, caps delivery.EdgeCapabilities) (delivery.Claim, error) {
	if !leaseID(lease.WorkerID) || !leaseID(lease.ControllerID) || lease.Epoch == 0 || !opaqueID(attemptID) {
		return delivery.Claim{}, errors.New("invalid delivery claim")
	}
	if len(caps.Sources) == 0 {
		return delivery.Claim{}, delivery.ErrNoWork
	}
	// A suppressed delivery is resolved without work for the Edge. Claim again
	// in a fresh transaction, bounded, instead of idling for the poll interval.
	// Other local resolutions (retry expired, tracking unavailable) still yield.
	// A returned claim always carries the caller's attempt ID; a suppressed
	// resolution records its attempt under a fresh ID instead.
	for pass := 0; pass < maxLocalClaimPasses; pass++ {
		claim, local, err := r.claimDeliveryOnce(ctx, lease, attemptID, caps)
		if !local {
			return claim, err
		}
	}
	return delivery.Claim{}, delivery.ErrNoWork
}

// maxLocalClaimPasses bounds the locally resolved deliveries one claim call
// may consume before it yields.
const maxLocalClaimPasses = 32

// claimDeliveryOnce claims at most one delivery. local is true when the
// delivery was suppressed and the caller may claim again.
func (r *Repository) claimDeliveryOnce(ctx context.Context, lease ControllerLease, attemptID string, caps delivery.EdgeCapabilities) (delivery.Claim, bool, error) {
	var claim delivery.Claim
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return claim, false, err
	}
	defer tx.Rollback()
	policy, err := deliveryRetryPolicy(ctx, tx)
	if err != nil {
		return claim, false, err
	}
	exists, err := lockLiveControllerLease(ctx, tx, lease)
	if err != nil {
		return claim, false, err
	}
	if !exists {
		return claim, false, delivery.ErrLeaseInvalid
	}
	var trackingUnavailable bool
	var trackingCutoff sql.NullTime
	var firstAttempt sql.NullTime
	var suppression sql.NullString
	var pinnedAddress sql.NullString
	var now time.Time
	// Every delivery is addressed from the live session's report: the HELO and
	// bounce domain are the reported name of the address it leaves from, and the
	// source is always named explicitly. An app pinned to an IP is claimed only
	// while that address is reported, and waits otherwise; it is never sent from
	// another. An unpinned app uses the main IP (the first reported address). A
	// worker that reported no sources (or no names) is given nothing.
	primary, ok := caps.Primary()
	if !ok {
		return claim, false, delivery.ErrNoWork
	}
	addresses := make([]string, 0, len(caps.Sources))
	for _, source := range caps.Sources {
		addresses = append(addresses, source.Address.String())
	}
	err = tx.QueryRowContext(ctx, `SELECT d.id,d.message_id,m.app_id,m.object_key,d.final_object_key,d.recipient_index,m.enhanced_protection,m.accepted_at,d.route_attempts,m.recipient_policy_override,
		EXISTS (SELECT 1 FROM tracking_objects t WHERE t.delivery_id=d.id AND (t.object_key IS NULL OR t.revoked_at IS NOT NULL OR (t.expires_at IS NOT NULL AND (t.post_send_validity_seconds IS NULL OR t.expires_at-t.post_send_validity_seconds*interval '1 second'<=clock_timestamp()+$1*interval '1 millisecond')))),
		(SELECT expires_at-post_send_validity_seconds*interval '1 second' FROM tracking_objects t WHERE t.delivery_id=d.id),
		(SELECT min(a.started_at) FROM delivery_attempts a WHERE a.delivery_id=d.id AND a.outcome IS DISTINCT FROM 'recipient_policy_denied'),clock_timestamp(),
		(SELECT s.reason FROM message_recipients r JOIN suppressions s ON s.app_id=r.app_id AND s.address_lower=r.address_lower
			WHERE r.message_id=d.message_id AND r.recipient_index=d.recipient_index),
		host(ip.address)
		FROM deliveries d JOIN messages m ON m.id=d.message_id JOIN apps app ON app.id=m.app_id
		LEFT JOIN sending_ips ip ON ip.id=app.sending_ip_id
		WHERE app.active AND d.state='queued' AND d.retry_after<=clock_timestamp()
		AND (app.sending_ip_id IS NULL OR host(ip.address)=ANY($2::text[]))
		ORDER BY d.retry_after,d.id FOR UPDATE OF d SKIP LOCKED LIMIT 1`, delivery.TrackingMinimumRemaining.Milliseconds(), addresses).Scan(&claim.DeliveryID, &claim.MessageID, &claim.AppID, &claim.SourceKey, &claim.FinalKey, &claim.RecipientIndex, &claim.EnhancedProtection, &claim.AcceptedAt, &claim.RouteAttempt, &claim.RecipientPolicyOverride, &trackingUnavailable, &trackingCutoff, &firstAttempt, &now, &suppression, &pinnedAddress)
	if errors.Is(err, sql.ErrNoRows) {
		return claim, false, delivery.ErrNoWork
	}
	if err != nil {
		return claim, false, err
	}
	exists, err = lockLiveControllerLease(ctx, tx, lease)
	if err != nil {
		return claim, false, err
	}
	if !exists {
		return claim, false, delivery.ErrLeaseInvalid
	}
	if suppression.Valid {
		// The caller's ID is kept for a claim that will be returned.
		if attemptID, err = newOpaqueID(); err != nil {
			return claim, false, err
		}
	}
	// The address recorded is the one the worker is asked to send from: the
	// app's pinned IP, else the main IP. It is never NULL for a new attempt.
	source := primary
	if pinnedAddress.Valid {
		var ok bool
		if source, ok = caps.Source(pinnedAddress.String); !ok {
			return claim, false, errors.New("claimed sending IP is not reported by the worker")
		}
		claim.ResolvedAddress = pinnedAddress.String
	}
	sourceAddress := source.Address.String()
	claim.SourceAddress, claim.HeloName, claim.BounceDomain, claim.SourceName = sourceAddress, source.Hostname, source.Hostname, source.Hostname
	if _, err = tx.ExecContext(ctx, `INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized,source_address) VALUES ($1,$2,$3,$4,false,$5::text::inet)`, attemptID, claim.DeliveryID, lease.WorkerID, lease.Epoch, sourceAddress); err != nil {
		return claim, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='in_flight',current_attempt_id=$2 WHERE id=$1`, claim.DeliveryID, attemptID); err != nil {
		return claim, false, err
	}
	// local commits a locally decided result in the claim transaction.
	local := func(result delivery.Result, status oplog.Status, again bool) (delivery.Claim, bool, error) {
		if err := commitDeliveryResultTx(ctx, tx, result, false, policy); err != nil {
			return delivery.Claim{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return delivery.Claim{}, false, err
		}
		oplog.Delivery(claim.AppID, claim.MessageID, claim.DeliveryID, status, 0, claim.SourceName)
		return delivery.Claim{}, again, delivery.ErrNoWork
	}
	base := delivery.Result{AttemptID: attemptID, DeliveryID: claim.DeliveryID, WorkerID: lease.WorkerID, Epoch: lease.Epoch}
	if suppression.Valid {
		base.Outcome, base.SuppressionReason = delivery.OutcomeSuppressed, suppression.String
		return local(base, oplog.OutcomeSuppressed, true)
	}
	if firstAttempt.Valid && !now.Before(firstAttempt.Time) && now.Sub(firstAttempt.Time) >= policy.MaxAge {
		base.Outcome = delivery.OutcomeRetryExpired
		return local(base, oplog.OutcomeRetryExpired, false)
	}
	if trackingUnavailable {
		base.Outcome = delivery.OutcomeTrackingUnavailable
		return local(base, oplog.OutcomeTrackingUnavailable, false)
	}
	if err = tx.Commit(); err != nil {
		return delivery.Claim{}, false, err
	}
	claim.AttemptID, claim.WorkerID, claim.Epoch = attemptID, lease.WorkerID, lease.Epoch
	if trackingCutoff.Valid {
		claim.TrackingSendCutoff = trackingCutoff.Time
	}
	return claim, false, nil
}

// CommitDeliveryResult is idempotent for the exact result and accepts results
// from older controller epochs. The caller must authenticate the worker ID.
// Only after this commits may the controller acknowledge the edge journal.
func (r *Repository) CommitDeliveryResult(ctx context.Context, result delivery.Result) error {
	_, err := r.CommitDeliveryResultIdentified(ctx, result)
	return err
}

// CommitDeliveryResultIdentified behaves like CommitDeliveryResult and also
// returns the owning app and message IDs, read in the same transaction.
func (r *Repository) CommitDeliveryResultIdentified(ctx context.Context, result delivery.Result) (delivery.Committed, error) {
	var committed delivery.Committed
	if !opaqueID(result.AttemptID) || !opaqueID(result.DeliveryID) || !leaseID(result.WorkerID) || (result.JournalID != "" && !journal.ValidIdentity(result.JournalID)) || result.Epoch == 0 || result.Outcome == delivery.OutcomeRetryExpired || result.Outcome == delivery.OutcomeSuppressed || result.SuppressionReason != "" || !validOutcome(result.Outcome, result.SMTPCode) {
		return committed, errors.New("invalid delivery result")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return committed, err
	}
	defer tx.Rollback()
	policy, err := deliveryRetryPolicy(ctx, tx)
	if err != nil {
		return committed, err
	}
	if err = applyDeliveryResultTx(ctx, tx, result, false, policy); errors.Is(err, errResultAlreadyCompleted) {
		committed.Replayed = true
	} else if err != nil {
		return committed, err
	}
	// The attempt is logged under the HELO name of its address: "primary" only
	// for an attempt from before schema 45 (source_address NULL), and "removed"
	// for an address whose row is gone. Never the address itself.
	if err = tx.QueryRowContext(ctx, `SELECT m.app_id,m.id,CASE WHEN a.source_address IS NULL THEN 'primary' ELSE COALESCE(ip.helo_name,'removed') END
		FROM deliveries d JOIN messages m ON m.id=d.message_id
		LEFT JOIN delivery_attempts a ON a.id=$2 AND a.delivery_id=d.id
		LEFT JOIN sending_ips ip ON ip.address=a.source_address
		WHERE d.id=$1`, result.DeliveryID, result.AttemptID).Scan(&committed.AppID, &committed.MessageID, &committed.SourceName); err != nil {
		return delivery.Committed{}, err
	}
	if err = tx.Commit(); err != nil {
		return delivery.Committed{}, err
	}
	return committed, nil
}

// errResultAlreadyCompleted reports an exact replay that changed nothing.
var errResultAlreadyCompleted = errors.New("delivery result already completed")

func commitDeliveryResultTx(ctx context.Context, tx *sql.Tx, result delivery.Result, reconciliation bool, policy delivery.RetryPolicy) error {
	if err := applyDeliveryResultTx(ctx, tx, result, reconciliation, policy); err != nil && !errors.Is(err, errResultAlreadyCompleted) {
		return err
	}
	return nil
}

// applyDeliveryResultTx applies the same result and webhook transition for
// journal replay and conservative reconciliation under an existing tx.
func applyDeliveryResultTx(ctx context.Context, tx *sql.Tx, result delivery.Result, reconciliation bool, policy delivery.RetryPolicy) error {
	if reconciliation && (result.Outcome != "uncertain" || result.SMTPCode != 0) {
		return errors.New("invalid reconciliation result")
	}
	var state string
	var startAuthorized bool
	var outcome, startJournal sql.NullString
	var code sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT state,outcome,smtp_code,start_authorized,start_journal_id FROM delivery_attempts WHERE id=$1 AND delivery_id=$2 AND worker_id=$3 AND epoch=$4 FOR UPDATE`, result.AttemptID, result.DeliveryID, result.WorkerID, result.Epoch).Scan(&state, &outcome, &code, &startAuthorized, &startJournal)
	if errors.Is(err, sql.ErrNoRows) {
		return delivery.ErrResultConflict
	}
	if err != nil {
		return err
	}
	localPermanent := result.Outcome == delivery.OutcomeNullMX || result.Outcome == delivery.OutcomeDomainNotFound
	if localPermanent && (startAuthorized || result.JournalID != "") {
		return delivery.ErrResultConflict
	}
	policyDenied := result.Outcome == delivery.OutcomeRecipientPolicyDenied
	trackingUnavailable := result.Outcome == delivery.OutcomeTrackingUnavailable
	retryExpired := result.Outcome == delivery.OutcomeRetryExpired
	localSuppressed := result.Outcome == delivery.OutcomeSuppressed
	if localSuppressed && (startAuthorized || result.JournalID != "" || !validSuppressionReason(result.SuppressionReason)) {
		return delivery.ErrResultConflict
	}
	if trackingUnavailable && (startAuthorized || result.JournalID != "") {
		return delivery.ErrResultConflict
	}
	if retryExpired && (startAuthorized || result.JournalID != "") {
		return delivery.ErrResultConflict
	}
	if policyDenied && startAuthorized {
		return delivery.ErrResultConflict
	}
	// A journal may acknowledge a permitted SMTP result only for the same
	// journal that received the durable permit. Legacy attempts with unknown
	// identity are held for reconciliation rather than assigned a new source.
	if !reconciliation && startAuthorized && (!startJournal.Valid || startJournal.String != result.JournalID) {
		return delivery.ErrResultConflict
	}
	if state == "completed" {
		if !outcome.Valid || !code.Valid || outcome.String != result.Outcome || int(code.Int64) != result.SMTPCode {
			return delivery.ErrResultConflict
		}
		return errResultAlreadyCompleted
	}
	if state != "in_flight" {
		return delivery.ErrResultConflict
	}
	// A recovered journal can be uncertain even if the worker crashed before
	// receiving a permit. Holding that result is safe; accepting an SMTP
	// response without authorization is not.
	if !startAuthorized && !((result.Outcome == "rejected" || result.Outcome == "uncertain" || localPermanent || policyDenied || trackingUnavailable || retryExpired || localSuppressed) && result.SMTPCode == 0) {
		return delivery.ErrResultConflict
	}
	newState := "uncertain"
	var retryDelay time.Duration
	if policyDenied {
		newState = "recipient_policy_held"
	} else if trackingUnavailable {
		newState = "tracking_held"
	} else if localPermanent || retryExpired || localSuppressed {
		newState = "failed"
	} else if result.Outcome == "accepted" {
		newState = "accepted"
	} else if result.Outcome == "rejected" || result.Outcome == "route_rejected" {
		if result.Outcome == "rejected" && result.SMTPCode >= 500 {
			newState = "failed"
		} else {
			var attempts int
			var first, now time.Time
			if err = tx.QueryRowContext(ctx, `SELECT count(*),min(started_at),clock_timestamp() FROM delivery_attempts WHERE delivery_id=$1 AND outcome IS DISTINCT FROM 'recipient_policy_denied'`, result.DeliveryID).Scan(&attempts, &first, &now); err != nil {
				return err
			}
			elapsed := now.Sub(first)
			if elapsed < 0 {
				elapsed = 0
			}
			if attempts >= policy.MaxAttempts || elapsed >= policy.MaxAge {
				newState = "failed"
			} else {
				newState = "queued"
				retryDelay = smtpRetryDelay(attempts, result.AttemptID, policy)
				if remaining := policy.MaxAge - elapsed; retryDelay > remaining {
					retryDelay = remaining
				}
			}
		}
	}
	advanceRoute := (result.Outcome == "rejected" || result.Outcome == "route_rejected") && startAuthorized
	update, err := tx.ExecContext(ctx, `UPDATE deliveries SET state=$3,current_attempt_id=NULL,retry_after=CASE WHEN $3='queued' THEN clock_timestamp()+$4*interval '1 millisecond' ELSE retry_after END,route_attempts=route_attempts+CASE WHEN $5 THEN 1 ELSE 0 END WHERE id=$1 AND current_attempt_id=$2 AND state='in_flight'`, result.DeliveryID, result.AttemptID, newState, retryDelay.Milliseconds(), advanceRoute)
	if err != nil {
		return err
	}
	n, err := update.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return delivery.ErrAttemptSuperseded
	}
	if newState == "accepted" || newState == "failed" {
		if err = clearPendingIfComplete(ctx, tx, result.DeliveryID); err != nil {
			return err
		}
	}
	// The stage and enhanced code are recorded only with an SMTP reply, and
	// only values that mirror the column CHECKs are written.
	var smtpStage, smtpEnhanced any
	if result.SMTPCode >= 400 && result.SMTPCode <= 599 {
		if result.SMTPStage == "MAIL" || result.SMTPStage == "RCPT" || result.SMTPStage == "DATA" {
			smtpStage = result.SMTPStage
		}
		if journal.ValidEnhanced(result.SMTPEnhanced) {
			smtpEnhanced = result.SMTPEnhanced
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE delivery_attempts SET state='completed',outcome=$2,smtp_code=$3,smtp_stage=$4,smtp_enhanced=$5,finished_at=clock_timestamp() WHERE id=$1`, result.AttemptID, result.Outcome, result.SMTPCode, smtpStage, smtpEnhanced); err != nil {
		return err
	}
	if hardBounceSuppression(result) {
		if err = addHardBounceSuppression(ctx, tx, result.DeliveryID); err != nil {
			return err
		}
	}
	var appID string
	var suppressionReason any
	if localSuppressed {
		suppressionReason = result.SuppressionReason
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,attempt_id,outcome,smtp_code,delivery_state,suppression_reason)
		SELECT $1,m.app_id,m.id,d.id,$1,$3,$4,$5,$6 FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE d.id=$2 RETURNING app_id`, result.AttemptID, result.DeliveryID, result.Outcome, result.SMTPCode, newState, suppressionReason).Scan(&appID)
	if err != nil {
		return err
	}
	if err = enqueueWebhookDeliveries(ctx, tx, result.AttemptID, appID); err != nil {
		return err
	}
	return nil
}

// hardBounceSuppression reports whether a result is a permanent rejection of
// the recipient itself: an RCPT TO reply of 5xx whose enhanced status is 5.1.1
// (bad destination mailbox) or 5.1.10 (recipient not found). A rejection at
// MAIL FROM or DATA describes the sender, IP or content and must never
// suppress a recipient; neither does a missing or other enhanced code. Only
// an SMTP-permitted attempt can carry a code, so this cannot fire for the
// local null_mx/domain_not_found/policy outcomes or bounce receipts.
func hardBounceSuppression(result delivery.Result) bool {
	return result.Outcome == "rejected" && result.SMTPStage == "RCPT" && result.SMTPCode >= 500 && result.SMTPCode <= 599 &&
		(result.SMTPEnhanced == "5.1.1" || result.SMTPEnhanced == "5.1.10")
}

// addHardBounceSuppression records the delivery's recipient as a recipient-
// originated hard bounce, in the result transaction. A recipient-originated
// entry outranks a manual one, which it replaces; every other existing entry
// (hard_bounce, spam_complaint, unsubscribe) wins. ON CONFLICT keeps a unique
// violation's DETAIL, which would quote the address, from ever firing, and no
// error here carries the database's text.
func addHardBounceSuppression(ctx context.Context, tx *sql.Tx, deliveryID string) error {
	id, err := newOpaqueID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO suppressions(id,app_id,address,address_lower,domain_lower,reason,origin,source_message_id)
		SELECT $1,m.app_id,r.address,r.address_lower,r.domain_lower,'hard_bounce','recipient',m.id
		FROM deliveries d JOIN messages m ON m.id=d.message_id
		JOIN message_recipients r ON r.message_id=d.message_id AND r.recipient_index=d.recipient_index
		WHERE d.id=$2 AND m.id ~ '^[0-9a-f]{32}$'
		ON CONFLICT (app_id,address_lower) DO UPDATE
		SET reason=EXCLUDED.reason,origin=EXCLUDED.origin,source_message_id=EXCLUDED.source_message_id
		WHERE suppressions.reason='manual'`, id, deliveryID)
	if err != nil {
		// Keep cancellation visible to the session; hide the driver text.
		failed := errors.New("record hard bounce suppression failed")
		for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
			if errors.Is(err, cause) {
				return errors.Join(failed, cause)
			}
		}
		return failed
	}
	return nil
}

// Serialize sibling completions on the parent. The second statement receives
// a fresh READ COMMITTED snapshot after acquiring the parent lock.
func clearPendingIfComplete(ctx context.Context, tx *sql.Tx, deliveryID string) error {
	var messageID string
	if err := tx.QueryRowContext(ctx, `SELECT m.id FROM messages m JOIN deliveries d ON d.message_id=m.id WHERE d.id=$1 FOR UPDATE OF m`, deliveryID).Scan(&messageID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE messages m SET pending=false WHERE m.id=$1 AND m.pending AND m.state='prepared'
		AND EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id=m.id)
		AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id=m.id AND d.state NOT IN ('accepted','failed'))`, messageID)
	return err
}

// smtpRetryDelay bounds load with deterministic, per-attempt jitter. The
// result is stable for retries of the same committed outcome and policy.
func smtpRetryDelay(attempt int, id string, policy delivery.RetryPolicy) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := policy.InitialDelay
	for i := 1; i < attempt && base < policy.MaxDelay; i++ {
		if base > policy.MaxDelay/2 {
			base = policy.MaxDelay
		} else {
			base *= 2
		}
	}
	sum := sha256.Sum256([]byte(id))
	percent := 100 + binary.BigEndian.Uint32(sum[:4])%21
	delay := base * time.Duration(percent) / 100
	if delay > policy.MaxDelay {
		return policy.MaxDelay
	}
	return delay
}

// preparationRetryDelay preserves the independently bounded preparation retry
// schedule. It does not represent attempts to contact a remote SMTP server.
func preparationRetryDelay(attempt int, id string) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 10 {
		attempt = 10
	}
	base := time.Minute * time.Duration(1<<uint(attempt-1))
	if base > 6*time.Hour {
		base = 6 * time.Hour
	}
	sum := sha256.Sum256([]byte(id))
	percent := 80 + binary.BigEndian.Uint32(sum[:4])%41
	delay := base * time.Duration(percent) / 100
	if delay > 6*time.Hour {
		return 6 * time.Hour
	}
	return delay
}
func validOutcome(outcome string, code int) bool {
	return outcome == "accepted" && code == 250 || outcome == "rejected" && (code == 0 || code >= 400 && code <= 599) || outcome == "route_rejected" && code >= 400 && code <= 599 || (outcome == "uncertain" || outcome == delivery.OutcomeNullMX || outcome == delivery.OutcomeDomainNotFound || outcome == delivery.OutcomeRecipientPolicyDenied || outcome == delivery.OutcomeTrackingUnavailable || outcome == delivery.OutcomeRetryExpired || outcome == delivery.OutcomeSuppressed) && code == 0
}
