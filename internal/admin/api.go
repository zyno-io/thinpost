// Package admin defines a fail-closed operator API. A real identity provider,
// authorization policy and runnable server must be wired before deployment.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zyno-io/thinpost/internal/customdomain"
	"github.com/zyno-io/thinpost/internal/providerid"
)

var ErrNotFound = errors.New("resource not found")
var ErrInvalidInput = errors.New("invalid input")
var ErrConflict = errors.New("resource conflict")

type Principal struct {
	Subject       string
	Administrator bool
}
type Authenticator interface {
	Authenticate(context.Context, string) (Principal, error)
}
type App struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	// SendingIPID is the app's sending IP; nil means the Edge's primary address.
	SendingIPID *string `json:"sending_ip_id"`
}
type Key struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}
type IssuedKey struct {
	Key    Key    `json:"key"`
	Secret string `json:"secret"`
}

// Sender is the recorded From address and decoded display name.
type Sender struct {
	Address string  `json:"address"`
	Name    *string `json:"name,omitempty"`
}

// MessageSummary never carries the subject of an Enhanced Protection message:
// the store withholds it in SQL and sets SubjectMasked instead.
type MessageSummary struct {
	ID                 string   `json:"id"`
	State              string   `json:"state"`
	Status             string   `json:"status"`
	EnhancedProtection bool     `json:"enhanced_protection"`
	Subject            *string  `json:"subject,omitempty"`
	SubjectMasked      bool     `json:"subject_masked"`
	SenderDomain       *string  `json:"sender_domain,omitempty"`
	Sender             *Sender  `json:"sender,omitempty"`
	Recipients         []string `json:"recipients,omitempty"`
	// RecipientKinds is parallel to Recipients: "to", "cc" or "bcc".
	RecipientKinds []string  `json:"recipient_kinds,omitempty"`
	RecipientCount *int      `json:"recipient_count,omitempty"`
	AcceptedAt     time.Time `json:"accepted_at"`
	DeliveryCount  int       `json:"delivery_count"`
	AcceptedCount  int       `json:"accepted_count"`
	FailedCount    int       `json:"failed_count"`
	UncertainCount int       `json:"uncertain_count"`
	// SuppressedCount is the failed deliveries dropped because the recipient
	// was on the suppression list; they are also counted in FailedCount.
	SuppressedCount int `json:"suppressed_count"`
	// OperatorTest is present exactly when an administrator submitted the
	// message through the console test send, and records which operator-only
	// options that send used.
	OperatorTest *OperatorTest `json:"operator_test,omitempty"`
	// SMTP is present exactly when the message was submitted over SMTP with
	// an app credential.
	SMTP *MessageSMTP `json:"smtp,omitempty"`
}

// MessageSMTP names the credential that submitted a message over SMTP and,
// when the app's From policy replaced the sender's From address, the address
// the client originally sent.
type MessageSMTP struct {
	CredentialID   string  `json:"credential_id"`
	CredentialName string  `json:"credential_name"`
	OriginalFrom   *string `json:"original_from,omitempty"`
}

// OperatorTest tags a console test message with the options it was sent with.
type OperatorTest struct {
	BypassLimits          bool `json:"bypass_limits"`
	BypassRecipientPolicy bool `json:"bypass_recipient_policy"`
}

// DeriveMessageStatus folds the preparation state and delivery counts into one
// overall status. pending counts queued and in-flight deliveries, held counts
// policy- and tracking-held ones, retrying counts pending ones that already
// have a completed attempt.
// DeliveryCounts are a message's deliveries by state for DeriveMessageStatus.
// Held is recipient_policy_held or tracking_held; RecoveryHeld is
// recovery_hold; Retrying counts pending deliveries with a completed SMTP
// attempt.
type DeliveryCounts struct {
	Total, Accepted, Failed, Uncertain, Pending, Held, RecoveryHeld, Retrying int
	// Suppressed counts failed deliveries dropped for a suppression; it is a
	// subset of Failed.
	Suppressed int
}

func DeriveMessageStatus(state string, c DeliveryCounts) string {
	switch state {
	case "queued":
		return "queued"
	case "preparing":
		return "preparing"
	case "preparation_failed":
		return "failed"
	case "recovery_hold":
		return "held"
	// A message submitted on hold waits for its app; the two terminal hold
	// states never produced a delivery.
	case "on_hold":
		return "on_hold"
	case "cancelled":
		return "cancelled"
	case "hold_expired":
		return "expired"
	case "prepared":
		switch {
		case c.Pending > 0 && c.Retrying > 0:
			return "retrying"
		case c.Pending > 0:
			return "sending"
		case c.RecoveryHeld > 0:
			return "held"
		case c.Uncertain > 0 || c.Held > 0:
			return "attention"
		case c.Total > 0 && c.Accepted == c.Total:
			return "delivered"
		case c.Total > 0 && c.Suppressed == c.Total:
			return "suppressed"
		case c.Accepted > 0:
			return "partially_delivered"
		case c.Failed > 0:
			return "failed"
		}
	}
	return "unknown"
}

