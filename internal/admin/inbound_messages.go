package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

// ErrContentExpired is returned by a content read for an inbound message that is
// past its retention (content_expires_at, by the database clock) or whose stored
// object is already gone. The API answers 409 with a constant body.
var ErrContentExpired = errors.New("inbound content expired")

// Inbound webhook delivery summary states.
const (
	InboundWebhookNone      = "none"      // no delivery was recorded
	InboundWebhookPending   = "pending"   // at least one is queued, in flight or held
	InboundWebhookDelivered = "delivered" // every delivery succeeded
	InboundWebhookFailed    = "failed"    // none succeeded and at least one failed
	InboundWebhookPartial   = "partial"   // some succeeded and some failed
)

// DeriveInboundWebhookStatus folds the delivery counts of one message into one
// state. Cancelled deliveries are not counted.
func DeriveInboundWebhookStatus(delivered, pending, failed int) string {
	switch {
	case pending > 0:
		return InboundWebhookPending
	case failed > 0 && delivered > 0:
		return InboundWebhookPartial
	case failed > 0:
		return InboundWebhookFailed
	case delivered > 0:
		return InboundWebhookDelivered
	}
	return InboundWebhookNone
}

// InboundWebhookSummary counts the inbound-lane webhook deliveries of one
// message (one per envelope recipient and enabled endpoint).
type InboundWebhookSummary struct {
	Status    string `json:"status"`
	Total     int    `json:"total"`
	Delivered int    `json:"delivered"`
	Pending   int    `json:"pending"`
	Failed    int    `json:"failed"`
}

// InboundCorrelationSummary is the list view of the reply link; the detail has
// more. Matched is a claim made by the message's headers, not authentication.
type InboundCorrelationSummary struct {
	Matched   bool   `json:"matched"`
	MessageID string `json:"message_id,omitempty"`
}

// InboundMessageSummary is one row of the list. Subject is withheld in SQL for
// an Enhanced Protection message (and is also absent when none was recorded).
// From holds only the recorded address: the display name lives in the stored
// message and is shown by the content read.
type InboundMessageSummary struct {
	ID                 string                    `json:"id"`
	ReceivedAt         time.Time                 `json:"received_at"`
	From               *Sender                   `json:"from,omitempty"`
	Recipient          string                    `json:"recipient,omitempty"`
	RecipientCount     int                       `json:"recipient_count"`
	Subject            *string                   `json:"subject,omitempty"`
	EnhancedProtection bool                      `json:"enhanced_protection"`
	SizeBytes          int                       `json:"size_bytes"`
	SPF                string                    `json:"spf"`
	DKIM               string                    `json:"dkim"`
	DMARC              string                    `json:"dmarc"`
	Correlation        InboundCorrelationSummary `json:"correlation"`
	Webhook            InboundWebhookSummary     `json:"webhook"`
	// ContentExpired is judged by the database clock against content_expires_at.
	ContentExpired bool `json:"content_expired"`
}

