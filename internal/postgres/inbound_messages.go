package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

// Audit actions of Enhanced Protection reads of received content. The resource
// is the inbound message ID; nothing else is recorded.
const (
	InboundBodyReadAction = "inbound.body.read"
	InboundRawReadAction  = "inbound.raw.read"
)

// The list never selects the subject of an Enhanced Protection message: the CASE
// yields NULL. Webhook counts come from the inbound-lane deliveries of the
// message's events (cancelled ones are not counted). The expiry is judged by the
// database clock.
const inboundSummaryColumns = `im.id,im.received_at,im.header_from,CASE WHEN im.enhanced_protection THEN NULL ELSE im.subject END,im.enhanced_protection,
	im.size_bytes,im.spf_result,im.dkim_result,im.dmarc_result,im.correlated_message_id,im.content_expires_at<=clock_timestamp(),
	(SELECT r.address FROM inbound_recipients r WHERE r.inbound_id=im.id AND r.app_id=im.app_id ORDER BY r.recipient_index LIMIT 1),
	(SELECT count(*) FROM inbound_recipients r WHERE r.inbound_id=im.id AND r.app_id=im.app_id),
	wd.delivered,wd.pending,wd.failed`

const inboundSummaryJoin = `LEFT JOIN LATERAL (SELECT
		count(*) FILTER (WHERE w.state='delivered') AS delivered,
		count(*) FILTER (WHERE w.state IN ('queued','in_flight','recovery_hold')) AS pending,
		count(*) FILTER (WHERE w.state='failed') AS failed
		FROM webhook_events e JOIN webhook_deliveries w ON w.event_id=e.id AND w.app_id=e.app_id AND w.lane='inbound'
		WHERE e.inbound_id=im.id AND e.app_id=im.app_id) wd ON true`

// scanInboundSummary scans the summary columns, then any extra destinations the
// caller appended to the select list, so list and detail share one reading.
func scanInboundSummary(row rowScanner, item *admin.InboundMessageSummary, extra ...any) error {
	var from, subject, correlated sql.NullString
	var recipient sql.NullString
	dest := []any{&item.ID, &item.ReceivedAt, &from, &subject, &item.EnhancedProtection, &item.SizeBytes, &item.SPF, &item.DKIM, &item.DMARC,
		&correlated, &item.ContentExpired, &recipient, &item.RecipientCount, &item.Webhook.Delivered, &item.Webhook.Pending, &item.Webhook.Failed}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return err
	}
	if from.Valid {
		item.From = &admin.Sender{Address: from.String}
	}
	if subject.Valid {
		item.Subject = &subject.String
	}
	item.Recipient = recipient.String
	if correlated.Valid {
		item.Correlation = admin.InboundCorrelationSummary{Matched: true, MessageID: correlated.String}
	}
	w := &item.Webhook
	w.Total = w.Delivered + w.Pending + w.Failed
	w.Status = admin.DeriveInboundWebhookStatus(w.Delivered, w.Pending, w.Failed)
	return nil
}

