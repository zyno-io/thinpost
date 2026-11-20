// Package postgres implements durable submission and delivery transactions.
// The complete mail service and its deployment are not yet wired.
package postgres

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"embed"
	"errors"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/submission"
)

// Schema is exported for integration testing and migration authoring. It is not
// applied automatically: production must use versioned migrations.
//
//go:embed schema.sql migrations/*.sql
var Schema embed.FS

// ErrSMTPRecordInvalid reports an SMTP commit record that breaks a server-side
// invariant (a key without a TTL, a malformed original From). It is a bug in
// the caller, not a bad message, so intake answers it as a temporary failure.
var ErrSMTPRecordInvalid = errors.New("invalid SMTP commit record")

type Repository struct {
	DB *sql.DB
	// Generation comes from deployment configuration, independently of SQL.
	Generation string

	// Objects verifies immutable object presence while a transaction holds the
	// same advisory lock used by a future orphan collector.
	Objects ObjectVerifier

	// WebhookDestinations is the operator's internal webhook exception list;
	// nil accepts only public HTTPS endpoints.
	WebhookDestinations *admin.WebhookDestinations
}

func (r *Repository) LookupKey(ctx context.Context, digest [32]byte) (submission.App, error) {
	var app submission.App
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return app, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT a.id,k.id,a.list_unsubscribe,a.default_track_opens,a.default_track_clicks FROM api_keys k JOIN apps a ON a.id = k.app_id WHERE k.digest = $1 AND k.active AND a.active`, digest[:]).Scan(&app.ID, &app.KeyID, &app.ListUnsubscribe, &app.DefaultTrackOpens, &app.DefaultTrackClicks)
	if errors.Is(err, sql.ErrNoRows) {
		return submission.App{}, submission.ErrInvalidKey
	}
	if err != nil {
		return app, err
	}
	if err = tx.Commit(); err != nil {
		return submission.App{}, err
	}
	return app, nil
}

// LookupOperatorApp reads an app for an operator submission. It is the
// operator counterpart of LookupKey: the principal is an authenticated
// administrator, so there is no key, and an inactive app is reported as paused
// rather than as an invalid credential.
func (r *Repository) LookupOperatorApp(ctx context.Context, appID string) (submission.App, error) {
	app := submission.App{ID: appID}
	if !opaqueID(appID) {
		return submission.App{}, admin.ErrNotFound
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return submission.App{}, err
	}
	defer tx.Rollback()
	var active bool
	err = tx.QueryRowContext(ctx, `SELECT active,list_unsubscribe,default_track_opens,default_track_clicks FROM apps WHERE id=$1`, appID).Scan(&active, &app.ListUnsubscribe, &app.DefaultTrackOpens, &app.DefaultTrackClicks)
	if errors.Is(err, sql.ErrNoRows) {
		return submission.App{}, admin.ErrNotFound
	}
	if err != nil {
		return submission.App{}, err
	}
	if !active {
		return submission.App{}, submission.ErrAppPaused
	}
	if err = tx.Commit(); err != nil {
		return submission.App{}, err
	}
	return app, nil
}

func (r *Repository) Commit(ctx context.Context, record acceptance.Record) (submission.Receipt, error) {
	var receipt submission.Receipt
	if record.RecipientCount < 1 || record.RecipientCount > 100 || len(record.PreviousHashes) > 7 {
		return receipt, submission.ErrInvalidMessage
	}
	address, err := mail.ParseAddress(record.From)
	if err != nil {
		return receipt, submission.ErrInvalidMessage
	}
	at := strings.LastIndexByte(address.Address, '@')
	if at < 1 || at == len(address.Address)-1 {
		return receipt, submission.ErrInvalidMessage
	}
	domain := strings.ToLower(address.Address[at+1:])
	// Validate every recorded value before any INSERT so a CHECK violation,
	// whose diagnostics quote the row, cannot occur. Values are never logged.
	smtp := record.SMTPCredentialID != ""
	if !(validRecordedSubject(record.Subject) || smtp && validRecordedSMTPSubject(record.Subject)) || !validRecordedDomain(record.SenderDomain) || record.SenderDomain != domain || !validRecordedSender(record.SenderAddress, record.SenderName, domain) {
		return receipt, submission.ErrInvalidMessage
	}
	var lease ControllerLease
	if smtp {
		// The fence names the control session lease the commit is valid under;
		// without a usable one nothing can be fenced, so nothing is committed.
		lease = ControllerLease{WorkerID: record.Fence.WorkerID, ControllerID: record.Fence.ControllerID, Epoch: record.Fence.Epoch}
		// A fence that cannot name a lease is a server-side fault, answered as a
		// lost lease (a temporary failure), never as a bad message.
		if !leaseID(lease.WorkerID) || !leaseID(lease.ControllerID) || lease.Epoch == 0 {
			return receipt, delivery.ErrLeaseInvalid
		}
		if len(record.OriginalFromAddress) > maxRecordedAddressBytes || !utf8.ValidString(record.OriginalFromAddress) || strings.ContainsAny(record.OriginalFromAddress, "\x00\r\n") ||
			record.OriginalFromAddress != "" && len(record.OriginalFromAddress) < 3 ||
			(record.IdempotencyKey == "") != (record.IdempotencyTTL == 0) || record.IdempotencyTTL < 0 || record.IdempotencyTTL > 24*time.Hour || len(record.IdempotencyKey) > 256 {
			return receipt, ErrSMTPRecordInvalid
		}
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	var pendingLimit int
	err = tx.QueryRowContext(ctx, `SELECT pending_message_limit FROM apps WHERE id=$1 AND active FOR UPDATE`, record.AppID).Scan(&pendingLimit)
	if errors.Is(err, sql.ErrNoRows) {
		// An operator or SMTP submission has no key whose revocation explains
		// this: the app was paused after authorization, which is reported as such.
		if (record.Operator != "" || smtp) && record.KeyID == "" {
			var exists bool
			if tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1`, record.AppID).Scan(&exists) == nil {
				return receipt, submission.ErrAppPaused
			}
		}
		return receipt, submission.ErrInvalidMessage
	}
	if err != nil {
		return receipt, err
	}
	var exists bool
	// Exactly one principal. A key submission rechecks its key; an operator
	// submission has no key and is bound to the console adapter and to no
	// idempotency key; an SMTP submission rechecks its credential's digest and
	// is bound to the SMTP adapter, the raw path and no override. Any other
	// combination is refused, so a stray operator or credential value can never
	// open a keyless path for a provider adapter.
	switch {
	case smtp && record.KeyID == "" && record.Operator == "":
		// Overrides are an operator privilege, and only the SMTP principal may
		// use the SMTP adapter and the raw path.
		if record.Adapter != submission.AdapterSMTP || !record.Raw || record.LimitsOverride || record.RecipientPolicyOverride {
			return receipt, submission.ErrInvalidKey
		}
		var stored []byte
		err = tx.QueryRowContext(ctx, `SELECT digest FROM smtp_credentials WHERE id=$1 AND app_id=$2 AND revoked_at IS NULL FOR SHARE`, record.SMTPCredentialID, record.AppID).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return receipt, submission.ErrInvalidKey
		}
		if err != nil {
			return receipt, err
		}
		if !hmac.Equal(stored, record.SMTPCredentialDigest[:]) {
			return receipt, submission.ErrInvalidKey
		}
	case record.KeyID != "" && record.Operator == "" && !smtp:
		// Overrides are an operator privilege: a key principal never carries one.
		// The SMTP adapter and the raw path belong to the SMTP principal alone.
		if record.Adapter == submission.AdapterConsole || record.Adapter == submission.AdapterSMTP || record.Raw || record.IdempotencyTTL != 0 || record.Fence != (submission.Fence{}) || record.LimitsOverride || record.RecipientPolicyOverride {
			return receipt, submission.ErrInvalidKey
		}
		err = tx.QueryRowContext(ctx, `SELECT true FROM api_keys WHERE id=$1 AND app_id=$2 AND active FOR SHARE`, record.KeyID, record.AppID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return receipt, submission.ErrInvalidKey
		}
		if err != nil {
			return receipt, err
		}
	case record.KeyID == "" && !smtp && !record.Raw && record.IdempotencyTTL == 0 && record.Fence == (submission.Fence{}) && adminText(record.Operator, 256) && record.Adapter == submission.AdapterConsole && record.IdempotencyKey == "":
	default:
		return receipt, submission.ErrInvalidKey
	}
	for _, senderDomain := range []string{domain, record.SendingDomain} {
		if senderDomain == "" {
			continue
		}
		err = tx.QueryRowContext(ctx, `SELECT true FROM sender_domains WHERE app_id=$1 AND domain=$2 AND active FOR SHARE`, record.AppID, strings.ToLower(senderDomain)).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			if smtp {
				return receipt, submission.ErrSenderDomainInactive
			}
			return receipt, submission.ErrInvalidMessage
		}
		if err != nil {
			return receipt, err
		}
	}
	// Source object IDs are cryptographically random and have no tenant data.
	object, err := objectkey.Parse(record.ObjectKey)
	if err != nil || object.Kind != "submissions" {
		return receipt, submission.ErrInvalidMessage
	}
	messageID := object.ID
	var recipients recipientRows
	if !recipients.add(messageID, record.Recipients, record.RecipientKinds, record.RecipientCount, smtp) {
		return receipt, submission.ErrInvalidMessage
	}
	if record.IdempotencyKey != "" {
		var inserted string
		if smtp {
			// A provisional expiry measured from the transaction start; the window
			// really starts at acceptance, so it is reset just before COMMIT (the
			// object check below can take up to 30 seconds).
			err = tx.QueryRowContext(ctx, `INSERT INTO idempotency_keys (app_id, adapter, key, request_hash, message_id, expires_at)
			VALUES ($1,$2,$3,$4,$5,now()+$6::bigint*interval '1 millisecond')
			ON CONFLICT (app_id,adapter,key) DO UPDATE SET request_hash=EXCLUDED.request_hash, message_id=EXCLUDED.message_id, expires_at=EXCLUDED.expires_at, batch_receipt_ids=NULL
			WHERE idempotency_keys.expires_at <= now() RETURNING message_id`, record.AppID, record.Adapter, record.IdempotencyKey, record.RequestHash[:], messageID, record.IdempotencyTTL.Milliseconds()).Scan(&inserted)
		} else {
			err = tx.QueryRowContext(ctx, `INSERT INTO idempotency_keys (app_id, adapter, key, request_hash, message_id, expires_at)
			VALUES ($1,$2,$3,$4,$5,now()+interval '24 hours')
			ON CONFLICT (app_id,adapter,key) DO UPDATE SET request_hash=EXCLUDED.request_hash, message_id=EXCLUDED.message_id, expires_at=EXCLUDED.expires_at, batch_receipt_ids=NULL
			WHERE idempotency_keys.expires_at <= now() RETURNING message_id`, record.AppID, record.Adapter, record.IdempotencyKey, record.RequestHash[:], messageID).Scan(&inserted)
		}
		if errors.Is(err, sql.ErrNoRows) {
			var hash []byte
			var single bool
			err = tx.QueryRowContext(ctx, `SELECT i.request_hash, m.id, m.accepted_at, i.batch_receipt_ids IS NULL FROM idempotency_keys i JOIN messages m ON m.id=i.message_id WHERE i.app_id=$1 AND i.adapter=$2 AND i.key=$3`, record.AppID, record.Adapter, record.IdempotencyKey).Scan(&hash, &receipt.ID, &receipt.AcceptedAt, &single)
			if err != nil {
				return submission.Receipt{}, err
			}
			if !single || !record.MatchesRequestHash(hash) {
				return submission.Receipt{}, submission.ErrIdempotencyConflict
			}
			if err := tx.Commit(); err != nil {
				return submission.Receipt{}, err
			}
			receipt.Replayed = true
			return receipt, nil
		}
		if err != nil {
			return receipt, err
		}
	}
	// The app row is locked for the entire transaction, so concurrent replicas
	// cannot overspend the quota. A duplicate receipt returned above is free.
	// A limits override neither checks nor consumes the daily quota and the
	// pending limit: recipient_quota_used and its day are left untouched.
	if !record.LimitsOverride {
		var quotaUpdated bool
		err = tx.QueryRowContext(ctx, `UPDATE apps SET
			recipient_quota_used=(CASE WHEN recipient_quota_day=(statement_timestamp() AT TIME ZONE 'UTC')::date THEN recipient_quota_used ELSE 0 END)+$2,
			recipient_quota_day=(statement_timestamp() AT TIME ZONE 'UTC')::date
			WHERE id=$1 AND (CASE WHEN recipient_quota_day=(statement_timestamp() AT TIME ZONE 'UTC')::date THEN recipient_quota_used ELSE 0 END)+$2 <= recipient_daily_limit
			RETURNING true`, record.AppID, record.RecipientCount).Scan(&quotaUpdated)
		if errors.Is(err, sql.ErrNoRows) {
			return submission.Receipt{}, submission.ErrQuotaExceeded
		}
		if err != nil {
			return submission.Receipt{}, err
		}
		// This transaction holds the application row lock through the insert, so
		// concurrent replicas cannot both consume the final pending-message slot.
		// The daily-quota update also rolls back if the pending limit is reached.
		if pendingLimit == 0 {
			return submission.Receipt{}, submission.ErrBacklogExceeded
		}
		pending, err := countPendingMessages(ctx, tx, record.AppID, pendingLimit)
		if err != nil {
			return submission.Receipt{}, err
		}
		if pending >= pendingLimit {
			return submission.Receipt{}, submission.ErrBacklogExceeded
		}
	}
	if err = r.lockAndVerifyObjects(ctx, tx, []string{record.ObjectKey}); err != nil {
		return submission.Receipt{}, err
	}
	if smtp {
		// Lock the session lease last, after every statement that can block
		// (the idempotency insert, the quota and backlog checks and the object
		// check, whose storage call can take 30 seconds): the share lock then
		// stalls lease renewal for a few statements only. Once held, acquiring a
		// later epoch for the worker waits for this commit.
		live, err := lockLiveControllerLease(ctx, tx, lease)
		if err != nil {
			return submission.Receipt{}, err
		}
		if !live {
			return submission.Receipt{}, delivery.ErrLeaseInvalid
		}
	}
	state, holdMS := initialMessageState(record.Hold)
	if smtp {
		err = tx.QueryRowContext(ctx, `INSERT INTO messages (id, app_id, object_key, enhanced_protection, subject, recipient_count, sender_domain, sender_domain_rev, sender_address, sender_name, state, hold_expires_at, console_test, limits_override, recipient_policy_override, smtp_credential_id, original_from_address) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,now()+$12::bigint*interval '1 millisecond',false,false,false,$13,$14) RETURNING id,accepted_at`,
			messageID, record.AppID, record.ObjectKey, record.EnhancedProtection, record.Subject, record.RecipientCount, record.SenderDomain, reverseDomainLabels(record.SenderDomain), record.SenderAddress, nullableName(record.SenderName), state, holdMS, record.SMTPCredentialID, nullableName(record.OriginalFromAddress)).Scan(&receipt.ID, &receipt.AcceptedAt)
	} else {
		err = tx.QueryRowContext(ctx, `INSERT INTO messages (id, app_id, object_key, enhanced_protection, subject, recipient_count, sender_domain, sender_domain_rev, sender_address, sender_name, state, hold_expires_at, console_test, limits_override, recipient_policy_override) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,now()+$12::bigint*interval '1 millisecond',$13,$14,$15) RETURNING id,accepted_at`,
			messageID, record.AppID, record.ObjectKey, record.EnhancedProtection, record.Subject, record.RecipientCount, record.SenderDomain, reverseDomainLabels(record.SenderDomain), record.SenderAddress, nullableName(record.SenderName), state, holdMS, record.Operator != "", record.LimitsOverride, record.RecipientPolicyOverride).Scan(&receipt.ID, &receipt.AcceptedAt)
	}
	if err != nil {
		return submission.Receipt{}, err
	}
	if err = insertRecipients(ctx, tx, record.AppID, &recipients); err != nil {
		return submission.Receipt{}, err
	}
	if record.Operator != "" {
		auditID, idErr := newOpaqueID()
		if idErr != nil {
			return submission.Receipt{}, idErr
		}
		// Only the recipient count and the two overrides: never the subject,
		// addresses, domain or body.
		if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
			VALUES ($1,$2,$3,'message.test.submitted',$4,jsonb_build_object('recipient_count',$5::integer,'bypass_limits',$6::boolean,'bypass_recipient_policy',$7::boolean))`, auditID, record.Operator, record.AppID, messageID, record.RecipientCount, record.LimitsOverride, record.RecipientPolicyOverride); err != nil {
			return submission.Receipt{}, err
		}
	}
	if record.Hold {
		if err = insertMessageEvent(ctx, tx, record.AppID, messageID, holdEventHeld); err != nil {
			return submission.Receipt{}, err
		}
	}
	if smtp {
		// The last statement before COMMIT: the lease must still be live (the
		// first check was made before the inserts), and the idempotency window
		// starts now, at acceptance. The transaction owns the row, so nobody has
		// seen the provisional value.
		live, err := controllerLeaseLive(ctx, tx, lease)
		if err != nil {
			return submission.Receipt{}, err
		}
		if !live {
			return submission.Receipt{}, delivery.ErrLeaseInvalid
		}
		if record.IdempotencyKey != "" {
			if _, err = tx.ExecContext(ctx, `UPDATE idempotency_keys SET expires_at=clock_timestamp()+$4::bigint*interval '1 millisecond' WHERE app_id=$1 AND adapter=$2 AND key=$3`,
				record.AppID, record.Adapter, record.IdempotencyKey, record.IdempotencyTTL.Milliseconds()); err != nil {
				return submission.Receipt{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return submission.Receipt{}, err
	}
	return receipt, nil
}
