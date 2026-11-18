package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/submission"
)

// Suppression addresses are plaintext, like message_recipients. Nothing in
// this file logs one, wraps one into an error, writes one to an audit row or
// puts one in SQL text: every failure is an admin sentinel, every statement
// takes values as parameters, and writes use ON CONFLICT DO NOTHING so a
// constraint violation (whose DETAIL would quote the row) cannot fire. Go
// validation mirrors every CHECK on the table.

func validSuppressionReason(reason string) bool { return admin.ValidSuppressionReason(reason) }

// suppressionKeys mirrors the CHECKs on address, address_lower and domain_lower.
func suppressionKeys(address string) (lower, domain string, ok bool) {
	lower, domain, _, ok = recipientKeys(address)
	if !ok || len(domain) < 1 || len(domain) > maxRecordedAddressBytes {
		return "", "", false
	}
	return lower, domain, true
}

func (r *Repository) ListSuppressions(ctx context.Context, appID string, query admin.SuppressionQuery) (admin.SuppressionPage, error) {
	var page admin.SuppressionPage
	if !opaqueID(appID) || query.Limit < 1 || query.Limit > 100 || (query.Reason != "" && !validSuppressionReason(query.Reason)) || len(query.Cursor) > 512 {
		return page, admin.ErrInvalidInput
	}
	cursor, err := parseHistoryCursor(query.Cursor)
	if err != nil {
		return page, err
	}
	var addressLike, domainExact, reason, before any
	if query.Reason != "" {
		reason = query.Reason
	}
	if query.Q != "" {
		lower := strings.ToLower(query.Q)
		switch query.Field {
		case admin.SuppressionFieldAddress, "":
			addressLike = likeEscape(lower) + "%"
		case admin.SuppressionFieldDomain:
			domainExact = strings.TrimPrefix(lower, "@")
		default:
			return page, admin.ErrInvalidInput
		}
	}
	if !cursor.At.IsZero() {
		before = cursor.At
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	// Prefix LIKE only uses text_pattern_ops indexes under per-execution plans,
	// and an unbounded search must not hold a connection.
	if _, err = tx.ExecContext(ctx, `SET LOCAL plan_cache_mode = force_custom_plan`); err != nil {
		return page, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout = '5s'`); err != nil {
		return page, err
	}
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return page, admin.ErrNotFound
	}
	if err != nil {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,address,reason,origin,created_at,COALESCE(source_message_id,'') FROM suppressions
		WHERE app_id=$1 AND ($2::text IS NULL OR reason=$2) AND ($3::text IS NULL OR address_lower LIKE $3 ESCAPE '\')
		AND ($4::text IS NULL OR domain_lower=$4) AND ($5::timestamptz IS NULL OR (created_at,id)<($5,$6::text))
		ORDER BY created_at DESC,id DESC LIMIT $7`, appID, reason, addressLike, domainExact, before, cursor.ID, query.Limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var item admin.Suppression
		if err = rows.Scan(&item.ID, &item.Address, &item.Reason, &item.Origin, &item.CreatedAt, &item.SourceMessageID); err != nil {
			return admin.SuppressionPage{}, err
		}
		page.Suppressions = append(page.Suppressions, item)
	}
	if err = rows.Err(); err != nil {
		return admin.SuppressionPage{}, err
	}
	rows.Close()
	if len(page.Suppressions) > query.Limit {
		page.Suppressions = page.Suppressions[:query.Limit]
		last := page.Suppressions[len(page.Suppressions)-1]
		data, marshalErr := json.Marshal(historyCursor{At: last.CreatedAt, ID: last.ID})
		if marshalErr != nil {
			return admin.SuppressionPage{}, marshalErr
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	if err = tx.Commit(); err != nil {
		return admin.SuppressionPage{}, err
	}
	return page, nil
}

func (r *Repository) AddSuppressions(ctx context.Context, actor, appID string, addresses []string) ([]admin.SuppressionResult, error) {
	return r.addSuppressions(ctx, actor, appID, "", admin.SuppressionOriginConsole, addresses)
}

// requireActiveKey mirrors the send path (ReserveSubmission): the app and the
// key must both be active when the write transaction runs. It returns
// submission.ErrInvalidKey otherwise, which the adapters answer with 401.
func requireActiveKey(ctx context.Context, tx *sql.Tx, appID, keyID string) error {
	if keyID == "" || len(keyID) > 256 {
		return submission.ErrInvalidKey
	}
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1 AND active FOR SHARE`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return submission.ErrInvalidKey
	}
	if err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, `SELECT true FROM api_keys WHERE id=$1 AND app_id=$2 AND active FOR SHARE`, keyID, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return submission.ErrInvalidKey
	}
	return err
}

// addSuppressions adds entries. A non-empty keyID (provider API) additionally
// requires that key and its app to be active.
func (r *Repository) addSuppressions(ctx context.Context, actor, appID, keyID, origin string, addresses []string) ([]admin.SuppressionResult, error) {
	if !adminText(actor, 256) || !opaqueID(appID) || len(addresses) < 1 || len(addresses) > 100 {
		return nil, admin.ErrInvalidInput
	}
	type entry struct{ address, lower, domain string }
	entries := make([]entry, len(addresses))
	for i, address := range addresses {
		lower, domain, ok := suppressionKeys(address)
		if !ok {
			return nil, admin.ErrInvalidInput
		}
		entries[i] = entry{address, lower, domain}
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if keyID != "" {
		if err = requireActiveKey(ctx, tx, appID, keyID); err != nil {
			return nil, err
		}
	}
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1 FOR SHARE`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, admin.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	results := make([]admin.SuppressionResult, 0, len(entries))
	for _, e := range entries {
		var item admin.Suppression
		status := "added"
		inserted := false
		for attempt := 0; attempt < 2 && !inserted; attempt++ {
			id, idErr := newOpaqueID()
			if idErr != nil {
				return nil, idErr
			}
			err = tx.QueryRowContext(ctx, `INSERT INTO suppressions(id,app_id,address,address_lower,domain_lower,reason,origin)
				VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (app_id,address_lower) DO NOTHING
				RETURNING id,address,reason,origin,created_at,COALESCE(source_message_id,'')`,
				id, appID, e.address, e.lower, e.domain, admin.SuppressionReasonManual, origin).
				Scan(&item.ID, &item.Address, &item.Reason, &item.Origin, &item.CreatedAt, &item.SourceMessageID)
			if err == nil {
				inserted = true
				break
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			err = tx.QueryRowContext(ctx, `SELECT id,address,reason,origin,created_at,COALESCE(source_message_id,'') FROM suppressions WHERE app_id=$1 AND address_lower=$2`, appID, e.lower).
				Scan(&item.ID, &item.Address, &item.Reason, &item.Origin, &item.CreatedAt, &item.SourceMessageID)
			if err == nil {
				status = "exists"
				break
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			// Deleted between the conflict and the read: try the insert again.
		}
		if status == "added" && !inserted {
			return nil, admin.ErrConflict
		}
		if status == "added" {
			if err = insertSuppressionAudit(ctx, tx, actor, appID, "suppression.added", item); err != nil {
				return nil, err
			}
		}
		results = append(results, admin.SuppressionResult{Address: e.address, Status: status, Suppression: item})
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}

func (r *Repository) DeleteSuppressions(ctx context.Context, actor, appID string, ids []string) (int, error) {
	if !adminText(actor, 256) || !opaqueID(appID) || len(ids) < 1 || len(ids) > 100 {
		return 0, admin.ErrInvalidInput
	}
	for _, id := range ids {
		if !opaqueID(id) {
			return 0, admin.ErrInvalidInput
		}
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, admin.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `DELETE FROM suppressions WHERE app_id=$1 AND id=ANY($2::text[]) RETURNING id,reason,origin`, appID, ids)
	if err != nil {
		return 0, err
	}
	var removed []admin.Suppression
	for rows.Next() {
		var item admin.Suppression
		if err = rows.Scan(&item.ID, &item.Reason, &item.Origin); err != nil {
			rows.Close()
			return 0, err
		}
		removed = append(removed, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, item := range removed {
		if err = insertSuppressionAudit(ctx, tx, actor, appID, "suppression.removed", item); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return len(removed), nil
}

// insertSuppressionAudit records the action, the suppression ID and the
// reason and origin. The address is never written.
func insertSuppressionAudit(ctx context.Context, tx *sql.Tx, actor, appID, action string, item admin.Suppression) error {
	auditID, err := newOpaqueID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
		VALUES ($1,$2,$3,$4,$5,jsonb_build_object('reason',$6::text,'origin',$7::text))`, auditID, actor, appID, action, item.ID, item.Reason, item.Origin)
	return err
}

// SuppressedRecipients counts how many of the distinct lowercase addresses are
// on the app's list. It backs the Postmark all-suppressed rejection.
func (r *Repository) SuppressedRecipients(ctx context.Context, appID string, lowers []string) (int, error) {
	if !opaqueID(appID) || len(lowers) < 1 || len(lowers) > maxRecordedRecipients {
		return 0, submission.ErrInvalidMessage
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM suppressions WHERE app_id=$1 AND address_lower=ANY($2::text[])`, appID, lowers).Scan(&count); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

const maxProviderSuppressionRows = 10001

// ListProviderSuppressions serves the Postmark dump and the Resend list.
func (r *Repository) ListProviderSuppressions(ctx context.Context, appID string, f submission.ProviderSuppressionFilter) ([]submission.ProviderSuppression, bool, error) {
	if !opaqueID(appID) || f.Limit < 1 || f.Limit >= maxProviderSuppressionRows+1 || (f.After != "" && f.Before != "") ||
		(f.After != "" && !opaqueID(f.After)) || (f.Before != "" && !opaqueID(f.Before)) || len(f.Reasons) > 4 || len(f.Origins) > 3 {
		return nil, false, submission.ErrInvalidMessage
	}
	for _, reason := range f.Reasons {
		if !validSuppressionReason(reason) {
			return nil, false, submission.ErrInvalidMessage
		}
	}
	for _, origin := range f.Origins {
		switch origin {
		case admin.SuppressionOriginRecipient, admin.SuppressionOriginAPI, admin.SuppressionOriginConsole:
		default:
			return nil, false, submission.ErrInvalidMessage
		}
	}
	var reasons, origins, from, to any
	if len(f.Reasons) > 0 {
		reasons = f.Reasons
	}
	if len(f.Origins) > 0 {
		origins = f.Origins
	}
	if !f.From.IsZero() {
		from = f.From
	}
	if !f.To.IsZero() {
		to = f.To
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout = '10s'`); err != nil {
		return nil, false, err
	}
	var anchorAt sql.NullTime
	anchorID, newer := f.After, false
	if f.Before != "" {
		anchorID, newer = f.Before, true
	}
	if anchorID != "" {
		err = tx.QueryRowContext(ctx, `SELECT created_at FROM suppressions WHERE app_id=$1 AND id=$2`, appID, anchorID).Scan(&anchorAt.Time)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, submission.ErrInvalidMessage
		}
		if err != nil {
			return nil, false, err
		}
		anchorAt.Valid = true
	}
	var older, newerAt any
	if anchorAt.Valid {
		if newer {
			newerAt = anchorAt.Time
		} else {
			older = anchorAt.Time
		}
	}
	order := "DESC"
	if newer {
		order = "ASC"
	}
	// order is one of two constants, never input.
	rows, err := tx.QueryContext(ctx, `SELECT id,address,reason,origin,created_at,COALESCE(source_message_id,'') FROM suppressions
		WHERE app_id=$1 AND ($2::text[] IS NULL OR reason=ANY($2)) AND ($3::text[] IS NULL OR origin=ANY($3))
		AND ($4::timestamptz IS NULL OR created_at>=$4) AND ($5::timestamptz IS NULL OR created_at<$5)
		AND ($6::timestamptz IS NULL OR (created_at,id)<($6,$8::text)) AND ($7::timestamptz IS NULL OR (created_at,id)>($7,$8::text))
		ORDER BY created_at `+order+`,id `+order+` LIMIT $9`, appID, reasons, origins, from, to, older, newerAt, anchorID, f.Limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var items []submission.ProviderSuppression
	for rows.Next() {
		var item submission.ProviderSuppression
		if err = rows.Scan(&item.ID, &item.Address, &item.Reason, &item.Origin, &item.CreatedAt, &item.SourceMessageID); err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	rows.Close()
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	more := len(items) > f.Limit
	if more {
		items = items[:f.Limit]
	}
	if newer {
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	}
	return items, more, nil
}

// AddProviderSuppressions adds manual entries with origin api.
func (r *Repository) AddProviderSuppressions(ctx context.Context, app submission.App, addresses []string) error {
	_, err := r.addSuppressions(ctx, "key:"+app.KeyID, app.ID, app.KeyID, admin.SuppressionOriginAPI, addresses)
	return err
}

// DeleteProviderSuppressions removes entries by address (the address travels
// only as a bound parameter). Recipient-originated entries and spam complaints
// stay: only the console can remove them.
func (r *Repository) DeleteProviderSuppressions(ctx context.Context, app submission.App, addresses []string) ([]bool, error) {
	actor, appID := "key:"+app.KeyID, app.ID
	if !adminText(actor, 256) || !opaqueID(appID) || len(addresses) < 1 || len(addresses) > 100 {
		return nil, admin.ErrInvalidInput
	}
	lowers := make([]string, len(addresses))
	for i, address := range addresses {
		lower, _, ok := suppressionKeys(address)
		if !ok {
			return nil, admin.ErrInvalidInput
		}
		lowers[i] = lower
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = requireActiveKey(ctx, tx, appID, app.KeyID); err != nil {
		return nil, err
	}
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, admin.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	results := make([]bool, len(lowers))
	for i, lower := range lowers {
		var item admin.Suppression
		err = tx.QueryRowContext(ctx, `DELETE FROM suppressions WHERE app_id=$1 AND address_lower=$2 AND origin<>'recipient' AND reason<>'spam_complaint' RETURNING id,reason,origin`, appID, lower).
			Scan(&item.ID, &item.Reason, &item.Origin)
		if err == nil {
			if err = insertSuppressionAudit(ctx, tx, actor, appID, "suppression.removed", item); err != nil {
				return nil, err
			}
			results[i] = true
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		var protected bool
		err = tx.QueryRowContext(ctx, `SELECT true FROM suppressions WHERE app_id=$1 AND address_lower=$2`, appID, lower).Scan(&protected)
		if errors.Is(err, sql.ErrNoRows) {
			results[i] = true
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}
