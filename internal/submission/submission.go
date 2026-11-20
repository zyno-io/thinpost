// Package submission defines the boundary between HTTP adapters and durable
// message acceptance. It does not itself store or deliver messages.
package submission

import (
	"context"
	"errors"
	"mime"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalidKey          = errors.New("invalid API key")
	ErrInvalidMessage      = errors.New("invalid message")
	ErrUnavailable         = errors.New("submission unavailable")
	ErrIdempotencyConflict = errors.New("idempotency key conflicts with an earlier request")
	ErrQuotaExceeded       = errors.New("application recipient quota exceeded")
	ErrRateLimited         = errors.New("application submission rate exceeded")
	ErrBacklogExceeded     = errors.New("application pending message limit exceeded")
	// ErrAppPaused reports that the application is inactive. It is returned
	// for operator and SMTP submissions, whose principals are not credentials
	// that an inactive application would already have invalidated (an API key
	// is, so a paused app reads as an invalid key to a key principal). The batch
	// adapters never see it.
	ErrAppPaused = errors.New("application is paused")
	// ErrSenderDomainInactive reports that the sender domain stopped being an
	// active domain of the app between the policy decision and the commit. It
	// is returned on the raw (SMTP) path only; HTTP reports ErrInvalidMessage.
	ErrSenderDomainInactive = errors.New("sender domain is not active")
	// ErrMessageTooLarge reports a raw message whose prepared form would not
	// fit the delivery limits. It is returned on the raw (SMTP) path only.
	ErrMessageTooLarge = errors.New("message too large")
	// ErrRecipientNotPermitted reports a raw message with a recipient outside
	// the operator's allowlist; the whole message is refused. It is returned on
	// the raw (SMTP) path only: HTTP reports the same condition as
	// ErrInvalidMessage.
	ErrRecipientNotPermitted = errors.New("recipient not permitted")
	// ErrInvalidRequest reports a request that could not be decoded, or that
	// is outside the profile of its route. It is never a policy refusal: the
	// message was not submitted to acceptance.
	ErrInvalidRequest = errors.New("invalid request")
)

// Adapter names recorded with a submission. AdapterNative is Thinpost's own
// JSON API. AdapterConsole is reserved for operator submissions and is the
// only adapter an operator principal may use. AdapterSMTP is reserved for SMTP
// submission and is the only adapter an SMTP principal may use.
const (
	AdapterNative  = "thinpost"
	AdapterConsole = "console"
	AdapterSMTP    = "smtp"
)

// Fence identifies the control session lease an SMTP submission was accepted
// under. The commit of an SMTP message is valid only while that exact lease
// epoch is live (see postgres.Repository.Commit). It lives here, not in the
// session package, to keep acceptance free of an import cycle.
type Fence struct {
	WorkerID, ControllerID string
	Epoch                  uint64
}

// App identifies the authenticated application. It must come from an
// authoritative credential lookup, never from the message payload.
// ListUnsubscribe is the app's Thinpost one-click unsubscribe opt-in as read at
// authentication. DefaultTrackOpens and DefaultTrackClicks are the app's
// tracking defaults as read at authentication; only acceptance applies them.
//
// Exactly one of KeyID, Operator and SMTPCredentialID identifies the principal.
// KeyID is set by authenticating an API key. Operator is the subject of an
// authenticated administrator and is set only by
// OperatorService.AuthorizeOperator; it selects the operator submission path
// (adapter AdapterConsole, no idempotency key). SMTPCredentialID is a verified
// SMTP submission credential with SMTPCredentialDigest, the SHA-256 of its
// secret, which the repository rechecks under a lock; it selects the raw path
// (adapter AdapterSMTP). Nothing else may set Operator or SMTPCredentialID.
type App struct {
	ID, KeyID                             string
	Operator                              string
	SMTPCredentialID                      string
	SMTPCredentialDigest                  [32]byte
	ListUnsubscribe                       bool
	DefaultTrackOpens, DefaultTrackClicks bool
}

