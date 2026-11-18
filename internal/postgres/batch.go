package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/mail"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/submission"
)

// CommitBatch publishes every message and the optional 24-hour response
// marker in one transaction, under the same app and recovery-gate locks as a
// single submission. Any failed authorization, quota, or object check rolls
// back the whole batch. Uploaded but unreferenced objects require collection.
func (r *Repository) CommitBatch(ctx context.Context, record acceptance.BatchRecord) (submission.BatchReceipt, error) {
	if (record.Adapter != "resend" && record.Adapter != "sendgrid") || (record.Adapter == "sendgrid" && record.IdempotencyKey != "") || record.AppID == "" || record.KeyID == "" || len(record.Items) == 0 || len(record.Items) > 100 || len(record.PreviousHashes) > 7 || len(record.IdempotencyKey) > 256 || !utf8.ValidString(record.IdempotencyKey) || strings.ContainsRune(record.IdempotencyKey, 0) {
		return submission.BatchReceipt{}, submission.ErrInvalidMessage
	}
	ids, keys := make([]string, 0, len(record.Items)), make([]string, 0, len(record.Items))
	domains, seen := make(map[string]bool), make(map[string]bool)
	totalRecipients := 0
	var recipients recipientRows
	for _, item := range record.Items {
		parsed, err := objectkey.Parse(item.ObjectKey)
		if err != nil || parsed.Kind != "submissions" || seen[parsed.ID] || item.RecipientCount < 1 || item.RecipientCount > 100 || (record.Adapter == "resend" && item.RecipientCount > 50) || strings.ContainsAny(item.From, "\r\n") {
			return submission.BatchReceipt{}, submission.ErrInvalidMessage
		}
		address, err := mail.ParseAddress(item.From)
		if err != nil {
			return submission.BatchReceipt{}, submission.ErrInvalidMessage
		}
		at := strings.LastIndexByte(address.Address, '@')
		if at < 1 || at == len(address.Address)-1 {
			return submission.BatchReceipt{}, submission.ErrInvalidMessage
		}
		domain := strings.ToLower(address.Address[at+1:])
		// Validate every recorded value before any INSERT (see Commit).
		if !validRecordedSubject(item.Subject) || !validRecordedDomain(item.SenderDomain) || item.SenderDomain != domain || !validRecordedSender(item.SenderAddress, item.SenderName, domain) || !recipients.add(parsed.ID, item.Recipients, item.RecipientKinds, item.RecipientCount) {
			return submission.BatchReceipt{}, submission.ErrInvalidMessage
		}
		domains[domain] = true
		seen[parsed.ID] = true
		ids, keys = append(ids, parsed.ID), append(keys, item.ObjectKey)
		totalRecipients += item.RecipientCount
		if record.Adapter == "sendgrid" && totalRecipients > 1000 {
			return submission.BatchReceipt{}, submission.ErrInvalidMessage
		}
	}
	serialized, err := json.Marshal(ids)
	if err != nil {
		return submission.BatchReceipt{}, errors.New("cannot encode batch receipt")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return submission.BatchReceipt{}, err
	}
	defer tx.Rollback()
	var pendingLimit int
	err = tx.QueryRowContext(ctx, `SELECT pending_message_limit FROM apps WHERE id=$1 AND active FOR UPDATE`, record.AppID).Scan(&pendingLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return submission.BatchReceipt{}, submission.ErrInvalidMessage
	}
	if err != nil {
		return submission.BatchReceipt{}, err
	}
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM api_keys WHERE id=$1 AND app_id=$2 AND active FOR SHARE`, record.KeyID, record.AppID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return submission.BatchReceipt{}, submission.ErrInvalidKey
	}
	if err != nil {
		return submission.BatchReceipt{}, err
	}
	orderedDomains := make([]string, 0, len(domains))
	for domain := range domains {
		orderedDomains = append(orderedDomains, domain)
	}
	sort.Strings(orderedDomains)
	for _, domain := range orderedDomains {
		err = tx.QueryRowContext(ctx, `SELECT true FROM sender_domains WHERE app_id=$1 AND domain=$2 AND active FOR SHARE`, record.AppID, domain).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return submission.BatchReceipt{}, submission.ErrInvalidMessage
		}
		if err != nil {
			return submission.BatchReceipt{}, err
		}
	}
	if record.IdempotencyKey != "" {
		var inserted string
		err = tx.QueryRowContext(ctx, `INSERT INTO idempotency_keys(app_id,adapter,key,request_hash,message_id,expires_at,batch_receipt_ids)
			VALUES ($1,$2,$3,$4,$5,now()+interval '24 hours',$6::jsonb)
			ON CONFLICT (app_id,adapter,key) DO UPDATE SET request_hash=EXCLUDED.request_hash,message_id=EXCLUDED.message_id,expires_at=EXCLUDED.expires_at,batch_receipt_ids=EXCLUDED.batch_receipt_ids
			WHERE idempotency_keys.expires_at<=now() RETURNING message_id`, record.AppID, record.Adapter, record.IdempotencyKey, record.RequestHash[:], ids[0], string(serialized)).Scan(&inserted)
		if errors.Is(err, sql.ErrNoRows) {
			var previousHash, previousIDs []byte
			var firstID string
			err = tx.QueryRowContext(ctx, `SELECT request_hash,batch_receipt_ids,message_id FROM idempotency_keys WHERE app_id=$1 AND adapter=$2 AND key=$3`, record.AppID, record.Adapter, record.IdempotencyKey).Scan(&previousHash, &previousIDs, &firstID)
			if err != nil {
				return submission.BatchReceipt{}, err
			}
			if len(previousIDs) == 0 || !record.MatchesRequestHash(previousHash) {
				return submission.BatchReceipt{}, submission.ErrIdempotencyConflict
			}
			var existing []string
			if json.Unmarshal(previousIDs, &existing) != nil || len(existing) != len(record.Items) || len(existing) == 0 || existing[0] != firstID {
				return submission.BatchReceipt{}, errors.New("invalid persisted batch receipt")
			}
			unique := make(map[string]bool, len(existing))
			for _, id := range existing {
				if !opaqueID(id) || unique[id] {
					return submission.BatchReceipt{}, errors.New("invalid persisted batch receipt")
				}
				unique[id] = true
			}
			if err = tx.Commit(); err != nil {
				return submission.BatchReceipt{}, err
			}
			return submission.BatchReceipt{IDs: existing, Replayed: true}, nil
		}
		if err != nil {
			return submission.BatchReceipt{}, err
		}
	}
	var quotaUpdated bool
	err = tx.QueryRowContext(ctx, `UPDATE apps SET
		recipient_quota_used=(CASE WHEN recipient_quota_day=(statement_timestamp() AT TIME ZONE 'UTC')::date THEN recipient_quota_used ELSE 0 END)+$2,
		recipient_quota_day=(statement_timestamp() AT TIME ZONE 'UTC')::date
		WHERE id=$1 AND (CASE WHEN recipient_quota_day=(statement_timestamp() AT TIME ZONE 'UTC')::date THEN recipient_quota_used ELSE 0 END)+$2<=recipient_daily_limit RETURNING true`, record.AppID, totalRecipients).Scan(&quotaUpdated)
	if errors.Is(err, sql.ErrNoRows) {
		return submission.BatchReceipt{}, submission.ErrQuotaExceeded
	}
	if err != nil {
		return submission.BatchReceipt{}, err
	}
	if pendingLimit < len(record.Items) {
		return submission.BatchReceipt{}, submission.ErrBacklogExceeded
	}
	pending, err := countPendingMessages(ctx, tx, record.AppID, pendingLimit)
	if err != nil {
		return submission.BatchReceipt{}, err
	}
	if pending+len(record.Items) > pendingLimit {
		return submission.BatchReceipt{}, submission.ErrBacklogExceeded
	}
	if err = r.lockAndVerifyObjects(ctx, tx, keys); err != nil {
		return submission.BatchReceipt{}, err
	}
	for i, item := range record.Items {
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev,sender_address,sender_name) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			ids[i], record.AppID, item.ObjectKey, item.EnhancedProtection, item.Subject, item.RecipientCount, item.SenderDomain, reverseDomainLabels(item.SenderDomain), item.SenderAddress, nullableName(item.SenderName)); err != nil {
			return submission.BatchReceipt{}, err
		}
	}
	if err = insertRecipients(ctx, tx, record.AppID, &recipients); err != nil {
		return submission.BatchReceipt{}, err
	}
	if err = tx.Commit(); err != nil {
		return submission.BatchReceipt{}, err
	}
	return submission.BatchReceipt{IDs: ids}, nil
}