type InboundMessagePage struct {
	Messages   []InboundMessageSummary `json:"messages"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

// InboundEnvelope is what the sending server said and how it connected. MailFrom
// is absent for the null reverse path.
type InboundEnvelope struct {
	MailFrom   string   `json:"mail_from,omitempty"`
	Recipients []string `json:"recipients"`
	ClientIP   string   `json:"client_ip,omitempty"`
	Helo       string   `json:"helo,omitempty"`
	TLSVersion string   `json:"tls_version,omitempty"`
	TLSCipher  string   `json:"tls_cipher,omitempty"`
}

// InboundAuthMethod is one authentication result with the domain it was
// evaluated for, when recorded.
type InboundAuthMethod struct {
	Result   string `json:"result"`
	Domain   string `json:"domain,omitempty"`
	Identity string `json:"identity,omitempty"` // SPF only: mailfrom or helo
	Policy   string `json:"policy,omitempty"`   // DMARC only
}

// InboundARC is never evaluated; Present says whether an ARC-Seal was seen.
type InboundARC struct {
	Present   bool `json:"present"`
	Evaluated bool `json:"evaluated"`
}

type InboundAuthentication struct {
	SPF   InboundAuthMethod   `json:"spf"`
	DKIM  []InboundAuthMethod `json:"dkim"`
	DMARC InboundAuthMethod   `json:"dmarc"`
	ARC   InboundARC          `json:"arc"`
}

// InboundCorrelation is the full reply link. DeliveryID is absent for a
// message-level match; FromMatchesRecipient is absent when undetermined.
type InboundCorrelation struct {
	Matched              bool   `json:"matched"`
	MessageID            string `json:"message_id,omitempty"`
	DeliveryID           string `json:"delivery_id,omitempty"`
	Header               string `json:"header,omitempty"`
	FromMatchesRecipient *bool  `json:"from_matches_recipient,omitempty"`
}

// InboundMessageDetail is metadata only: no body and no object key. The header
// fields are the ones recorded with the message; the subject of an Enhanced
// Protection message is withheld.
type InboundMessageDetail struct {
	Message          InboundMessageSummary `json:"message"`
	ContentExpiresAt time.Time             `json:"content_expires_at"`
	Envelope         InboundEnvelope       `json:"envelope"`
	MessageIDHeader  string                `json:"message_id_header,omitempty"`
	Authentication   InboundAuthentication `json:"authentication"`
	Correlation      InboundCorrelation    `json:"correlation"`
}

// InboundMessageStore reads received messages. Reads are scoped to the app: an
// ID of another app is ErrNotFound. Nothing here returns content.
type InboundMessageStore interface {
	ListInboundMessages(ctx context.Context, actor, appID, cursor string, limit int) (InboundMessagePage, error)
	GetInboundMessage(ctx context.Context, actor, appID, inboundID string) (InboundMessageDetail, error)
	// ListInboundWebhookDeliveries is the webhook history of one message, with
	// the same rows and audit record as the app-wide history.
	ListInboundWebhookDeliveries(ctx context.Context, actor, appID, inboundID, cursor string, limit int) (WebhookDeliveryPage, error)
}

// InboundRaw is an opened raw download: the size is known before the first byte
// so the handler can send the headers, and WriteTo streams the message.
type InboundRaw interface {
	Size() int64
	WriteTo(io.Writer) (int64, error)
}

// InboundContentReader authorizes, and for Enhanced Protection durably audits,
// each read before any object is fetched, and refuses expired content with
// ErrContentExpired. Implementations must not log content.
type InboundContentReader interface {
	ReadInboundContent(ctx context.Context, actor, appID, inboundID string) (MessageBody, error)
	OpenInboundRaw(ctx context.Context, actor, appID, inboundID string) (InboundRaw, error)
}

// inboundMessageRoute serves inbound-messages under an app. Paths are
// /api/v1/apps/{appId}/inbound-messages[/{id}[/content|raw|webhook-deliveries]].
func (h *Handler) inboundMessageRoute(w http.ResponseWriter, r *http.Request, actor, appID string, path []string) {
	if len(path) > 8 || len(path) == 8 && path[7] != "content" && path[7] != "raw" && path[7] != "webhook-deliveries" || len(path) >= 7 && !opaqueID(path[6]) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		method(w, "GET")
		return
	}
	switch {
	case len(path) == 6:
		h.inboundMessageList(w, r, actor, appID)
	case len(path) == 7:
		if r.URL.RawQuery != "" {
			fail(w, http.StatusBadRequest)
			return
		}
		store, ok := h.Store.(InboundMessageStore)
		if !ok {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		detail, err := store.GetInboundMessage(r.Context(), actor, appID, path[6])
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, normalizeInboundDetail(detail))
	case path[7] == "webhook-deliveries":
		h.inboundWebhookDeliveries(w, r, actor, appID, path[6])
	default:
		h.inboundContent(w, r, actor, appID, path[6], path[7] == "raw")
	}
}

func normalizeInboundDetail(d InboundMessageDetail) InboundMessageDetail {
	if d.Envelope.Recipients == nil {
		d.Envelope.Recipients = []string{}
	}
	if d.Authentication.DKIM == nil {
		d.Authentication.DKIM = []InboundAuthMethod{}
	}
	return d
}

func (h *Handler) inboundMessageList(w http.ResponseWriter, r *http.Request, actor, appID string) {
	// No search and no filters: only paging keys are accepted, so no address or
	// term can reach an access log through the query string.
	cursor, limit, err := inboundPageQuery(r)
	if err != nil {
		fail(w, http.StatusBadRequest)
		return
	}
	store, ok := h.Store.(InboundMessageStore)
	if !ok {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	page, err := store.ListInboundMessages(r.Context(), actor, appID, cursor, limit)
	if err != nil {
		storeError(w, err)
		return
	}
	if page.Messages == nil {
		page.Messages = []InboundMessageSummary{}
	}
	respond(w, http.StatusOK, page)
}

func inboundPageQuery(r *http.Request) (string, int, error) {
	// The same strict parser as the webhook history: a query string that does not
	// parse (a bad escape, a semicolon) is rejected, never half-read.
	return webhookPageQuery(r.URL.RawQuery)
}

func (h *Handler) inboundWebhookDeliveries(w http.ResponseWriter, r *http.Request, actor, appID, inboundID string) {
	cursor, limit, err := webhookPageQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, http.StatusBadRequest)
		return
	}
	store, ok := h.Store.(InboundMessageStore)
	if !ok {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	page, err := store.ListInboundWebhookDeliveries(r.Context(), actor, appID, inboundID, cursor, limit)
	if err != nil {
		storeError(w, err)
		return
	}
	if page.Deliveries == nil {
		page.Deliveries = []WebhookDelivery{}
	}
	respond(w, http.StatusOK, page)
}

// inboundContent serves the decoded content or, with raw, the .eml download.
// The raw route sets its headers before the first byte, so an error is always a
// normal JSON answer and a streaming failure can only truncate the download.
func (h *Handler) inboundContent(w http.ResponseWriter, r *http.Request, actor, appID, inboundID string, raw bool) {
	if r.URL.RawQuery != "" {
		fail(w, http.StatusBadRequest)
		return
	}
	if h.InboundContent == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	if !raw {
		body, err := h.InboundContent.ReadInboundContent(r.Context(), actor, appID, inboundID)
		if err != nil {
			inboundContentError(w, err)
			return
		}
		if body.Attachments == nil {
			body.Attachments = []AttachmentMetadata{}
		}
		respond(w, http.StatusOK, body)
		return
	}
	file, err := h.InboundContent.OpenInboundRaw(r.Context(), actor, appID, inboundID)
	if err != nil {
		inboundContentError(w, err)
		return
	}
	header := w.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Content-Disposition", `attachment; filename="inbound-`+inboundID+`.eml"`)
	header.Set("Cache-Control", "no-store")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Content-Length", strconv.FormatInt(file.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = file.WriteTo(w)
}

func inboundContentError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrContentExpired) {
		respond(w, http.StatusConflict, map[string]string{"error": "content_expired"})
		return
	}
	storeError(w, err)
}