// TrackingOptions is a set of tracking kinds. In a request, Message.Tracking
// holds the kinds explicitly opted in and Message.TrackingOff the kinds
// explicitly opted out; in a stored source, Message.Tracking holds the
// effective tracking that acceptance resolved.
type TrackingOptions struct {
	Opens  bool `json:"opens,omitempty"`
	Clicks bool `json:"clicks,omitempty"`
}

// Attachment content is retained inside the encrypted source object. Public
// adapters must not accept attachments until all source readers are upgraded.
// Inline content IDs do not include the surrounding angle brackets.
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Content     []byte `json:"content"`
	Inline      bool   `json:"inline,omitempty"`
	ContentID   string `json:"content_id,omitempty"`
}

func (Attachment) String() string   { return "<attachment redacted>" }
func (Attachment) GoString() string { return "<attachment redacted>" }

const MaxAttachments = 20

// MaxAttachmentBytes is the fixed ceiling on the total decoded attachment
// bytes of any message. Validate enforces it, and every reader of a stored
// source calls Validate, so it must never decrease: a lower value would make
// accepted messages unreadable. It bounds the configurable admission limit.
//
// It is sized so the final MIME always fits edgeprotocol.MaxMIMEBytes (8 MiB)
// with room for the rest of the message: 5.5 MiB of content is 7689560 bytes
// of base64, 7891916 bytes with a CRLF after every 76 characters, leaving
// about 485 KiB of the 8 MiB for headers, text, HTML and part framing.
// Acceptance preflight still measures the real MIME (including the To and Cc
// recipient headers), so a ceiling-size attachment with a large body is
// rejected visibly rather than stored.
const MaxAttachmentBytes = 5<<20 + 1<<19

// DefaultMaxAttachmentBytes is the admission limit when none is configured.
const DefaultMaxAttachmentBytes = 5 << 20

// MinAttachmentLimit is the smallest configurable admission limit.
const MinAttachmentLimit = 64 << 10

type Message struct {
	From               string
	To                 []string
	Subject            string
	Text               string
	HTML               string
	EnhancedProtection bool
	// Omit when empty to preserve fingerprints and stored source objects created
	// before reply addresses were supported.
	ReplyTo []string `json:"ReplyTo,omitempty"`
	// Cc and Bcc are additional envelope recipients, accepted only through each
	// provider's own fields. Adapters store nil (never an empty slice) so
	// fingerprints and stored sources without them are byte-identical. Only
	// source version 2 may carry them.
	Cc       []string         `json:"Cc,omitempty"`
	Bcc      []string         `json:"Bcc,omitempty"`
	Tracking *TrackingOptions `json:"Tracking,omitempty"`
	// TrackingOff lists kinds the request explicitly turned off, overriding the
	// app default. Part of the idempotency fingerprint; never stored (acceptance
	// clears it). Omitted when nil so earlier fingerprints are byte-identical.
	TrackingOff *TrackingOptions `json:"TrackingOff,omitempty"`
	Attachments []Attachment     `json:"Attachments,omitempty"`
	// App-supplied List-Unsubscribe passthrough. Omitted when absent so
	// fingerprints and stored sources created before U1 are byte-identical.
	// Values can contain addresses or tokens and must never be logged.
	ListUnsubscribe     string `json:"ListUnsubscribe,omitempty"`
	ListUnsubscribePost bool   `json:"ListUnsubscribePost,omitempty"`
	// App-supplied threading headers. InReplyTo is one trimmed msg-id and
	// References is 1-100 msg-ids joined by single spaces (see
	// NormalizeInReplyTo and NormalizeReferences); adapters store the
	// normalized form so the idempotency fingerprint covers it. Omitted when
	// absent so earlier fingerprints and stored sources are byte-identical.
	// Message-IDs can identify mail threads and must never be logged.
	InReplyTo  string `json:"InReplyTo,omitempty"`
	References string `json:"References,omitempty"`
}

// MaxEnvelopeRecipients is the most To, Cc and Bcc addresses together on one
// message. It matches the database limits on recipient indexes and counts.
const MaxEnvelopeRecipients = 100

