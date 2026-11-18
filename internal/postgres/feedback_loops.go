package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/feedback"
)

// Spam-complaint intake. Nothing here stores or logs a report, an address from
// a report or reporter free text: the suppressed address is copied inside SQL
// from message_recipients, and only IDs, enums and the reporter DKIM domain are
// persisted.

// IsMailbox implements feedback.Repository.
func (r *Repository) IsMailbox(ctx context.Context, tokenHash [32]byte) (bool, error) {
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM feedback_mailbox WHERE token_hash=$1)`, tokenHash[:]).Scan(&exists); err != nil {
		return false, err
	}
	return exists, tx.Commit()
}

// Reporters implements feedback.Repository.
func (r *Repository) Reporters(ctx context.Context) ([]string, error) {
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	domains, err := queryReporters(ctx, tx)
	if err != nil {
		return nil, err
	}
	return domains, tx.Commit()
}

func queryReporters(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT domain FROM feedback_reporters ORDER BY domain LIMIT 33`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	domains := []string{}
	for rows.Next() {
		var domain string
		if err = rows.Scan(&domain); err != nil {
			return nil, err
		}
		domains = append(domains, domain)
	}
	return domains, rows.Err()
}

// Candidates implements feedback.Repository. One UNION ALL statement, so the
// shared flag and the delivery rows come from one snapshot. A delivery ID
// matches only a legacy message (per-delivery Message-ID); a message ID matches
// only a message prepared with the shared Message-ID.
func (r *Repository) Candidates(ctx context.Context, id string) ([]feedback.Delivery, error) {
	if !opaqueID(id) {
		return nil, nil
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT d.id,m.id,m.app_id,m.sender_domain,d.state,d.bounce_token_hash FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE d.id=$1 AND NOT m.shared_message_id
		UNION ALL
		SELECT d.id,m.id,m.app_id,m.sender_domain,d.state,d.bounce_token_hash FROM messages m JOIN deliveries d ON d.message_id=m.id WHERE m.id=$1 AND m.shared_message_id
		LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []feedback.Delivery
	for rows.Next() {
		var d feedback.Delivery
		var domain sql.NullString
		if err = rows.Scan(&d.ID, &d.MessageID, &d.AppID, &domain, &d.State, &d.BounceTokenHash); err != nil {
			return nil, err
		}
		d.SenderDomain = domain.String
		out = append(out, d)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// Count implements feedback.Repository.
func (r *Repository) Count(ctx context.Context, result feedback.Result) error {
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = countFeedback(ctx, tx, feedback.Channel, result); err != nil {
		return err
	}
	return tx.Commit()
}

func countFeedback(ctx context.Context, tx *sql.Tx, channel string, result feedback.Result) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO inbound_feedback_daily(day,channel,result,count) VALUES ((clock_timestamp() AT TIME ZONE 'UTC')::date,$1,$2,1)
		ON CONFLICT (day,channel,result) DO UPDATE SET count=LEAST(inbound_feedback_daily.count+1,1000000000)`, channel, string(result))
	return err
}

// Record implements feedback.Repository. In one transaction it inserts the
// recipient_feedback row; for a new row it also writes the spam_complaint
// suppression (precedence-ranked), the complained webhook event and its fan-out.
// A duplicate changes nothing but the counter. The delivery state is never
// changed. The counter is always incremented.
func (r *Repository) Record(ctx context.Context, deliveryID, reporterDomain string) (feedback.Result, error) {
	if !opaqueID(deliveryID) || len(reporterDomain) == 0 || len(reporterDomain) > 253 {
		return "", errors.New("invalid feedback")
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var appID, messageID, state string
	err = tx.QueryRowContext(ctx, `SELECT m.app_id,m.id,d.state FROM deliveries d JOIN messages m ON m.id=d.message_id
		JOIN message_recipients r ON r.message_id=d.message_id AND r.recipient_index=d.recipient_index WHERE d.id=$1`, deliveryID).Scan(&appID, &messageID, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return finishCount(ctx, tx, feedback.Uncorrelated)
	}
	if err != nil {
		return "", err
	}
	if state != "accepted" {
		return finishCount(ctx, tx, feedback.NotDelivered)
	}
	id, err := newOpaqueID()
	if err != nil {
		return "", err
	}
	var occurred sql.NullTime
	err = tx.QueryRowContext(ctx, `INSERT INTO recipient_feedback(id,delivery_id,kind,channel,reporter_domain) VALUES ($1,$2,'spam_complaint','arf',$3)
		ON CONFLICT (delivery_id,kind) DO NOTHING RETURNING received_at`, id, deliveryID, reporterDomain).Scan(&occurred)
	if errors.Is(err, sql.ErrNoRows) {
		// A repeat changes nothing: a replayed old report must not undo an
		// operator's removal of the suppression.
		return finishCount(ctx, tx, feedback.Duplicate)
	}
	if err != nil {
		return "", err
	}
	if err = upsertRecipientSuppression(ctx, tx, deliveryID, "spam_complaint"); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_events(id,app_id,message_id,delivery_id,outcome,smtp_code,occurred_at,feedback_id)
		VALUES ($1,$2,$3,$4,'complained',0,$5,$1)`, id, appID, messageID, deliveryID, occurred.Time); err != nil {
		return "", err
	}
	// A legacy app over the active-subscription cap still records the complaint
	// and its event; only the fan-out is skipped.
	if err = enqueueWebhookDeliveries(ctx, tx, id, appID); err != nil && !errors.Is(err, errWebhookFanoutExceeded) {
		return "", err
	}
	return finishCount(ctx, tx, feedback.Accepted)
}

func finishCount(ctx context.Context, tx *sql.Tx, result feedback.Result) (feedback.Result, error) {
	if err := countFeedback(ctx, tx, feedback.Channel, result); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", errors.New("feedback commit not confirmed")
	}
	return result, nil
}

