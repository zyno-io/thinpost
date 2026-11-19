// Package postgres implements durable submission and delivery transactions.
// The complete mail service and its deployment are not yet wired.
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"net/mail"
	"strings"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/submission"
)

// Schema is exported for integration testing and migration authoring. It is not
// applied automatically: production must use versioned migrations.
//
//go:embed schema.sql migrations/*.sql
var Schema embed.FS

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
	if !validRecordedSubject(record.Subject) || !validRecordedDomain(record.SenderDomain) || record.SenderDomain != domain || !validRecordedSender(record.SenderAddress, record.SenderName, domain) {
		return receipt, submission.ErrInvalidMessage
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return receipt, err
	}
	defer tx.Rollback()
	var pendingLimit int
	err = tx.QueryRowContext(ctx, `SELECT pending_message_limit FROM apps WHERE id=$1 AND active FOR UPDATE`, record.AppID).Scan(&pendingLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, submission.ErrInvalidMessage
	}
	if err != nil {
		return receipt, err
	}
	var exists bool
	// Exactly one principal. A key submission rechecks its key; an operator
	// submission has no key and is bound to the console adapter and to no
	// idempotency key. Any other combination is refused, so a stray operator
	// value can never open a keyless path for a provider adapter.
	switch {
	case record.KeyID != "" && record.Operator == "":
		if record.Adapter == submission.AdapterConsole {
			return receipt, submission.ErrInvalidKey
		}
		err = tx.QueryRowContext(ctx, `SELECT true FROM api_keys WHERE id=$1 AND app_id=$2 AND active FOR SHARE`, record.KeyID, record.AppID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return receipt, submission.ErrInvalidKey
		}
		if err != nil {
			return receipt, err
		}
	case record.KeyID == "" && adminText(record.Operator, 256) && record.Adapter == submission.AdapterConsole && record.IdempotencyKey == "":
	default:
		return receipt, submission.ErrInvalidKey
	}
	for _, senderDomain := range []string{domain, record.SendingDomain} {
		if senderDomain == "" {
			continue
		}
		err = tx.QueryRowContext(ctx, `SELECT true FROM sender_domains WHERE app_id=$1 AND domain=$2 AND active FOR SHARE`, record.AppID, strings.ToLower(senderDomain)).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
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
	if !recipients.add(messageID, record.Recipients, record.RecipientKinds, record.RecipientCount) {
		return receipt, submission.ErrInvalidMessage
	}
	if record.IdempotencyKey != "" {
		var inserted string
		err = tx.QueryRowContext(ctx, `INSERT INTO idempotency_keys (app_id, adapter, key, request_hash, message_id, expires_at)
			VALUES ($1,$2,$3,$4,$5,now()+interval '24 hours')
			ON CONFLICT (app_id,adapter,key) DO UPDATE SET request_hash=EXCLUDED.request_hash, message_id=EXCLUDED.message_id, expires_at=EXCLUDED.expires_at, batch_receipt_ids=NULL
			WHERE idempotency_keys.expires_at <= now() RETURNING message_id`, record.AppID, record.Adapter, record.IdempotencyKey, record.RequestHash[:], messageID).Scan(&inserted)
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
	if err = r.lockAndVerifyObjects(ctx, tx, []string{record.ObjectKey}); err != nil {
		return submission.Receipt{}, err
	}
	state, holdMS := initialMessageState(record.Hold)
	err = tx.QueryRowContext(ctx, `INSERT INTO messages (id, app_id, object_key, enhanced_protection, subject, recipient_count, sender_domain, sender_domain_rev, sender_address, sender_name, state, hold_expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,now()+$12::bigint*interval '1 millisecond') RETURNING id,accepted_at`,
		messageID, record.AppID, record.ObjectKey, record.EnhancedProtection, record.Subject, record.RecipientCount, record.SenderDomain, reverseDomainLabels(record.SenderDomain), record.SenderAddress, nullableName(record.SenderName), state, holdMS).Scan(&receipt.ID, &receipt.AcceptedAt)
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
		// Only the recipient count: never the subject, addresses or body.
		if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
			VALUES ($1,$2,$3,'message.test.submitted',$4,jsonb_build_object('recipient_count',$5::integer))`, auditID, record.Operator, record.AppID, messageID, record.RecipientCount); err != nil {
			return submission.Receipt{}, err
		}
	}
	if record.Hold {
		if err = insertMessageEvent(ctx, tx, record.AppID, messageID, holdEventHeld); err != nil {
			return submission.Receipt{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return submission.Receipt{}, err
	}
	return receipt, nil
}