// MessageSearch fields. Domain fields match the exact domain or a subdomain.
const (
	SearchFieldAny             = "any"
	SearchFieldID              = "id"
	SearchFieldRecipient       = "recipient"
	SearchFieldRecipientDomain = "recipient_domain"
	SearchFieldSenderDomain    = "sender_domain"
	SearchFieldSubject         = "subject"
)

// MessageQuery is a search request. Text is secret: it must never be logged,
// placed in an error string, an audit row or a URL.
type MessageQuery struct {
	Text   string
	Field  string
	Cursor string
	Limit  int
}

// ValidSearchField reports whether field is a supported search field.
func ValidSearchField(field string) bool {
	switch field {
	case SearchFieldAny, SearchFieldID, SearchFieldRecipient, SearchFieldRecipientDomain, SearchFieldSenderDomain, SearchFieldSubject:
		return true
	}
	return false
}

// ValidSearchText applies the shared bounds: trimmed, 1 to 254 bytes of valid
// UTF-8 and no control characters.
func ValidSearchText(q string) bool {
	if len(q) == 0 || len(q) > 254 || !utf8.ValidString(q) || strings.TrimSpace(q) != q {
		return false
	}
	for _, c := range q {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

type MessagePage struct {
	Messages   []MessageSummary `json:"messages"`
	NextCursor string           `json:"next_cursor,omitempty"`
}
type DeliverySummary struct {
	ID                   string     `json:"id"`
	RecipientIndex       int        `json:"recipient_index"`
	State                string     `json:"state"`
	RetryAttempts        int        `json:"retry_attempts"`
	NextAttemptAt        *time.Time `json:"next_attempt_at,omitempty"`
	LastCompletedOutcome *string    `json:"last_completed_outcome,omitempty"`
	LastSMTPCode         *int       `json:"last_smtp_code,omitempty"`
	// Recipient is the envelope address for RecipientIndex; absent for
	// messages accepted before recipients were recorded.
	Recipient *string `json:"recipient,omitempty"`
	// RecipientKind is "to", "cc" or "bcc"; absent with Recipient.
	RecipientKind *string `json:"recipient_kind,omitempty"`
	// Tracking is present only for a delivery with a tracking record.
	Tracking *DeliveryTracking `json:"tracking,omitempty"`
	// Feedback lists the recipient feedback recorded for this delivery (for
	// example a one-click unsubscribe), oldest first.
	Feedback []DeliveryFeedback `json:"feedback,omitempty"`
	// UnsubscribeForward is present once the recipient unsubscribed from a
	// message that had its own List-Unsubscribe headers (replaced by Thinpost's):
	// what became of the call to the sender's own link.
	UnsubscribeForward *UnsubscribeForward `json:"unsubscribe_forward,omitempty"`
	// MessageIDHeader is this delivery's RFC 5322 Message-ID with angle
	// brackets: the same for every delivery of a message prepared at schema 38
	// or later, per delivery for earlier ones. Absent when the sender domain
	// was not recorded.
	MessageIDHeader *string `json:"message_id_header,omitempty"`
}

// UnsubscribeForward is the state of the call to the sender's own unsubscribe
// link after a recipient unsubscribed through Thinpost's link. It never holds
// the link: only its host.
type UnsubscribeForward struct {
	// State is "queued" (waiting for an attempt, or retrying), "delivered" (the
	// sender's endpoint answered 2xx), "failed" (gave up) or "not_forwarded".
	State string `json:"state"`
	// Reason: for not_forwarded, "mailto_only", "no_one_click_post" or
	// "unusable_link"; for failed or a retrying forward, the last failure
	// ("blocked_destination", "resolution_failed", "connection_failed",
	// "tls_failed", "timeout", "http_status", "rejected", "invalid_link" or
	// "unknown"). Absent when delivered.
	Reason string `json:"reason,omitempty"`
	// Host is the DNS name of the sender's link; absent for not_forwarded.
	Host     string `json:"host,omitempty"`
	Attempts int    `json:"attempts"`
	// FinishedAt is when it was delivered or given up on.
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// DeliveryFeedback is one recorded unsubscribe or complaint. It holds no
// address, report or request content.
type DeliveryFeedback struct {
	Kind       string    `json:"kind"`
	Channel    string    `json:"channel"`
	ReceivedAt time.Time `json:"received_at"`
}

// DeliveryTracking is the recorded engagement of one delivery: events kept
// after the per-link and daily caps, so it can undercount, and it includes
// scanner and proxy fetches.
type DeliveryTracking struct {
	Opens       int        `json:"opens"`
	Clicks      int        `json:"clicks"`
	LastEventAt *time.Time `json:"last_event_at,omitempty"`
}
type MessageDetail struct {
	Message            MessageSummary      `json:"message"`
	Deliveries         []DeliverySummary   `json:"deliveries"`
	PreparationFailure *PreparationFailure `json:"preparation_failure,omitempty"`
	// Hold is present only for a message submitted on hold.
	Hold *MessageHold `json:"hold,omitempty"`
}

// MessageHold describes the hold of a message submitted on hold. Resolution
// and ResolvedAt are absent while it is held.
type MessageHold struct {
	ExpiresAt  time.Time  `json:"expires_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	// Resolution is HoldReleased, HoldCancelled or HoldExpired.
	Resolution string `json:"resolution,omitempty"`
}

// Hold resolutions reported in MessageHold.
const (
	HoldReleased  = "released"
	HoldCancelled = "cancelled"
	HoldExpired   = "expired"
)

// HoldConflict is the 409 reason when a release or cancel cannot apply. It
// matches ErrConflict with errors.Is.
type HoldConflict string

// Hold conflict reasons, sent as {"error": reason}.
const (
	HoldNotHeld      HoldConflict = "not_held"
	HoldWasReleased  HoldConflict = "released"
	HoldWasCancelled HoldConflict = "cancelled"
	HoldHasExpired   HoldConflict = "hold_expired"
	HoldInRecovery   HoldConflict = "recovery_hold"
)

func (c HoldConflict) Error() string        { return "hold conflict: " + string(c) }
func (c HoldConflict) Is(target error) bool { return target == ErrConflict }

type PreparationFailure struct {
	Attempts int    `json:"attempts"`
	Reason   string `json:"reason"`
}
type MessageBody struct {
	From               string               `json:"from"`
	To                 []string             `json:"to"`
	Cc                 []string             `json:"cc,omitempty"`
	Bcc                []string             `json:"bcc,omitempty"`
	ReplyTo            []string             `json:"reply_to,omitempty"`
	Subject            string               `json:"subject"`
	Text               string               `json:"text"`
	HTML               string               `json:"html"`
	EnhancedProtection bool                 `json:"enhanced_protection"`
	Attachments        []AttachmentMetadata `json:"attachments"`
	// Raw is set when the message was submitted as raw MIME over SMTP: the
	// contents above are parsed from the original, which can be downloaded
	// whole, and Attachments is always empty.
	Raw bool `json:"raw,omitempty"`
	// Omitted names what an inbound or raw MIME read could not show ("text", "html",
	// "headers"): a body over 1 MiB once decoded, an unknown charset or an
	// oversized header block. Absent for messages sent through the API.
	Omitted []string `json:"omitted,omitempty"`
}

// AttachmentMetadata describes a submitted attachment. ContentType is the
// submitted value and informational only; downloads never use it.
type AttachmentMetadata struct {
	Index       int    `json:"index"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	Inline      bool   `json:"inline"`
	ContentID   string `json:"content_id,omitempty"`
}

// AttachmentContent holds submitted attachment bytes; it is sensitive.
type AttachmentContent struct {
	Filename    string
	ContentType string
	Content     []byte
}

// HeaderField is one header field of the signed per-recipient MIME object.
type HeaderField struct {
	Name         string `json:"name"`
	Value        string `json:"value"`
	DecodedValue string `json:"decoded_value,omitempty"`
}
type Domain struct {
	Name       string     `json:"name"`
	Active     bool       `json:"active"`
	CreatedAt  time.Time  `json:"created_at"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	// ActiveKey signs the domain's mail. PendingKey is a key waiting for its
	// DNS record: the first key of a new domain, or the next key of a
	// rotation. RetiredKey is the previous key whose record should stay in DNS
	// for a while. A domain with none of them has no DKIM key yet.
	ActiveKey  *DKIMKey        `json:"active_key,omitempty"`
	PendingKey *DKIMKey        `json:"pending_key,omitempty"`
	RetiredKey *DKIMRetiredKey `json:"retired_key,omitempty"`
	// BounceDomain is the domain's own Return-Path domain, if it has one.
	// CNAMETarget is the name its CNAME must point at (the HELO name of the
	// app's sending IP), shown only once public DNS confirms the name;
	// CNAMETargetStatus says why there is none: ok, not_reported or
	// dns_unconfirmed. Both are set only on verified domains.
	BounceDomain      *BounceDomain `json:"bounce_domain,omitempty"`
	CNAMETarget       *string       `json:"cname_target,omitempty"`
	CNAMETargetStatus string        `json:"cname_target_status,omitempty"`
	// TrackingDomain is the domain's own tracking address, if it has one
	// (removed ones are not shown here).
	TrackingDomain *TrackingDomain `json:"tracking_domain,omitempty"`
}

// DKIMKey is a key Thinpost generated and the TXT record that publishes it.
// PublishedAt is set on a pending key once its record has been seen in DNS.
type DKIMKey struct {
	Selector    string     `json:"selector"`
	DNSName     string     `json:"dns_name"`
	DNSValue    string     `json:"dns_value"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	ActivatedAt *time.Time `json:"activated_at,omitempty"`
}

// DKIMRetiredKey is a previous key: its private half is gone, and its record
// stays useful until KeepRecordUntil for mail already queued.
type DKIMRetiredKey struct {
	Selector        string    `json:"selector"`
	KeepRecordUntil time.Time `json:"keep_record_until"`
}

// DKIMKeyRef names a pending key and its public half (SPKI DER), so the DNS
// check and the activation refer to the same key.
type DKIMKeyRef struct {
	Selector  string
	PublicKey []byte
}

// DomainChallenge is the DKIM TXT record to publish for a pending key.
type DomainChallenge struct {
	Domain   Domain `json:"domain"`
	DNSName  string `json:"dns_name"`
	DNSValue string `json:"dns_value"`
}
type TXTResolver interface {
	LookupTXT(context.Context, string) ([]string, error)
}

// ContentReader must authorize app ownership and, for Enhanced Protection
// messages, durably audit each read before returning any message contents.
// Implementations must not log content.
type ContentReader interface {
	ReadMessageBody(ctx context.Context, actor, appID, messageID string) (MessageBody, error)
	ReadAttachment(ctx context.Context, actor, appID, messageID string, index int) (AttachmentContent, error)
	// OpenMessageRaw returns the original bytes of a raw MIME message, audited
	// like a body read; ErrNotFound for any other message.
	OpenMessageRaw(ctx context.Context, actor, appID, messageID string) ([]byte, error)
	ReadDeliveryHeaders(ctx context.Context, actor, appID, messageID, deliveryID string) ([]HeaderField, error)
}

// PublicAuthConfig contains only public browser OIDC parameters, never a
// client secret. The SPA uses authorization code with PKCE.
type PublicAuthConfig struct {
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes"`
}

// Store mutations must persist the operator audit record in the same database
// transaction and never write a plaintext API key or token to audit records.
type Store interface {
	ListApps(context.Context) ([]App, error)
	CreateApp(context.Context, string, string) (App, error)
	ListKeys(context.Context, string) ([]Key, error)
	IssueKeyForOperator(context.Context, string, string, string) (IssuedKey, error)
	RevokeKeyForOperator(context.Context, string, string, string) error
	ListMessages(context.Context, string, string, string, int) (MessagePage, error)
	GetMessage(context.Context, string, string, string) (MessageDetail, error)
	SearchMessages(ctx context.Context, actor, appID string, query MessageQuery) (MessagePage, error)
	// RevealSubject returns the recorded subject. For Enhanced Protection it
	// audits the read (message ID only) before returning.
	RevealSubject(ctx context.Context, actor, appID, messageID string) (string, error)
	RetryPreparation(context.Context, string, string, string) error
	ListDomains(context.Context, string) ([]Domain, error)
	RequestDomain(context.Context, string, string, string) (DomainChallenge, error)
	// CreateDKIMKey makes the domain's next pending key (a first key for a
	// domain without one, or a rotation); ErrConflict when one is pending.
	CreateDKIMKey(ctx context.Context, actor, appID, domain string) (DomainChallenge, error)
	// PendingDKIMKey returns the pending key to check in DNS; an empty
	// selector means the domain's only pending key. ErrConflict when there is none.
	PendingDKIMKey(ctx context.Context, appID, domain, selector string) (DKIMKeyRef, error)
	// ActivateDKIMKey turns a pending key whose record was found in DNS into the
	// signing key. With no active key it also verifies the domain. With one,
	// switchActive=false only records that the record was seen, and true retires
	// (and shreds) the active key.
	ActivateDKIMKey(ctx context.Context, actor, appID, domain string, key DKIMKeyRef, switchActive bool) (Domain, error)
	// CancelDKIMKey deletes a pending key; ErrNotFound when it is not pending.
	CancelDKIMKey(ctx context.Context, actor, appID, domain, selector string) error
}
type Handler struct {
	Authenticator Authenticator
	Store         Store
	AuthConfig    *PublicAuthConfig
	ContentReader ContentReader
	// InboundContent serves content and raw reads of received messages.
	InboundContent InboundContentReader
	DNS            TXTResolver
	// ReverseDNS serves the advisory sending-IP DNS check; without it that
	// check is unavailable.
	ReverseDNS ReverseResolver
	// CNAME serves the bounce-domain check; without it that check is
	// unavailable.
	CNAME customdomain.Resolver
	// MailAuthDNS serves the advisory SPF and DMARC check of a sending domain;
	// without it that check is unavailable.
	MailAuthDNS MailAuthResolver
	// TrackingChecker serves the tracking-domain check; without it that check
	// is unavailable. Only replicas with tracking configured have one.
	TrackingChecker TrackingDomainChecker
	// AdminHost is the hostname of THINPOST_ADMIN_ORIGIN, which a tracking
	// domain may never use. Empty when not set.
	AdminHost      string
	Webhooks       WebhookAdmin
	WebhookHistory WebhookHistoryStore
	PolicyHolds    PolicyHoldStore
	TrackingHolds  TrackingHoldStore
	AppStatus      AppStatusStore
	// Apps renames and deletes apps and hides deleted apps from every app
	// route; without it those routes answer 503 and no app is hidden.
	Apps         AppStore
	Suppressions SuppressionStore
	Operations   OperationsStore
	// TestMessages submits operator test messages as an app. Without it the
	// test-send route answers 503.
	TestMessages TestMessageSubmitter
	// SigningReadiness reports whether mail from a domain would be DKIM-signed
	// now; nil when this deployment has no object keyring (no DKIM keys).
	SigningReadiness SigningReadiness
	// Runtime is the serving replica's startup configuration.
	Runtime *RuntimeInfo
	// MX checks a receiving domain's MX record against the inbound mail host,
	// which is the main IP's HELO name as the Edge reported it (EdgeIdentityStore)
	// once ReverseDNS confirms it.
	MX MXResolver
	// WebhookDestinations lists the operator's exact-match internal webhook
	// URLs (THINPOST_WEBHOOK_INTERNAL_DESTINATIONS); nil accepts only public
	// HTTPS endpoints.
	WebhookDestinations *WebhookDestinations
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.URL.Path == "/api/v1/config" {
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		if !ValidPublicAuth(h.AuthConfig) {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		respond(w, http.StatusOK, map[string]any{"oidc": h.AuthConfig})
		return
	}
	if h.Authenticator == nil || h.Store == nil {
		fail(w, http.StatusServiceUnavailable)
		return
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		fail(w, http.StatusUnauthorized)
		return
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 8192 {
		fail(w, http.StatusUnauthorized)
		return
	}
	principal, err := h.Authenticator.Authenticate(r.Context(), parts[1])
	if err != nil || !safeText(principal.Subject, 256) {
		fail(w, http.StatusUnauthorized)
		return
	}
	if !principal.Administrator {
		fail(w, http.StatusForbidden)
		return
	}
	path := strings.Split(r.URL.Path, "/")
	if h.operationsRoute(w, r, principal.Subject) || h.feedbackRoute(w, r, principal.Subject) || h.sendingIPRoute(w, r, principal.Subject) || h.trackingDomainsRoute(w, r) {
		return
	}
	if r.URL.Path == "/api/v1/apps" {
		switch r.Method {
		case http.MethodGet:
			apps, err := h.Store.ListApps(r.Context())
			if err != nil {
				storeError(w, err)
				return
			}
			if apps == nil {
				apps = []App{}
			}
			respond(w, http.StatusOK, map[string]any{"apps": apps})
		case http.MethodPost:
			var req struct {
				Name        string  `json:"name"`
				SendingIPID *string `json:"sending_ip_id"`
			}
			if decode(r, &req) != nil || !safeText(req.Name, 128) || req.SendingIPID != nil && !opaqueID(*req.SendingIPID) {
				fail(w, http.StatusBadRequest)
				return
			}
			var app App
			var err error
			if req.SendingIPID != nil {
				ips, ok := h.Store.(SendingIPStore)
				if !ok {
					fail(w, http.StatusServiceUnavailable)
					return
				}
				app, err = ips.CreateAppWithSendingIP(r.Context(), principal.Subject, req.Name, req.SendingIPID)
			} else {
				app, err = h.Store.CreateApp(r.Context(), principal.Subject, req.Name)
			}
			if err != nil {
				storeError(w, err)
				return
			}
			respond(w, http.StatusCreated, app)
		default:
			method(w, "GET, POST")
		}
		return
	}
	if len(path) < 5 || path[1] != "api" || path[2] != "v1" || path[3] != "apps" || !opaqueID(path[4]) {
		http.NotFound(w, r)
		return
	}
	appID := path[4]
	// The one place every app-scoped route passes: a deleted app is unknown.
	if h.Apps != nil {
		exists, err := h.Apps.AppExists(r.Context(), appID)
		if err != nil {
			storeError(w, err)
			return
		}
		if !exists {
			fail(w, http.StatusNotFound)
			return
		}
	}
	if len(path) == 5 {
		h.appRoute(w, r, principal.Subject, appID)
		return
	}
	// Message routes accept any provider rendering of the message ID (UUID text,
	// "id@domain", "<id@domain>") and use the 32-hex form from here on.
	if len(path) > 6 && path[5] == "messages" {
		if id, ok := providerid.Normalize(path[6]); ok {
			path[6] = id
		}
	}
	if h.inboundRoute(w, r, principal.Subject, appID, path) || h.smtpRoute(w, r, principal.Subject, appID, path) {
		return
	}
	if len(path) == 6 && (path[5] == "activate" || path[5] == "deactivate") {
		if r.Method != http.MethodPost {
			method(w, "POST")
			return
		}
		if r.URL.RawQuery != "" || r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			fail(w, http.StatusBadRequest)
			return
		}
		if h.AppStatus == nil {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		if err := h.AppStatus.SetAppActive(r.Context(), principal.Subject, appID, path[5] == "activate"); err != nil {
			storeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(path) == 6 && path[5] == "sending-ip" {
		h.appSendingIP(w, r, principal.Subject, appID)
		return
	}
	if len(path) == 6 && path[5] == "recipient-quota" {
		h.recipientQuota(w, r, principal.Subject, appID)
		return
	}
	if len(path) == 6 && path[5] == "pending-quota" {
		h.pendingQuota(w, r, principal.Subject, appID)
		return
	}
	if len(path) == 6 && path[5] == "submission-rate" {
		h.submissionRate(w, r, principal.Subject, appID)
		return
	}
	if len(path) == 6 && path[5] == "list-unsubscribe" {
		h.listUnsubscribe(w, r, principal.Subject, appID)
		return
	}
	if len(path) == 6 && path[5] == "tracking-defaults" {
		h.trackingDefaults(w, r, principal.Subject, appID)
		return
	}
	if len(path) == 6 && path[5] == "tracking-quota" {
		h.trackingQuota(w, r, principal.Subject, appID)
		return
	}
	if len(path) == 6 && path[5] == "suppressions" {
		h.suppressions(w, r, principal.Subject, appID)
		return
	}
	if len(path) == 7 && path[5] == "suppressions" && path[6] == "search" {
		h.suppressionSearch(w, r, appID)
		return
	}
	if len(path) == 7 && path[5] == "suppressions" && path[6] == "delete" {
		h.suppressionDelete(w, r, principal.Subject, appID)
		return
	}
	if len(path) == 6 && path[5] == "webhooks" {
		h.webhooks(w, r, principal.Subject, appID)
		return
	}
	if (len(path) == 7 || len(path) == 8 && path[7] != "") && path[5] == "webhooks" && opaqueID(path[6]) {
		action := ""
		if len(path) == 8 {
			action = path[7]
		}
		h.webhook(w, r, principal.Subject, appID, path[6], action)
		return
	}
	if len(path) == 6 && path[5] == "webhook-deliveries" {
		h.webhookDeliveries(w, r, principal.Subject, appID)
		return
	}
	if len(path) == 9 && path[5] == "webhook-deliveries" && opaqueID(path[6]) && opaqueID(path[7]) {
		h.webhookDeliveryAction(w, r, principal.Subject, appID, path[6], path[7], path[8])
		return
	}
	if len(path) == 6 && path[5] == "domains" {
		switch r.Method {
		case http.MethodGet:
			domains, err := h.Store.ListDomains(r.Context(), appID)
			if err != nil {
				storeError(w, err)
				return
			}
			if domains == nil {
				domains = []Domain{}
			}
			if err = h.attachBounceDomains(r.Context(), appID, domains); err != nil {
				storeError(w, err)
				return
			}
			if err = h.attachTrackingDomains(r.Context(), appID, domains); err != nil {
				storeError(w, err)
				return
			}
			respond(w, http.StatusOK, map[string]any{"domains": domains})
		case http.MethodPost:
			var req struct {
				Domain string `json:"domain"`
			}
			if decode(r, &req) != nil {
				fail(w, http.StatusBadRequest)
				return
			}
			domain, ok := NormalizeDomain(req.Domain)
			if !ok {
				fail(w, http.StatusBadRequest)
				return
			}
			challenge, err := h.Store.RequestDomain(r.Context(), principal.Subject, appID, domain)
			if err != nil {
				storeError(w, err)
				return
			}
			respond(w, http.StatusCreated, challenge)
		default:
			method(w, "GET, POST")
		}
		return
	}
	if len(path) >= 8 && len(path) <= 10 && path[5] == "domains" {
		if h.bounceDomainRoute(w, r, principal.Subject, appID, path[6:]) || h.trackingDomainRoute(w, r, principal.Subject, appID, path[6:]) || h.domainKeyRoute(w, r, principal.Subject, appID, path[6:]) || h.senderDomainDNSRoute(w, r, appID, path[6:]) {
			return
		}
	}
	if len(path) == 10 && path[5] == "messages" && opaqueID(path[6]) && path[7] == "deliveries" && opaqueID(path[8]) && path[9] == "retry-policy-hold" {
		if r.Method != http.MethodPost {
			method(w, "POST")
			return
		}
		if r.URL.RawQuery != "" || r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			fail(w, http.StatusBadRequest)
			return
		}
		if h.PolicyHolds == nil {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		if err := h.PolicyHolds.RetryPolicyHold(r.Context(), principal.Subject, appID, path[6], path[8]); err != nil {
			storeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(path) == 10 && path[5] == "messages" && opaqueID(path[6]) && path[7] == "deliveries" && opaqueID(path[8]) && path[9] == "abandon-tracking-hold" {
		if r.Method != http.MethodPost {
			method(w, "POST")
			return
		}
		if r.URL.RawQuery != "" || r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
			fail(w, http.StatusBadRequest)
			return
		}
		if h.TrackingHolds == nil {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		if err := h.TrackingHolds.AbandonTrackingHold(r.Context(), principal.Subject, appID, path[6], path[8]); err != nil {
			storeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(path) == 8 && path[5] == "messages" && opaqueID(path[6]) && path[7] == "retry-preparation" {
		if r.Method != http.MethodPost {
			method(w, "POST")
			return
		}
		if r.URL.RawQuery != "" {
			fail(w, http.StatusBadRequest)
			return
		}
		if err := h.Store.RetryPreparation(r.Context(), principal.Subject, appID, path[6]); err != nil {
			storeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(path) == 8 && path[5] == "messages" && opaqueID(path[6]) && path[7] == "body" {
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		if r.URL.RawQuery != "" {
			fail(w, http.StatusBadRequest)
			return
		}
		if h.ContentReader == nil {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		body, err := h.ContentReader.ReadMessageBody(r.Context(), principal.Subject, appID, path[6])
		if err != nil {
			storeError(w, err)
			return
		}
		if body.Attachments == nil {
			body.Attachments = []AttachmentMetadata{}
		}
		respond(w, http.StatusOK, body)
		return
	}
	if len(path) == 9 && path[5] == "messages" && opaqueID(path[6]) && path[7] == "attachments" {
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		if r.URL.RawQuery != "" {
			fail(w, http.StatusBadRequest)
			return
		}
		index, ok := attachmentIndex(path[8])
		if !ok {
			fail(w, http.StatusNotFound)
			return
		}
		if h.ContentReader == nil {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		attachment, err := h.ContentReader.ReadAttachment(r.Context(), principal.Subject, appID, path[6], index)
		if err != nil {
			storeError(w, err)
			return
		}
		writeAttachment(w, index, attachment)
		return
	}
	if len(path) == 8 && path[5] == "messages" && opaqueID(path[6]) && path[7] == "raw" {
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		if r.URL.RawQuery != "" {
			fail(w, http.StatusBadRequest)
			return
		}
		if h.ContentReader == nil {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		raw, err := h.ContentReader.OpenMessageRaw(r.Context(), principal.Subject, appID, path[6])
		if err != nil {
			storeError(w, err)
			return
		}
		writeRaw(w, path[6], raw)
		return
	}
	if len(path) == 10 && path[5] == "messages" && opaqueID(path[6]) && path[7] == "deliveries" && opaqueID(path[8]) && path[9] == "headers" {
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		if r.URL.RawQuery != "" {
			fail(w, http.StatusBadRequest)
			return
		}
		if h.ContentReader == nil {
			fail(w, http.StatusServiceUnavailable)
			return
		}
		headers, err := h.ContentReader.ReadDeliveryHeaders(r.Context(), principal.Subject, appID, path[6], path[8])
		if err != nil {
			storeError(w, err)
			return
		}
		if headers == nil {
			headers = []HeaderField{}
		}
		respond(w, http.StatusOK, map[string]any{"headers": headers})
		return
	}
	if len(path) == 7 && path[5] == "messages" && path[6] == "test" {
		h.testMessage(w, r, principal.Subject, appID)
		return
	}
	if len(path) == 7 && path[5] == "messages" && path[6] == "search" {
		if r.Method != http.MethodPost {
			method(w, "POST")
			return
		}
		// Search terms are accepted only in the body so that they never reach
		// access logs; the query string is rejected outright.
		if r.URL.RawQuery != "" {
			fail(w, http.StatusBadRequest)
			return
		}
		var req struct {
			Q      string `json:"q"`
			Field  string `json:"field"`
			Cursor string `json:"cursor"`
			Limit  *int   `json:"limit"`
		}
		if decode(r, &req) != nil {
			fail(w, http.StatusBadRequest)
			return
		}
		if req.Field == "" {
			req.Field = SearchFieldAny
		}
		limit := 50
		if req.Limit != nil {
			limit = *req.Limit
		}
		if !ValidSearchText(req.Q) || !ValidSearchField(req.Field) || limit < 1 || limit > 100 || len(req.Cursor) > 512 {
			fail(w, http.StatusBadRequest)
			return
		}
		page, err := h.Store.SearchMessages(r.Context(), principal.Subject, appID, MessageQuery{Text: req.Q, Field: req.Field, Cursor: req.Cursor, Limit: limit})
		if err != nil {
			storeError(w, err)
			return
		}
		if page.Messages == nil {
			page.Messages = []MessageSummary{}
		}
		respond(w, http.StatusOK, page)
		return
	}
	if len(path) == 8 && path[5] == "messages" && opaqueID(path[6]) && path[7] == "subject" {
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		if r.URL.RawQuery != "" {
			fail(w, http.StatusBadRequest)
			return
		}
		subject, err := h.Store.RevealSubject(r.Context(), principal.Subject, appID, path[6])
		if err != nil {
			storeError(w, err)
			return
		}
		respond(w, http.StatusOK, map[string]string{"subject": subject})
		return
	}
	if len(path) == 7 && path[5] == "messages" && opaqueID(path[6]) {
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		if r.URL.RawQuery != "" {
			fail(w, http.StatusBadRequest)
			return
		}
		detail, err := h.Store.GetMessage(r.Context(), principal.Subject, appID, path[6])
		if err != nil {
			storeError(w, err)
			return
		}
		if detail.Deliveries == nil {
			detail.Deliveries = []DeliverySummary{}
		}
		respond(w, http.StatusOK, detail)
		return
	}
	if len(path) == 6 && path[5] == "messages" {
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		query := r.URL.Query()
		for key, values := range query {
			if (key != "limit" && key != "cursor") || len(values) != 1 {
				fail(w, http.StatusBadRequest)
				return
			}
		}
		limit := 50
		if value := query.Get("limit"); value != "" {
			var parseErr error
			limit, parseErr = strconv.Atoi(value)
			if parseErr != nil || limit < 1 || limit > 100 {
				fail(w, http.StatusBadRequest)
				return
			}
		}
		cursor := query.Get("cursor")
		if len(cursor) > 512 {
			fail(w, http.StatusBadRequest)
			return
		}
		page, err := h.Store.ListMessages(r.Context(), principal.Subject, appID, cursor, limit)
		if err != nil {
			storeError(w, err)
			return
		}
		if page.Messages == nil {
			page.Messages = []MessageSummary{}
		}
		respond(w, http.StatusOK, page)
		return
	}
	if path[5] != "keys" {
		http.NotFound(w, r)
		return
	}
	if len(path) == 6 {
		switch r.Method {
		case http.MethodGet:
			keys, err := h.Store.ListKeys(r.Context(), appID)
			if err != nil {
				storeError(w, err)
				return
			}
			if keys == nil {
				keys = []Key{}
			}
			respond(w, http.StatusOK, map[string]any{"keys": keys})
		case http.MethodPost:
			var req struct {
				Name string `json:"name"`
			}
			if decode(r, &req) != nil || !safeText(req.Name, 128) {
				fail(w, http.StatusBadRequest)
				return
			}
			key, err := h.Store.IssueKeyForOperator(r.Context(), principal.Subject, appID, req.Name)
			if err != nil {
				storeError(w, err)
				return
			}
			respond(w, http.StatusCreated, key)
		default:
			method(w, "GET, POST")
		}
		return
	}
	if len(path) == 7 && opaqueID(path[6]) {
		if r.Method != http.MethodDelete {
			method(w, "DELETE")
			return
		}
		if err = h.Store.RevokeKeyForOperator(r.Context(), principal.Subject, appID, path[6]); err != nil {
			storeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.NotFound(w, r)
}

// ValidPublicAuth reports whether the public browser configuration is safe to expose.
func ValidPublicAuth(config *PublicAuthConfig) bool {
	if config == nil || !safeText(config.ClientID, 256) || len(config.Scopes) == 0 || len(config.Scopes) > 16 {
		return false
	}
	u, err := url.Parse(config.Issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	hasOpenID := false
	for _, scope := range config.Scopes {
		if !safeText(scope, 256) || strings.ContainsAny(scope, " \t") {
			return false
		}
		if scope == "openid" {
			hasOpenID = true
		}
	}
	return hasOpenID
}

// NormalizeDomain accepts only ordinary ASCII DNS domain names; IDNs must be
// provided as punycode. It does not itself prove control of a domain.
func NormalizeDomain(value string) (string, bool) {
	value = strings.ToLower(value)
	if len(value) > 253 || len(value) == 0 || strings.Count(value, ".") < 1 {
		return "", false
	}
	if _, err := netip.ParseAddr(value); err == nil {
		return "", false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", false
			}
		}
	}
	return value, true
}
func safeText(value string, max int) bool {
	if len(value) == 0 || len(value) > max || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}
func opaqueID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func decode(r *http.Request, dest any) error {
	return decodeLimit(r, dest, 16384)
}

// decodeLimit is decode with a larger body cap for routes whose contract needs it.
func decodeLimit(r *http.Request, dest any, limit int) error {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil || len(data) > limit || !utf8.Valid(data) {
		return ErrInvalidInput
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(dest); err != nil {
		return err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return ErrInvalidInput
	}
	return nil
}

// MaxAttachmentIndex is the highest valid attachment index (20 attachments).
const MaxAttachmentIndex = 19

// attachmentIndex accepts only canonical decimal 0 through 19, without sign,
// leading zeros or whitespace.
func attachmentIndex(value string) (int, bool) {
	if len(value) == 0 || len(value) > 2 || (len(value) == 2 && value[0] == '0') {
		return 0, false
	}
	index := 0
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, false
		}
		index = index*10 + int(c-'0')
	}
	return index, index <= MaxAttachmentIndex
}

// writeAttachment sends submitted bytes as an opaque download. The submitted
// content type is never used: the response is always application/octet-stream
// with nosniff, a sandboxing CSP and same-origin resource policy so a browser
// cannot render attachment content in the console's origin.
func writeAttachment(w http.ResponseWriter, index int, attachment AttachmentContent) {
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": attachment.Filename})
	if disposition == "" {
		disposition = "attachment; filename=\"attachment-" + strconv.Itoa(index) + "\""
	}
	header := w.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Content-Disposition", disposition)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Content-Length", strconv.Itoa(len(attachment.Content)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(attachment.Content)
}

// writeRaw sends the original message as an opaque download, never rendered.
func writeRaw(w http.ResponseWriter, messageID string, raw []byte) {
	header := w.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Content-Disposition", "attachment; filename=\"message-"+messageID+".eml\"")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func method(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	fail(w, http.StatusMethodNotAllowed)
}
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, http.StatusNotFound)
	case errors.Is(err, ErrInvalidInput):
		fail(w, http.StatusBadRequest)
	case errors.Is(err, ErrConflict):
		fail(w, http.StatusConflict)
	default:
		fail(w, http.StatusServiceUnavailable)
	}
}
func fail(w http.ResponseWriter, status int) {
	respond(w, status, map[string]string{"error": http.StatusText(status)})
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