// FeedbackLoops implements admin.FeedbackLoopStore with a read-only snapshot
// that works while the deploy gate is closed.
func (r *Repository) FeedbackLoops(ctx context.Context) (admin.FeedbackLoops, error) {
	out := admin.FeedbackLoops{Reporters: []string{}, Counts: []admin.FeedbackCount{}}
	tx, err := r.beginOperationsRead(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var token string
	var created time.Time
	err = tx.QueryRowContext(ctx, `SELECT token,created_at FROM feedback_mailbox WHERE singleton`).Scan(&token, &created)
	if err == nil {
		out.Mailbox = &admin.FeedbackMailbox{Token: token, CreatedAt: created}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if out.Reporters, err = queryReporters(ctx, tx); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT day::text,channel,result,count FROM inbound_feedback_daily
		WHERE day > (clock_timestamp() AT TIME ZONE 'UTC')::date - 14 ORDER BY day DESC,channel,result`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var c admin.FeedbackCount
		if err = rows.Scan(&c.Day, &c.Channel, &c.Result, &c.Count); err != nil {
			return out, err
		}
		out.Counts = append(out.Counts, c)
	}
	return out, rows.Err()
}

// CreateFeedbackMailbox creates the single mailbox, or with rotate replaces its
// token. Without rotate an existing mailbox is admin.ErrConflict. Rotation makes
// the previous address stop being a mailbox immediately. The audit row has
// app_id NULL and never contains the token.
func (r *Repository) CreateFeedbackMailbox(ctx context.Context, actor string, rotate bool) (admin.FeedbackMailbox, error) {
	if !adminText(actor, 256) {
		return admin.FeedbackMailbox{}, admin.ErrInvalidInput
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return admin.FeedbackMailbox{}, err
	}
	token := hex.EncodeToString(raw[:])
	hash := sha256.Sum256([]byte(token))
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return admin.FeedbackMailbox{}, err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM feedback_mailbox WHERE singleton FOR UPDATE)`).Scan(&exists); err != nil {
		return admin.FeedbackMailbox{}, err
	}
	if exists && !rotate {
		return admin.FeedbackMailbox{}, admin.ErrConflict
	}
	var created time.Time
	action := "feedback.mailbox.created"
	if exists {
		action = "feedback.mailbox.rotated"
		err = tx.QueryRowContext(ctx, `UPDATE feedback_mailbox SET token=$1,token_hash=$2,created_at=clock_timestamp() WHERE singleton RETURNING created_at`, token, hash[:]).Scan(&created)
	} else {
		err = tx.QueryRowContext(ctx, `INSERT INTO feedback_mailbox(token,token_hash) VALUES ($1,$2) ON CONFLICT (singleton) DO NOTHING RETURNING created_at`, token, hash[:]).Scan(&created)
		if errors.Is(err, sql.ErrNoRows) {
			return admin.FeedbackMailbox{}, admin.ErrConflict
		}
	}
	if err != nil {
		return admin.FeedbackMailbox{}, err
	}
	auditID, err := newOpaqueID()
	if err != nil {
		return admin.FeedbackMailbox{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
		VALUES ($1,$2,NULL,$3,'feedback_mailbox','{}'::jsonb)`, auditID, actor, action); err != nil {
		return admin.FeedbackMailbox{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.FeedbackMailbox{}, err
	}
	return admin.FeedbackMailbox{Token: token, CreatedAt: created}, nil
}

// SetFeedbackReporters replaces the reporter-domain allowlist. The caller has
// normalized the domains; they must be unique here too. Audited as
// feedback.reporters.updated with the old and new lists (domains are not
// sensitive) and app_id NULL. An identical list changes and audits nothing.
func (r *Repository) SetFeedbackReporters(ctx context.Context, actor string, domains []string) ([]string, error) {
	if !adminText(actor, 256) || len(domains) > 32 {
		return nil, admin.ErrInvalidInput
	}
	next := append([]string{}, domains...)
	sort.Strings(next)
	for i, d := range next {
		if _, ok := admin.NormalizeDomain(d); !ok || d != lowerASCII(d) || len(d) < 3 || (i > 0 && next[i-1] == d) {
			return nil, admin.ErrInvalidInput
		}
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Serialize concurrent edits so the audited old list is the replaced one.
	if _, err = tx.ExecContext(ctx, `LOCK TABLE feedback_reporters IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return nil, err
	}
	old, err := queryReporters(ctx, tx)
	if err != nil {
		return nil, err
	}
	if equalStrings(old, next) {
		return next, tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM feedback_reporters WHERE NOT (domain = ANY($1::text[]))`, next); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO feedback_reporters(domain) SELECT unnest($1::text[]) ON CONFLICT (domain) DO NOTHING`, next); err != nil {
		return nil, err
	}
	auditID, err := newOpaqueID()
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor_id,app_id,action,resource_id,details)
		VALUES ($1,$2,NULL,'feedback.reporters.updated','feedback_reporters',jsonb_build_object('old',to_jsonb($3::text[]),'new',to_jsonb($4::text[])))`,
		auditID, actor, old, next); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return next, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
