package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
)

type historyCursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

func parseHistoryCursor(value string) (historyCursor, error) {
	var cursor historyCursor
	if value == "" {
		return cursor, nil
	}
	if len(value) > 512 {
		return cursor, admin.ErrInvalidInput
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor, admin.ErrInvalidInput
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&cursor); err != nil {
		return historyCursor{}, admin.ErrInvalidInput
	}
	if err = decoder.Decode(new(any)); err != io.EOF || cursor.At.IsZero() || !opaqueID(cursor.ID) {
		return historyCursor{}, admin.ErrInvalidInput
	}
	return cursor, nil
}
func encodeHistoryCursor(item admin.MessageSummary) (string, error) {
	data, err := json.Marshal(historyCursor{At: item.AcceptedAt, ID: item.ID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// summaryColumns selects one history row. The subject of an Enhanced
// Protection message is never selected: the CASE yields NULL and
// subject_masked is set instead. Recipients are the first %d envelope
// addresses (To, then Cc, then Bcc), each with its kind.
const summaryColumnsTemplate = `m.id,m.state,m.enhanced_protection,m.accepted_at,COUNT(d.id),
		COUNT(d.id) FILTER (WHERE d.state='accepted'),COUNT(d.id) FILTER (WHERE d.state='failed'),COUNT(d.id) FILTER (WHERE d.state='uncertain'),
		COUNT(d.id) FILTER (WHERE d.state IN ('queued','in_flight')),COUNT(d.id) FILTER (WHERE d.state IN ('recipient_policy_held','tracking_held')),
		COUNT(d.id) FILTER (WHERE d.state='recovery_hold'),
		COUNT(d.id) FILTER (WHERE d.state IN ('queued','in_flight') AND EXISTS (SELECT 1 FROM delivery_attempts a WHERE a.delivery_id=d.id AND a.state='completed' AND a.outcome IS DISTINCT FROM 'recipient_policy_denied')),
		COUNT(d.id) FILTER (WHERE d.state='failed' AND EXISTS (SELECT 1 FROM delivery_attempts a WHERE a.delivery_id=d.id AND a.outcome='suppressed')),
		CASE WHEN m.enhanced_protection THEN NULL ELSE m.subject END,
		m.enhanced_protection AND m.subject IS NOT NULL, m.sender_domain, m.sender_address, m.sender_name, m.recipient_count,
		(SELECT json_agg(json_build_array(r.address,r.kind) ORDER BY r.recipient_index) FROM
			(SELECT address,kind,recipient_index FROM message_recipients WHERE message_id=m.id ORDER BY recipient_index LIMIT %d) r),
		m.console_test,m.limits_override,m.recipient_policy_override,
		m.smtp_credential_id,(SELECT c.name FROM smtp_credentials c WHERE c.id=m.smtp_credential_id),m.original_from_address`

func summaryColumns(recipients int) string {
	return fmt.Sprintf(summaryColumnsTemplate, recipients)
}

// A delivery's recipient is the recorded address at its recipient_index;
// preparation builds deliveries in envelope order (To, Cc, Bcc). Absent for
// unrecorded messages.
const (
	deliveryRecipientColumn = "rcpt.address,rcpt.kind"
	deliveryRecipientJoin   = "LEFT JOIN message_recipients rcpt ON rcpt.message_id=d.message_id AND rcpt.recipient_index=d.recipient_index"
)

const (
	listRecipientLimit   = 3
	detailRecipientLimit = 100
)

type rowScanner interface{ Scan(...any) error }

// extra receives any columns selected after the summary columns, from the
// same row.
func scanSummary(row rowScanner, item *admin.MessageSummary, extra ...any) error {
	var subject, senderDomain, senderAddress, senderName, credentialID, credentialName, originalFrom sql.NullString
	var count sql.NullInt64
	var counts admin.DeliveryCounts
	var recipients []byte
	var consoleTest, limitsOverride, policyOverride bool
	dest := []any{&item.ID, &item.State, &item.EnhancedProtection, &item.AcceptedAt, &item.DeliveryCount, &item.AcceptedCount, &item.FailedCount, &item.UncertainCount, &counts.Pending, &counts.Held, &counts.RecoveryHeld, &counts.Retrying, &item.SuppressedCount, &subject, &item.SubjectMasked, &senderDomain, &senderAddress, &senderName, &count, &recipients, &consoleTest, &limitsOverride, &policyOverride, &credentialID, &credentialName, &originalFrom}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return err
	}
	counts.Total, counts.Accepted, counts.Failed, counts.Uncertain, counts.Suppressed = item.DeliveryCount, item.AcceptedCount, item.FailedCount, item.UncertainCount, item.SuppressedCount
	item.Status = admin.DeriveMessageStatus(item.State, counts)
	if consoleTest {
		item.OperatorTest = &admin.OperatorTest{BypassLimits: limitsOverride, BypassRecipientPolicy: policyOverride}
	}
	if credentialID.Valid {
		item.SMTP = &admin.MessageSMTP{CredentialID: credentialID.String, CredentialName: credentialName.String}
		if originalFrom.Valid {
			item.SMTP.OriginalFrom = &originalFrom.String
		}
	}
	if subject.Valid {
		item.Subject = &subject.String
	}
	if senderDomain.Valid {
		item.SenderDomain = &senderDomain.String
	}
	if senderAddress.Valid {
		item.Sender = &admin.Sender{Address: senderAddress.String}
		if senderName.Valid {
			item.Sender.Name = &senderName.String
		}
	}
	if count.Valid {
		value := int(count.Int64)
		item.RecipientCount = &value
	}
	if len(recipients) > 0 {
		var pairs [][2]string
		if err := json.Unmarshal(recipients, &pairs); err != nil {
			return errors.New("invalid recorded recipients")
		}
		item.Recipients = make([]string, len(pairs))
		item.RecipientKinds = make([]string, len(pairs))
		for i, pair := range pairs {
			item.Recipients[i], item.RecipientKinds[i] = pair[0], pair[1]
		}
	}
	return nil
}

// ListMessages returns metadata and state, never bodies or object keys. The
// subject of an Enhanced Protection message is withheld in SQL. History reads
// are not audited: only content reads of Enhanced Protection messages are.
func (r *Repository) ListMessages(ctx context.Context, actor, appID, cursorText string, limit int) (admin.MessagePage, error) {
	var page admin.MessagePage
	if !adminText(actor, 256) || !opaqueID(appID) || limit < 1 || limit > 100 {
		return page, admin.ErrInvalidInput
	}
	cursor, err := parseHistoryCursor(cursorText)
	if err != nil {
		return page, err
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var exists bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM apps WHERE id=$1`, appID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return page, admin.ErrNotFound
	}
	if err != nil {
		return page, err
	}
	var before any
	if !cursor.At.IsZero() {
		before = cursor.At
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+summaryColumns(listRecipientLimit)+`
		FROM messages m LEFT JOIN deliveries d ON d.message_id=m.id
		WHERE m.app_id=$1 AND ($2::timestamptz IS NULL OR (m.accepted_at,m.id)<($2,$3))
		GROUP BY m.id ORDER BY m.accepted_at DESC,m.id DESC LIMIT $4`, appID, before, cursor.ID, limit+1)
	if err != nil {
		return page, err
	}
	page, err = scanSummaryPage(rows, limit)
	if err != nil {
		return admin.MessagePage{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.MessagePage{}, err
	}
	return page, nil
}

// scanSummaryPage consumes and closes rows, trimming to limit and setting the
// cursor, which encodes only a timestamp and a message ID.
func scanSummaryPage(rows *sql.Rows, limit int) (admin.MessagePage, error) {
	var page admin.MessagePage
	defer rows.Close()
	for rows.Next() {
		var item admin.MessageSummary
		if err := scanSummary(rows, &item); err != nil {
			return admin.MessagePage{}, err
		}
		page.Messages = append(page.Messages, item)
	}
	if err := rows.Err(); err != nil {
		return admin.MessagePage{}, err
	}
	if len(page.Messages) > limit {
		page.Messages = page.Messages[:limit]
		cursor, err := encodeHistoryCursor(page.Messages[len(page.Messages)-1])
		if err != nil {
			return admin.MessagePage{}, err
		}
		page.NextCursor = cursor
	}
	return page, nil
}

// GetMessage returns app-scoped metadata and delivery states. It is not
// audited; the subject of an Enhanced Protection message is withheld and
// bodies require the separate content API.
func (r *Repository) GetMessage(ctx context.Context, actor, appID, messageID string) (admin.MessageDetail, error) {
	var detail admin.MessageDetail
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(messageID) {
		return detail, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return detail, err
	}
	defer tx.Rollback()
	// The hold columns come from the same row as the state, in one statement,
	// so the hold resolution can never disagree with the state and status.
	var holdExpires, holdResolved sql.NullTime
	err = scanSummary(tx.QueryRowContext(ctx, `SELECT `+summaryColumns(detailRecipientLimit)+`,m.hold_expires_at,m.hold_resolved_at
		FROM messages m LEFT JOIN deliveries d ON d.message_id=m.id WHERE m.app_id=$1 AND m.id=$2 GROUP BY m.id`, appID, messageID), &detail.Message, &holdExpires, &holdResolved)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.MessageDetail{}, admin.ErrNotFound
	}
	if err != nil {
		return admin.MessageDetail{}, err
	}
	if detail.Message.State == "preparation_failed" {
		var failure admin.PreparationFailure
		if err = tx.QueryRowContext(ctx, `SELECT prepare_attempts,prepare_failure FROM messages WHERE id=$1 AND app_id=$2 AND state='preparation_failed' FOR SHARE`, messageID, appID).Scan(&failure.Attempts, &failure.Reason); errors.Is(err, sql.ErrNoRows) {
			return admin.MessageDetail{}, admin.ErrConflict
		} else if err != nil {
			return admin.MessageDetail{}, err
		}
		detail.PreparationFailure = &failure
	}
	detail.Hold = messageHold(detail.Message.State, holdExpires, holdResolved)
	// Count attempts using the same exclusion as retry scheduling. An eligible
	// time does not guarantee dispatch: app state, the runtime gate, tracking
	// and worker availability remain independent constraints.
	rows, err := tx.QueryContext(ctx, `SELECT d.id,d.recipient_index,d.state,
		(SELECT count(*) FROM delivery_attempts a WHERE a.delivery_id=d.id AND a.outcome IS DISTINCT FROM 'recipient_policy_denied'),
		CASE WHEN d.state='queued' THEN d.retry_after ELSE NULL END,
		previous.outcome,previous.smtp_code,`+deliveryRecipientColumn+`,
		track.delivery_id IS NOT NULL,COALESCE(links.opens,0),COALESCE(links.clicks,0),links.last_event_at,msg.shared_message_id
		FROM deliveries d JOIN messages msg ON msg.id=d.message_id
		`+deliveryRecipientJoin+`
		LEFT JOIN LATERAL (SELECT a.outcome,a.smtp_code FROM delivery_attempts a
			WHERE a.delivery_id=d.id AND a.state='completed'
			ORDER BY a.started_at DESC,a.id DESC LIMIT 1) previous ON true
		LEFT JOIN tracking_objects track ON track.delivery_id=d.id
		LEFT JOIN LATERAL (SELECT COALESCE(SUM(l.event_count) FILTER (WHERE l.kind='open'),0) AS opens,
			COALESCE(SUM(l.event_count) FILTER (WHERE l.kind='click'),0) AS clicks,max(last.at) AS last_event_at
			FROM tracking_links l
			LEFT JOIN LATERAL (SELECT max(e.occurred_at) AS at FROM tracking_events e WHERE e.link_id=l.id) last ON true
			WHERE l.delivery_id=d.id) links ON true
		WHERE d.message_id=$1 ORDER BY d.recipient_index`, messageID)
	if err != nil {
		return admin.MessageDetail{}, err
	}
	for rows.Next() {
		var item admin.DeliverySummary
		var next sql.NullTime
		var outcome sql.NullString
		var code sql.NullInt64
		var recipient, kind sql.NullString
		var tracked bool
		var opens, clicks int
		var lastEvent sql.NullTime
		var shared bool
		if err = rows.Scan(&item.ID, &item.RecipientIndex, &item.State, &item.RetryAttempts, &next, &outcome, &code, &recipient, &kind, &tracked, &opens, &clicks, &lastEvent, &shared); err != nil {
			rows.Close()
			return admin.MessageDetail{}, err
		}
		if next.Valid {
			item.NextAttemptAt = &next.Time
		}
		if outcome.Valid {
			item.LastCompletedOutcome = &outcome.String
		}
		if code.Valid {
			value := int(code.Int64)
			item.LastSMTPCode = &value
		}
		if recipient.Valid {
			item.Recipient = &recipient.String
			if kind.Valid {
				item.RecipientKind = &kind.String
			}
		}
		if detail.Message.SenderDomain != nil {
			if header := messageIDHeader(shared, messageID, item.ID, *detail.Message.SenderDomain); header != "" {
				item.MessageIDHeader = &header
			}
		}
		if tracked {
			item.Tracking = &admin.DeliveryTracking{Opens: opens, Clicks: clicks}
			if lastEvent.Valid {
				item.Tracking.LastEventAt = &lastEvent.Time
			}
		}
		detail.Deliveries = append(detail.Deliveries, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return admin.MessageDetail{}, err
	}
	rows.Close()
	// Recipient feedback (kind, channel and time only), oldest first.
	if len(detail.Deliveries) > 0 {
		byDelivery := make(map[string]int, len(detail.Deliveries))
		for i := range detail.Deliveries {
			byDelivery[detail.Deliveries[i].ID] = i
		}
		feedback, feedbackErr := tx.QueryContext(ctx, `SELECT f.delivery_id,f.kind,f.channel,f.received_at FROM recipient_feedback f
			JOIN deliveries d ON d.id=f.delivery_id WHERE d.message_id=$1 ORDER BY f.received_at,f.id`, messageID)
		if feedbackErr != nil {
			return admin.MessageDetail{}, feedbackErr
		}
		for feedback.Next() {
			var deliveryID string
			var item admin.DeliveryFeedback
			if err = feedback.Scan(&deliveryID, &item.Kind, &item.Channel, &item.ReceivedAt); err != nil {
				feedback.Close()
				return admin.MessageDetail{}, err
			}
			if i, ok := byDelivery[deliveryID]; ok {
				detail.Deliveries[i].Feedback = append(detail.Deliveries[i].Feedback, item)
			}
		}
		if err = feedback.Err(); err != nil {
			feedback.Close()
			return admin.MessageDetail{}, err
		}
		feedback.Close()
	}
	if err = tx.Commit(); err != nil {
		return admin.MessageDetail{}, err
	}
	return detail, nil
}
