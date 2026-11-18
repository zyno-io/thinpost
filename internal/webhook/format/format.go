// Package format renders webhook events as request bodies and headers in the
// native Thinpost format or a provider-compatible one. It is a leaf package:
// it performs no I/O except reading random bytes, and never logs. Encoders
// emit only fields built from recorded data or constants true by construction;
// anything Thinpost does not record is omitted, never null or invented.
package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
)

const (
	Thinpost = "thinpost"
	Postmark = "postmark"
	Resend   = "resend"
	Mailgun  = "mailgun"
)

// ErrNoSecret is returned by NewSecret for formats that are not signed.
var ErrNoSecret = errors.New("webhook format has no signing secret")

// Event is one webhook_events row.
type Event struct {
	ID              string    `json:"id"`
	MessageID       string    `json:"message_id"`
	DeliveryID      string    `json:"delivery_id,omitempty"`
	AttemptID       string    `json:"attempt_id,omitempty"`
	BounceReceiptID string    `json:"bounce_receipt_id,omitempty"`
	TrackingLinkID  string    `json:"tracking_link_id,omitempty"`
	Outcome         string    `json:"outcome"`
	SMTPCode        int       `json:"smtp_code"`
	OccurredAt      time.Time `json:"occurred_at"`
	// DeliveryState is the delivery state the event produced; empty when the
	// event made no delivery transition or predates its recording.
	DeliveryState string `json:"delivery_state,omitempty"`
	// SuppressionReason is the list reason of a suppressed delivery.
	SuppressionReason string `json:"suppression_reason,omitempty"`
	// MessageIDHeader is the delivery's RFC 5322 Message-ID, with angle
	// brackets: shared by all deliveries of a message prepared at schema 38 or
	// later, per delivery for earlier ones. Empty (omitted) when there is no
	// delivery or no recorded sender domain. Native format only.
	MessageIDHeader string `json:"message_id_header,omitempty"`
}

// Fact is what Classify needs from an event.
type Fact struct {
	Outcome       string
	SMTPCode      int
	DeliveryState string
}

// Context is read-only message metadata. It has no body by construction.
// SenderAddress and SenderName are the recorded From address and decoded
// display name; both are empty for messages accepted before schema 31 and are
// then omitted, never invented. Subject is empty for Enhanced Protection messages, and
// encoders re-check EnhancedProtection before emitting it.
type Context struct {
	AcceptedAt         time.Time
	EnhancedProtection bool
	Subject            string
	SenderDomain       string
	SenderAddress      string
	SenderName         string
	Recipients         []string // the delivery's recipient, or every To recipient (never Cc or Bcc) for message-level events
	FirstOpen          *bool
}

type Input struct {
	Event   Event
	Context Context
	AppID   string
	Secret  []byte // opened secret bytes; nil for postmark
	Now     time.Time
	Test    bool
	Rand    io.Reader // nil means crypto/rand (Mailgun token)
	// Inbound is the recorded message of an inbound_received event; only the
	// inbound formats read it.
	Inbound *InboundInput
}

// Request never contains HTTP authentication; the sender adds it.
type Request struct {
	Body   []byte
	Header http.Header
}

type Format interface {
	// EventTypes lists the event names an endpoint of this format can filter on.
	EventTypes() []string
	// Classify maps an event to this format's event type; ok is false when the
	// format cannot represent the event honestly, which is never queued for it.
	Classify(Fact) (eventType string, ok bool)
	// NewSecret returns a new signing secret, or ErrNoSecret.
	NewSecret() (string, error)
	Build(Input) (Request, error)
}

var formats = map[string]Format{
	Thinpost:        thinpostFormat{},
	ThinpostInbound: thinpostInboundFormat{},
	Postmark:        postmarkFormat{},
	Resend:          resendFormat{},
	Mailgun:         mailgunFormat{},
}

// Names lists the supported formats in a stable order.
func Names() []string { return []string{Thinpost, Postmark, Resend, Mailgun, ThinpostInbound} }

func Lookup(name string) (Format, bool) {
	f, ok := formats[name]
	return f, ok
}

// Unsigned reports whether the named format has no signing secret.
func Unsigned(name string) bool { return name == Postmark }

// ValidEvents accepts a non-empty, unique list of at most 32 event types known
// to the named format.
func ValidEvents(name string, events []string) bool {
	f, ok := Lookup(name)
	if !ok || len(events) == 0 || len(events) > 32 {
		return false
	}
	known := f.EventTypes()
	for i, e := range events {
		if !slices.Contains(known, e) || slices.Contains(events[:i], e) {
			return false
		}
	}
	return true
}

// Wants reports whether an endpoint of the format with the given event filter
// (empty means every event) should receive the fact.
func Wants(name string, filter []string, f Fact) bool {
	impl, ok := Lookup(name)
	if !ok {
		return false
	}
	eventType, ok := impl.Classify(f)
	return ok && (len(filter) == 0 || slices.Contains(filter, eventType))
}

// TestEvent is the primary event of the provider formats with all-zero IDs, a
// reserved recipient and no subject, for the console's test send.
func TestEvent(eventID string, now time.Time) (Event, Context) {
	const zero = "00000000000000000000000000000000"
	return Event{ID: eventID, MessageID: zero, Outcome: "accepted", SMTPCode: 250, OccurredAt: now, DeliveryState: "accepted"},
		Context{AcceptedAt: now, Recipients: []string{"test@example.com"}}
}

// marshal encodes without HTML escaping, as provider samples show, and without
// the encoder's trailing newline.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func opaque(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (c Context) subject() string {
	if c.EnhancedProtection {
		return ""
	}
	return c.Subject
}

// fromDisplay is the From header form providers show: the bare address, or
// `Name <address>` with the name quoted when it holds anything but atom text
// and spaces. It is never RFC 2047-encoded; the JSON carries UTF-8 directly.
func (c Context) fromDisplay() string {
	if c.SenderAddress == "" {
		return ""
	}
	if c.SenderName == "" {
		return c.SenderAddress
	}
	plain := strings.TrimSpace(c.SenderName) == c.SenderName
	for _, r := range c.SenderName {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r < 0x80 && strings.ContainsRune("!#$%&'*+-/=?^_`{|}~", r)) {
			plain = false
			break
		}
	}
	if plain && !strings.Contains(c.SenderName, "  ") {
		return c.SenderName + " <" + c.SenderAddress + ">"
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(c.SenderName) + `" <` + c.SenderAddress + ">"
}

func (c Context) firstRecipient() string {
	if len(c.Recipients) == 0 {
		return ""
	}
	return c.Recipients[0]
}

var errInvalidInput = errors.New("invalid webhook event")
