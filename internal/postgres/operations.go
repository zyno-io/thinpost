package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
)

// beginOperationsRead starts a read-only snapshot that deliberately bypasses
// beginRuntime: the Operations page must keep working while the deploy gate is
// closed. The statement timeout bounds the backlog scans.
func (r *Repository) beginOperationsRead(ctx context.Context) (*sql.Tx, error) {
	if r.DB == nil {
		return nil, ErrRuntimeClosed
	}
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout='2s'`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// Operations returns counts and identifiers only. Each backlog predicate is
// the one its partial index was built with (messages_app_pending_v2_idx,
// deliveries_unfinished_message_idx, webhook_deliveries_unfinished_idx).
func (r *Repository) Operations(ctx context.Context) (admin.Operations, error) {
	var out admin.Operations
	tx, err := r.beginOperationsRead(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&out.GeneratedAt); err != nil {
		return admin.Operations{}, err
	}
	var changedBy sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT mode,revision,changed_at,changed_by,COALESCE(generation=$1,false),quarantine_complete FROM runtime_gate WHERE singleton`, r.Generation).
		Scan(&out.Gate.Mode, &out.Gate.Revision, &out.Gate.ChangedAt, &changedBy, &out.Gate.GenerationMatches, &out.Gate.QuarantineComplete)
	if err != nil {
		return admin.Operations{}, err
	}
	out.Gate.GenerationMatches = out.Gate.GenerationMatches && opaqueID(r.Generation)
	if changedBy.Valid {
		out.Gate.ChangedBy = &changedBy.String
	}
	rows, err := tx.QueryContext(ctx, `SELECT worker_id,controller_id,epoch,expires_at,expires_at>clock_timestamp() FROM controller_leases ORDER BY worker_id LIMIT 100`)
	if err != nil {
		return admin.Operations{}, err
	}
	out.Workers = []admin.OperationsWorker{}
	for rows.Next() {
		var w admin.OperationsWorker
		if err = rows.Scan(&w.WorkerID, &w.ControllerID, &w.Epoch, &w.ExpiresAt, &w.Live); err != nil {
			rows.Close()
			return admin.Operations{}, err
		}
		out.Workers = append(out.Workers, w)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return admin.Operations{}, err
	}
	rows.Close()
	var oldest sql.NullTime
	m := &out.Messages
	err = tx.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE state='queued' AND prepare_next_at<=clock_timestamp()),
		count(*) FILTER (WHERE state='queued' AND prepare_next_at>clock_timestamp()),
		count(*) FILTER (WHERE state='preparing'),
		count(*) FILTER (WHERE state='preparation_failed'),
		count(*) FILTER (WHERE state='recovery_hold'),
		min(prepare_next_at) FILTER (WHERE state='queued' AND prepare_next_at<=clock_timestamp())
		FROM messages WHERE pending`).Scan(&m.QueuedDue, &m.QueuedWaiting, &m.Preparing, &m.PreparationFailed, &m.RecoveryHold, &oldest)
	if err != nil {
		return admin.Operations{}, err
	}
	m.OldestDueAt = nullTimePtr(oldest)
	d := &out.Deliveries
	err = tx.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE state='queued' AND retry_after<=clock_timestamp()),
		count(*) FILTER (WHERE state='queued' AND retry_after>clock_timestamp()),
		count(*) FILTER (WHERE state='in_flight'),
		count(*) FILTER (WHERE state='uncertain'),
		count(*) FILTER (WHERE state='recipient_policy_held'),
		count(*) FILTER (WHERE state='tracking_held'),
		count(*) FILTER (WHERE state='recovery_hold'),
		min(retry_after) FILTER (WHERE state='queued' AND retry_after<=clock_timestamp())
		FROM deliveries WHERE state NOT IN ('accepted','failed')`).Scan(&d.QueuedDue, &d.QueuedWaiting, &d.InFlight, &d.Uncertain, &d.RecipientPolicyHeld, &d.TrackingHeld, &d.RecoveryHold, &oldest)
	if err != nil {
		return admin.Operations{}, err
	}
	d.OldestDueAt = nullTimePtr(oldest)
	w := &out.Webhooks
	err = tx.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE state='queued' AND next_attempt_at<=clock_timestamp()),
		count(*) FILTER (WHERE state='queued' AND next_attempt_at>clock_timestamp()),
		count(*) FILTER (WHERE state='in_flight'),
		count(*) FILTER (WHERE state='recovery_hold'),
		min(next_attempt_at) FILTER (WHERE state='queued' AND next_attempt_at<=clock_timestamp())
		FROM webhook_deliveries WHERE state IN ('queued','in_flight','recovery_hold')`).Scan(&w.QueuedDue, &w.QueuedWaiting, &w.InFlight, &w.RecoveryHold, &oldest)
	if err != nil {
		return admin.Operations{}, err
	}
	w.OldestDueAt = nullTimePtr(oldest)
	in := &out.Inbound
	err = tx.QueryRowContext(ctx, `SELECT to_char((clock_timestamp() AT TIME ZONE 'UTC')::date,'YYYY-MM-DD'),
		COALESCE(sum(count) FILTER (WHERE result='accepted'),0)::integer,
		COALESCE(sum(count) FILTER (WHERE result='correlated'),0)::integer,
		COALESCE(sum(count) FILTER (WHERE result='duplicate'),0)::integer,
		COALESCE(sum(count) FILTER (WHERE result='tempfail_routing'),0)::integer,
		COALESCE(sum(count) FILTER (WHERE result='tempfail_limit'),0)::integer,
		COALESCE(sum(count) FILTER (WHERE result='tempfail_internal'),0)::integer
		FROM inbound_daily WHERE day=(clock_timestamp() AT TIME ZONE 'UTC')::date`).Scan(&in.Day, &in.Accepted, &in.Correlated, &in.Duplicate, &in.TempfailRouting, &in.TempfailLimit, &in.TempfailInternal)
	if err != nil {
		return admin.Operations{}, err
	}
	policy, err := queryRetryPolicy(ctx, tx, "")
	if err != nil {
		return admin.Operations{}, err
	}
	out.RetryPolicy = adminRetryPolicy(policy)
	if err = tx.Commit(); err != nil {
		return admin.Operations{}, err
	}
	return out, nil
}

func encodeAttentionCursor(item admin.AttentionItem) (string, error) {
	id := item.DeliveryID
	if id == "" {
		id = item.MessageID
	}
	data, err := json.Marshal(historyCursor{At: item.Since, ID: id})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// Attention lists messages that need an operator across all apps, ordered by
// (accepted time, entity ID). The entity is a delivery for delivery states and
// the message otherwise; both IDs are opaque and unique, so the pair is a
// stable keyset. Predicates match the partial indexes above.
func (r *Repository) Attention(ctx context.Context, cursorText string, limit int) (admin.AttentionPage, error) {
	var page admin.AttentionPage
	if limit < 1 || limit > 100 {
		return page, admin.ErrInvalidInput
	}
	cursor, err := parseHistoryCursor(cursorText)
	if err != nil {
		return page, err
	}
	tx, err := r.beginOperationsRead(ctx)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var after any
	if !cursor.At.IsZero() {
		after = cursor.At
	}
	rows, err := tx.QueryContext(ctx, `SELECT app_id,app_name,message_id,delivery_id,kind,since FROM (
		SELECT m.app_id,a.name AS app_name,m.id AS message_id,NULL::text AS delivery_id,m.state AS kind,m.accepted_at AS since,m.id AS entity_id
			FROM messages m JOIN apps a ON a.id=m.app_id WHERE m.pending AND m.state IN ('preparation_failed','recovery_hold')
		UNION ALL
		SELECT m.app_id,a.name,m.id,d.id,d.state,m.accepted_at,d.id
			FROM deliveries d JOIN messages m ON m.id=d.message_id JOIN apps a ON a.id=m.app_id
			WHERE d.state IN ('uncertain','recipient_policy_held','tracking_held','recovery_hold')
		) x WHERE ($1::timestamptz IS NULL OR (since,entity_id)>($1::timestamptz,$2::text))
		ORDER BY since,entity_id LIMIT $3`, after, cursor.ID, limit+1)
	if err != nil {
		return admin.AttentionPage{}, err
	}
	page.Items = []admin.AttentionItem{}
	for rows.Next() {
		var item admin.AttentionItem
		var delivery sql.NullString
		if err = rows.Scan(&item.AppID, &item.AppName, &item.MessageID, &delivery, &item.Kind, &item.Since); err != nil {
			rows.Close()
			return admin.AttentionPage{}, err
		}
		item.DeliveryID = delivery.String
		page.Items = append(page.Items, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return admin.AttentionPage{}, err
	}
	rows.Close()
	if err = tx.Commit(); err != nil {
		return admin.AttentionPage{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		if page.NextCursor, err = encodeAttentionCursor(page.Items[limit-1]); err != nil {
			return admin.AttentionPage{}, errors.New("encode cursor")
		}
	}
	return page, nil
}