// ListInboundMessages returns metadata for the app's received messages, newest
// first with a keyset cursor of a timestamp and a message ID. It never returns a
// body or object key, and withholds the subject of an Enhanced Protection
// message in SQL. Like the outbound history it is not audited.
func (r *Repository) ListInboundMessages(ctx context.Context, actor, appID, cursorText string, limit int) (admin.InboundMessagePage, error) {
	var page admin.InboundMessagePage
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
	rows, err := tx.QueryContext(ctx, `SELECT `+inboundSummaryColumns+` FROM inbound_messages im `+inboundSummaryJoin+`
		WHERE im.app_id=$1 AND ($2::timestamptz IS NULL OR (im.received_at,im.id)<($2,$3))
		ORDER BY im.received_at DESC,im.id DESC LIMIT $4`, appID, before, cursor.ID, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var item admin.InboundMessageSummary
		if err = scanInboundSummary(rows, &item); err != nil {
			return admin.InboundMessagePage{}, err
		}
		page.Messages = append(page.Messages, item)
	}
	if err = rows.Err(); err != nil {
		return admin.InboundMessagePage{}, err
	}
	rows.Close()
	if len(page.Messages) > limit {
		page.Messages = page.Messages[:limit]
		last := page.Messages[limit-1]
		data, err := json.Marshal(historyCursor{At: last.ReceivedAt, ID: last.ID})
		if err != nil {
			return admin.InboundMessagePage{}, err
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	if err = tx.Commit(); err != nil {
		return admin.InboundMessagePage{}, err
	}
	return page, nil
}

// storedInboundAuth reads the authentication document, falling back to the
// three result columns when it is missing or unreadable, like the webhook
// renderer. ARC is never evaluated.
func storedInboundAuth(document sql.NullString, spf, dkim, dmarc string) admin.InboundAuthentication {
	var out admin.InboundAuthentication
	var stored struct {
		SPF   admin.InboundAuthMethod   `json:"spf"`
		DKIM  []admin.InboundAuthMethod `json:"dkim"`
		DMARC admin.InboundAuthMethod   `json:"dmarc"`
		ARC   struct {
			Present bool `json:"present"`
		} `json:"arc"`
	}
	if document.Valid && json.Unmarshal([]byte(document.String), &stored) == nil && stored.SPF.Result != "" && stored.DMARC.Result != "" {
		out.SPF, out.DKIM, out.DMARC = stored.SPF, stored.DKIM, stored.DMARC
		out.ARC.Present = stored.ARC.Present
	} else {
		out.SPF.Result, out.DMARC.Result = spf, dmarc
		if dkim != "" && dkim != "none" {
			out.DKIM = []admin.InboundAuthMethod{{Result: dkim}}
		}
	}
	if out.DKIM == nil {
		out.DKIM = []admin.InboundAuthMethod{}
	}
	return out
}

// GetInboundMessage returns app-scoped metadata, the envelope, authentication
// and correlation. It reads no object and is not audited; the subject of an
// Enhanced Protection message is withheld and content needs the content API.
func (r *Repository) GetInboundMessage(ctx context.Context, actor, appID, inboundID string) (admin.InboundMessageDetail, error) {
	var out admin.InboundMessageDetail
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(inboundID) {
		return out, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var mailFrom, clientIP, helo, tlsVersion, tlsCipher, messageIDHeader, corrDelivery, corrHeader, document sql.NullString
	var fromMatches sql.NullBool
	var recipients []byte
	row := tx.QueryRowContext(ctx, `SELECT `+inboundSummaryColumns+`,im.content_expires_at,im.mail_from,host(im.client_ip),im.helo,im.tls_version,im.tls_cipher,im.message_id_header,
		im.correlated_delivery_id,im.correlation_header,im.from_matches_recipient,im.authentication::text,
		COALESCE((SELECT json_agg(x.address ORDER BY x.recipient_index) FROM inbound_recipients x WHERE x.inbound_id=im.id AND x.app_id=im.app_id),'[]'::json)
		FROM inbound_messages im `+inboundSummaryJoin+` WHERE im.app_id=$1 AND im.id=$2`, appID, inboundID)
	err = scanInboundSummary(row, &out.Message, &out.ContentExpiresAt, &mailFrom, &clientIP, &helo, &tlsVersion, &tlsCipher, &messageIDHeader,
		&corrDelivery, &corrHeader, &fromMatches, &document, &recipients)
	if errors.Is(err, sql.ErrNoRows) {
		return out, admin.ErrNotFound
	}
	if err != nil {
		return admin.InboundMessageDetail{}, err
	}
	if err = tx.Commit(); err != nil {
		return admin.InboundMessageDetail{}, err
	}
	if err = json.Unmarshal(recipients, &out.Envelope.Recipients); err != nil {
		return admin.InboundMessageDetail{}, err
	}
	out.Envelope.MailFrom, out.Envelope.ClientIP, out.Envelope.Helo = mailFrom.String, clientIP.String, helo.String
	out.Envelope.TLSVersion, out.Envelope.TLSCipher = tlsVersion.String, tlsCipher.String
	out.MessageIDHeader = messageIDHeader.String
	out.Authentication = storedInboundAuth(document, out.Message.SPF, out.Message.DKIM, out.Message.DMARC)
	if out.Message.Correlation.Matched {
		out.Correlation = admin.InboundCorrelation{Matched: true, MessageID: out.Message.Correlation.MessageID, DeliveryID: corrDelivery.String, Header: corrHeader.String}
		if fromMatches.Valid {
			out.Correlation.FromMatchesRecipient = &fromMatches.Bool
		}
	}
	return out, nil
}

// authorizeInboundRead is the single door to stored inbound content. In one
// transaction it finds the app's message, refuses it past retention (database
// clock) and, for Enhanced Protection, commits the audit record (action and ID
// only) before returning the object reference, so no object is fetched without
// it. The returned input has everything the renderer needs except the bytes.
func (r *Repository) AuthorizeInboundRead(ctx context.Context, actor, appID, inboundID, action string) (string, bool, format.InboundInput, error) {
	var in format.InboundInput
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(inboundID) || action != InboundBodyReadAction && action != InboundRawReadAction {
		return "", false, in, admin.ErrInvalidInput
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return "", false, in, err
	}
	defer tx.Rollback()
	var key string
	var protected, expired bool
	var mailFrom, clientIP, helo, tlsVersion, tlsCipher, receiver, document, corrMessage, corrDelivery, corrHeader sql.NullString
	var spf, dkim, dmarc string
	var fromMatches sql.NullBool
	var recipients []byte
	err = tx.QueryRowContext(ctx, `SELECT im.object_key,im.enhanced_protection,im.content_expires_at<=clock_timestamp(),im.received_at,
		im.mail_from,host(im.client_ip),im.helo,im.tls_version,im.tls_cipher,im.receiver,im.authentication::text,im.spf_result,im.dkim_result,im.dmarc_result,
		im.correlated_message_id,im.correlated_delivery_id,im.correlation_header,im.from_matches_recipient,
		COALESCE((SELECT json_agg(x.address ORDER BY x.recipient_index) FROM inbound_recipients x WHERE x.inbound_id=im.id AND x.app_id=im.app_id),'[]'::json)
		FROM inbound_messages im WHERE im.app_id=$1 AND im.id=$2`, appID, inboundID).Scan(&key, &protected, &expired, &in.ReceivedAt,
		&mailFrom, &clientIP, &helo, &tlsVersion, &tlsCipher, &receiver, &document, &spf, &dkim, &dmarc, &corrMessage, &corrDelivery, &corrHeader, &fromMatches, &recipients)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, in, admin.ErrNotFound
	}
	if err != nil {
		return "", false, in, err
	}
	if expired {
		// Nothing is read, so nothing is audited.
		return "", false, in, admin.ErrContentExpired
	}
	if protected {
		if err = recordAudit(ctx, tx, actor, appID, action, inboundID); err != nil {
			return "", false, in, err
		}
	}
	if err = tx.Commit(); err != nil {
		return "", false, in, err
	}
	if err = json.Unmarshal(recipients, &in.Recipients); err != nil {
		return "", false, in, err
	}
	in.ID = inboundID
	if len(in.Recipients) > 0 {
		in.Recipient = in.Recipients[0]
	}
	in.MailFrom, in.ClientIP, in.Helo, in.TLSVersion, in.TLSCipher, in.Receiver = mailFrom.String, clientIP.String, helo.String, tlsVersion.String, tlsCipher.String, receiver.String
	in.SPF, in.DKIM, in.DMARC = spf, dkim, dmarc
	if document.Valid {
		in.Authentication = json.RawMessage(document.String)
	}
	return key, protected, in, nil
}

// ListInboundWebhookDeliveries is the history of one received message: the same
// rows, cursor and audit record as the app-wide webhook history, narrowed to the
// events of this message.
func (r *Repository) ListInboundWebhookDeliveries(ctx context.Context, actor, appID, inboundID, cursorText string, limit int) (admin.WebhookDeliveryPage, error) {
	var page admin.WebhookDeliveryPage
	if !adminText(actor, 256) || !opaqueID(appID) || !opaqueID(inboundID) || limit < 1 || limit > 100 {
		return page, admin.ErrInvalidInput
	}
	cursor, err := parseWebhookCursor(cursorText)
	if err != nil {
		return page, err
	}
	tx, err := r.beginRuntime(ctx)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var found bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM inbound_messages WHERE app_id=$1 AND id=$2`, appID, inboundID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return page, admin.ErrNotFound
	}
	if err != nil {
		return page, err
	}
	if err = recordAudit(ctx, tx, actor, appID, "webhook.delivery.list", inboundID); err != nil {
		return page, err
	}
	var before any
	if !cursor.At.IsZero() {
		before = cursor.At
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.id,w.subscription_id,e.inbound_id,e.outcome,w.state,w.attempts,e.occurred_at,
		CASE WHEN w.state='queued' THEN w.next_attempt_at ELSE NULL END,w.delivered_at
		FROM webhook_events e JOIN webhook_deliveries w ON w.event_id=e.id AND w.app_id=e.app_id
		WHERE e.app_id=$1 AND e.inbound_id=$2
		AND ($3::timestamptz IS NULL OR (e.occurred_at,e.id,w.subscription_id)<($3,$4,$5))
		ORDER BY e.occurred_at DESC,e.id DESC,w.subscription_id DESC LIMIT $6`, appID, inboundID, before, cursor.EventID, cursor.SubscriptionID, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var item admin.WebhookDelivery
		var next, delivered sql.NullTime
		if err = rows.Scan(&item.EventID, &item.SubscriptionID, &item.InboundID, &item.Outcome, &item.State, &item.Attempts, &item.OccurredAt, &next, &delivered); err != nil {
			return admin.WebhookDeliveryPage{}, err
		}
		if next.Valid {
			item.NextAttemptAt = &next.Time
		}
		if delivered.Valid {
			item.DeliveredAt = &delivered.Time
		}
		page.Deliveries = append(page.Deliveries, item)
	}
	if err = rows.Err(); err != nil {
		return admin.WebhookDeliveryPage{}, err
	}
	rows.Close()
	if len(page.Deliveries) > limit {
		page.Deliveries = page.Deliveries[:limit]
		if page.NextCursor, err = encodeWebhookCursor(page.Deliveries[len(page.Deliveries)-1]); err != nil {
			return admin.WebhookDeliveryPage{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return admin.WebhookDeliveryPage{}, err
	}
	return page, nil
}