// Recipient kinds, in envelope order.
const (
	RecipientTo  = "to"
	RecipientCc  = "cc"
	RecipientBcc = "bcc"
)

// Envelope returns every recipient: To, then Cc, then Bcc. A delivery's
// recipient index is a position in this list.
func (m Message) Envelope() []string {
	if len(m.Cc) == 0 && len(m.Bcc) == 0 {
		return m.To
	}
	all := make([]string, 0, len(m.To)+len(m.Cc)+len(m.Bcc))
	all = append(all, m.To...)
	all = append(all, m.Cc...)
	return append(all, m.Bcc...)
}

// RecipientKind returns the kind of the envelope entry at index i, or "" when
// i is out of range.
func (m Message) RecipientKind(i int) string {
	switch {
	case i < 0:
		return ""
	case i < len(m.To):
		return RecipientTo
	case i < len(m.To)+len(m.Cc):
		return RecipientCc
	case i < len(m.To)+len(m.Cc)+len(m.Bcc):
		return RecipientBcc
	}
	return ""
}

// RecipientKinds returns the kind of every envelope entry, in order.
func (m Message) RecipientKinds() []string {
	kinds := make([]string, 0, len(m.To)+len(m.Cc)+len(m.Bcc))
	for range m.To {
		kinds = append(kinds, RecipientTo)
	}
	for range m.Cc {
		kinds = append(kinds, RecipientCc)
	}
	for range m.Bcc {
		kinds = append(kinds, RecipientBcc)
	}
	return kinds
}

const (
	// ListUnsubscribePostValue is the only List-Unsubscribe-Post value
	// (RFC 8058) that Thinpost accepts.
	ListUnsubscribePostValue = "List-Unsubscribe=One-Click"
	maxHeaderLine            = 998
	maxUnsubscribeHTTPSURI   = 2048
)

const (
	maxMsgID           = 998
	maxReferencesIDs   = 100
	maxReferencesBytes = 8192
	// A References line is " <id>" at the least; In-Reply-To also carries its name.
	maxReferencesMsgID = maxHeaderLine - 1
	maxInReplyToMsgID  = maxHeaderLine - len("In-Reply-To: ")
)

func isAtext(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-/=?^_`{|}~", c) >= 0
}

func isDotAtomText(s string) bool {
	if s == "" {
		return false
	}
	prevDot := true
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '.':
			if prevDot {
				return false
			}
			prevDot = true
		case isAtext(s[i]):
			prevDot = false
		default:
			return false
		}
	}
	return !prevDot
}

// validMsgID is the strict RFC 5322 3.6.4 msg-id: "<" id-left "@" id-right ">"
// with a dot-atom-text id-left and a dot-atom-text or no-fold-literal id-right.
// Obsolete forms, comments and folding are not accepted. ASCII only.
func validMsgID(id string) bool {
	if len(id) < len("<a@b>") || len(id) > maxMsgID || id[0] != '<' || id[len(id)-1] != '>' {
		return false
	}
	inner := id[1 : len(id)-1]
	at := strings.IndexByte(inner, '@')
	if at < 0 || !isDotAtomText(inner[:at]) {
		return false
	}
	right := inner[at+1:]
	if strings.HasPrefix(right, "[") {
		if len(right) < 2 || right[len(right)-1] != ']' {
			return false
		}
		for i := 1; i < len(right)-1; i++ {
			if c := right[i]; c < 33 || c > 126 || c == '[' || c == ']' || c == '\\' {
				return false
			}
		}
		return true
	}
	return isDotAtomText(right)
}

func isHeaderSpace(c byte) bool { return c == ' ' || c == '\t' }

// NormalizeInReplyTo trims surrounding spaces and tabs and requires exactly one
// strict msg-id that fits one "In-Reply-To:" line. Errors never include the value.
func NormalizeInReplyTo(value string) (string, error) {
	value = strings.Trim(value, " \t")
	if len(value) > maxInReplyToMsgID || !validMsgID(value) {
		return "", ErrInvalidMessage
	}
	return value, nil
}

// NormalizeReferences requires 1-100 strict msg-ids separated by spaces or
// tabs (CR and LF are rejected) and returns them joined by single spaces, at
// most 8192 bytes. Every id must also fit its own folded line. Errors never
// include the value.
func NormalizeReferences(value string) (string, error) {
	if len(value) > maxReferencesBytes*2 {
		return "", ErrInvalidMessage
	}
	var ids []string
	for i := 0; i < len(value); {
		if isHeaderSpace(value[i]) {
			i++
			continue
		}
		start := i
		for i < len(value) && !isHeaderSpace(value[i]) {
			i++
		}
		id := value[start:i]
		if len(id) > maxReferencesMsgID || !validMsgID(id) || len(ids) == maxReferencesIDs {
			return "", ErrInvalidMessage
		}
		ids = append(ids, id)
	}
	normalized := strings.Join(ids, " ")
	if len(ids) == 0 || len(normalized) > maxReferencesBytes {
		return "", ErrInvalidMessage
	}
	return normalized, nil
}

// ValidateInReplyTo accepts an empty value or an already-normalized msg-id.
func ValidateInReplyTo(value string) error {
	if value == "" {
		return nil
	}
	if normalized, err := NormalizeInReplyTo(value); err != nil || normalized != value {
		return ErrInvalidMessage
	}
	return nil
}

// ValidateReferences accepts an empty value or an already-normalized list.
func ValidateReferences(value string) error {
	if value == "" {
		return nil
	}
	if normalized, err := NormalizeReferences(value); err != nil || normalized != value {
		return ErrInvalidMessage
	}
	return nil
}

// ValidateListUnsubscribe checks an app-supplied List-Unsubscribe value and
// whether List-Unsubscribe-Post accompanies it. The value is 1-2 "<uri>"
// items separated by commas; each is https without userinfo (at most 2048
// bytes) or mailto. The header line fits 998 bytes and is printable ASCII.
// Post requires an https item. Errors never include the value.
func ValidateListUnsubscribe(value string, post bool) error {
	if value == "" {
		if post {
			return ErrInvalidMessage
		}
		return nil
	}
	if len("List-Unsubscribe: ")+len(value) > maxHeaderLine {
		return ErrInvalidMessage
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return ErrInvalidMessage
		}
	}
	items, hasHTTPS := 0, false
	rest := value
	for {
		rest = strings.TrimLeft(rest, " ")
		if !strings.HasPrefix(rest, "<") {
			return ErrInvalidMessage
		}
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			return ErrInvalidMessage
		}
		uri := rest[1:end]
		rest = strings.TrimLeft(rest[end+1:], " ")
		items++
		if items > 2 || strings.ContainsAny(uri, "< ") {
			return ErrInvalidMessage
		}
		switch {
		case len(uri) > len("https:") && strings.EqualFold(uri[:len("https:")], "https:"):
			parsed, err := url.Parse(uri)
			if err != nil || len(uri) > maxUnsubscribeHTTPSURI || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
				return ErrInvalidMessage
			}
			hasHTTPS = true
		case len(uri) > len("mailto:") && strings.EqualFold(uri[:len("mailto:")], "mailto:"):
		default:
			return ErrInvalidMessage
		}
		if rest == "" {
			break
		}
		if rest[0] != ',' {
			return ErrInvalidMessage
		}
		rest = rest[1:]
	}
	if post && !hasHTTPS {
		return ErrInvalidMessage
	}
	return nil
}

// RequestTracking records a request's per-kind choice: nil leaves the kind to
// the app default, true opts in, false opts out. It allocates fresh options.
func (m *Message) RequestTracking(opens, clicks *bool) {
	on, off := TrackingOptions{}, TrackingOptions{}
	if opens != nil {
		on.Opens, off.Opens = *opens, !*opens
	}
	if clicks != nil {
		on.Clicks, off.Clicks = *clicks, !*clicks
	}
	m.Tracking, m.TrackingOff = nil, nil
	if on.Opens || on.Clicks {
		m.Tracking = &on
	}
	if off.Opens || off.Clicks {
		m.TrackingOff = &off
	}
}

// ValidateAttachmentLimit is the admission check for the configured limit,
// which is at most MaxAttachmentBytes. Validate alone checks the ceiling.
func (m Message) ValidateAttachmentLimit(limit int) error {
	total := 0
	for _, attachment := range m.Attachments {
		total += len(attachment.Content)
		if total > limit {
			return ErrInvalidMessage
		}
	}
	return nil
}

// Validate rejects header injection and malformed addresses before the
// message reaches the durable submission implementation.
func (m Message) Validate() error {
	if m.Tracking != nil && (m.HTML == "" || (!m.Tracking.Opens && !m.Tracking.Clicks)) {
		return ErrInvalidMessage
	}
	// An explicit opt-out is valid without HTML: it only overrides a default.
	if m.TrackingOff != nil && ((!m.TrackingOff.Opens && !m.TrackingOff.Clicks) ||
		(m.Tracking != nil && (m.Tracking.Opens && m.TrackingOff.Opens || m.Tracking.Clicks && m.TrackingOff.Clicks))) {
		return ErrInvalidMessage
	}
	if len(m.Attachments) > MaxAttachments {
		return ErrInvalidMessage
	}
	contentBytes := 0
	contentIDs := make(map[string]bool)
	for _, attachment := range m.Attachments {
		if attachment.Content == nil || len(attachment.Content) > MaxAttachmentBytes-contentBytes || !validAttachmentFilename(attachment.Filename) {
			return ErrInvalidMessage
		}
		contentBytes += len(attachment.Content)
		mediaType, params, err := mime.ParseMediaType(attachment.ContentType)
		typeName, subtype, ok := strings.Cut(mediaType, "/")
		if err != nil || !ok || typeName == "" || subtype == "" || strings.Contains(mediaType, "*") || len(params) != 0 || strings.ContainsAny(attachment.ContentType, "\r\n") || len(attachment.ContentType) > 128 {
			return ErrInvalidMessage
		}
		if attachment.Inline {
			id := strings.ToLower(attachment.ContentID)
			if m.HTML == "" || !validAttachmentContentID(attachment.ContentID) || contentIDs[id] {
				return ErrInvalidMessage
			}
			contentIDs[id] = true
		} else if attachment.ContentID != "" {
			return ErrInvalidMessage
		}
	}
	from, err := mail.ParseAddress(m.From)
	if err != nil || from.Address == "" || strings.ContainsAny(m.From, "\r\n") {
		return ErrInvalidMessage
	}
	if len(m.To) == 0 || len(m.To)+len(m.Cc)+len(m.Bcc) > MaxEnvelopeRecipients {
		return ErrInvalidMessage
	}
	for _, recipient := range m.Envelope() {
		address, err := mail.ParseAddress(recipient)
		if err != nil || address.Address == "" || strings.ContainsAny(recipient, "\r\n") {
			return ErrInvalidMessage
		}
	}
	if m.ListUnsubscribe == "" && m.ListUnsubscribePost || ValidateListUnsubscribe(m.ListUnsubscribe, m.ListUnsubscribePost) != nil {
		return ErrInvalidMessage
	}
	if ValidateInReplyTo(m.InReplyTo) != nil || ValidateReferences(m.References) != nil {
		return ErrInvalidMessage
	}
	if len(m.ReplyTo) > 10 {
		return ErrInvalidMessage
	}
	replyHeaderLength := len("Reply-To: ")
	for i, value := range m.ReplyTo {
		address, err := mail.ParseAddress(value)
		if err != nil || address.Address == "" || strings.ContainsAny(value, "\r\n") {
			return ErrInvalidMessage
		}
		if i > 0 {
			replyHeaderLength += 2
		}
		replyHeaderLength += len(address.String())
	}
	if replyHeaderLength > 998 {
		return ErrInvalidMessage
	}
	if strings.TrimSpace(m.Subject) == "" || !utf8.ValidString(m.Subject) || !utf8.ValidString(m.Text) || !utf8.ValidString(m.HTML) || (m.Text == "" && m.HTML == "") {
		return ErrInvalidMessage
	}
	for _, c := range m.Subject {
		if c == 127 || c < 32 && c != '\t' {
			return ErrInvalidMessage
		}
	}
	return nil
}

func validAttachmentFilename(name string) bool {
	if name == "" || len(name) > 200 || !utf8.ValidString(name) || strings.TrimSpace(name) != name || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return false
	}
	for _, c := range name {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func validAttachmentContentID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._@+-", c)) {
			return false
		}
	}
	return true
}

type Request struct {
	Message Message
	// IdempotencyKey is scoped to the authenticated app and adapter by the
	// durable implementation. A successful response requires a durable record.
	IdempotencyKey string
	Adapter        string
	// SendingDomain must be independently authorized for this application.
	SendingDomain string
	// RejectAllSuppressed makes Accept return ErrAllRecipientsSuppressed when
	// every distinct recipient is suppressed. Only the Postmark adapter sets it
	// (ErrorCode 406); the others accept and drop suppressed recipients later.
	RejectAllSuppressed bool
	// Hold accepts the message on hold (X-Thinpost-Hold: true): stored and
	// counted like any accepted message, but not prepared or sent until the app
	// releases it. It is part of the idempotency fingerprint.
	Hold bool
	// Overrides are operator-only relaxations of acceptance policy. They are
	// accepted only for an operator principal on the console adapter; Accept
	// refuses them for any other principal. They are part of the fingerprint
	// only when set, which no idempotent route can do.
	Overrides Overrides
}

// Overrides are the operator-only options of a console test send. The zero
// value relaxes nothing. They never relax the verified sender domain, DKIM
// signing, suppressions, the paused state, the admission gate, any size or
// profile limit, the egress policy or Enhanced Protection TLS.
type Overrides struct {
	// Limits skips, and does not count, the submission rate bucket, the daily
	// recipient quota and the pending-message limit.
	Limits bool
	// RecipientPolicy skips the deployment recipient allowlist only.
	RecipientPolicy bool
}

// Any reports whether any override is set.
func (o Overrides) Any() bool { return o.Limits || o.RecipientPolicy }

// MaxHold is how long a message submitted on hold waits for release. After it
// the hold expires and the message is never sent.
const MaxHold = 7 * 24 * time.Hour

type Receipt struct {
	ID         string
	AcceptedAt time.Time
	// Replayed is true when an idempotent retry returned an earlier receipt.
	Replayed bool
	// Held is set by the raw (SMTP) path when the message was accepted on hold
	// (the X-Thinpost-Hold header). It is never read from the database: the
	// replay digests cover the raw bytes, so a replay carries the same choice.
	Held bool
}

// BatchRequest represents one atomic provider batch. Resend's retry key
// identifies the ordered request as a whole, not individual messages.
type BatchRequest struct {
	Messages       []Message
	IdempotencyKey string
	Adapter        string
	// Hold accepts every message of the batch on hold (see Request.Hold).
	Hold bool
}

type BatchReceipt struct {
	IDs []string
	// Replayed is true when an idempotent retry returned earlier IDs.
	Replayed bool
}

// BatchService must commit every message in the batch together or none. A
// successful idempotent retry returns the original IDs in their original order.
type BatchService interface {
	AcceptBatch(context.Context, App, BatchRequest) (BatchReceipt, error)
}

// Service implementations must authenticate credentials, enforce sender
// authorization and persist the message before returning a receipt. An
// implementation must atomically enforce applicable idempotency semantics.
type Service interface {
	Authenticate(ctx context.Context, key string) (App, error)
	Accept(ctx context.Context, app App, request Request) (Receipt, error)
}

// OperatorService is implemented by services that can submit on behalf of an
// authenticated administrator. AuthorizeOperator returns the application as an
// operator principal; actor must be the verified administrator subject. The
// result is passed to the same Service.Accept as an API-key submission.
type OperatorService interface {
	AuthorizeOperator(ctx context.Context, appID, actor string) (App, error)
}
