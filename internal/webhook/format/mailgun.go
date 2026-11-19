package format

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/zyno-io/thinpost/internal/providerid"
)

const minMailgunKey = 32

type mailgunFormat struct{}

// Mailgun's own webhook names.
func (mailgunFormat) EventTypes() []string {
	return []string{"delivered", "temporary_fail", "permanent_fail", "opened", "unsubscribed", "complained"}
}

func (mailgunFormat) Classify(f Fact) (string, bool) {
	switch classify(f) {
	case classAccepted:
		return "delivered", true
	case classDeferred:
		return "temporary_fail", true
	case classPermanent, classSoft, classTrackingAbandoned, classSuppressed:
		return "permanent_fail", true
	case classOpened:
		return "opened", true
	case classUnsubscribed:
		return "unsubscribed", true
	case classComplained:
		return "complained", true
	}
	// preparation_failed and hold_expired are message-level; Mailgun events are
	// per recipient.
	return "", false
}

// NewSecret returns 64 hex characters, used verbatim as the signing key.
func (mailgunFormat) NewSecret() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", errors.New("cannot generate webhook secret")
	}
	return hex.EncodeToString(random), nil
}

type mailgunBody struct {
	Signature mailgunSignature `json:"signature"`
	EventData mailgunEvent     `json:"event-data"`
}

type mailgunSignature struct {
	Timestamp string `json:"timestamp"`
	Token     string `json:"token"`
	Signature string `json:"signature"`
}

type mailgunEvent struct {
	Event           string                 `json:"event"`
	ID              string                 `json:"id"`
	Timestamp       float64                `json:"timestamp"`
	LogLevel        string                 `json:"log-level"`
	Severity        string                 `json:"severity,omitempty"`
	Reason          string                 `json:"reason,omitempty"`
	Recipient       string                 `json:"recipient,omitempty"`
	RecipientDomain string                 `json:"recipient-domain,omitempty"`
	Message         mailgunMessage         `json:"message"`
	DeliveryStatus  *mailgunDeliveryStatus `json:"delivery-status,omitempty"`
	Domain          *mailgunDomain         `json:"domain,omitempty"`
	Tags            []string               `json:"tags"`
	UserVariables   map[string]string      `json:"user-variables"`
}

type mailgunMessage struct {
	Headers mailgunHeaders `json:"headers"`
}

type mailgunHeaders struct {
	MessageID string `json:"message-id"`
	From      string `json:"from,omitempty"`
	Subject   string `json:"subject,omitempty"`
}

type mailgunDeliveryStatus struct {
	Code       int    `json:"code,omitempty"`
	Message    string `json:"message"`
	BounceType string `json:"bounce-type,omitempty"`
}

type mailgunDomain struct {
	Name string `json:"name"`
}

func (mailgunFormat) Build(in Input) (Request, error) {
	fact := Fact{in.Event.Outcome, in.Event.SMTPCode, in.Event.DeliveryState}
	_, ok := (mailgunFormat{}).Classify(fact)
	if !ok || in.Now.IsZero() || len(in.Secret) < minMailgunKey || !opaque(in.Event.ID) || in.Event.MessageID == "" {
		return Request{}, errInvalidInput
	}
	cls := classify(fact)
	ev := mailgunEvent{
		ID:            in.Event.ID,
		Timestamp:     float64(in.Event.OccurredAt.UnixMicro()) / 1e6,
		LogLevel:      "info",
		Message:       mailgunMessage{mailgunHeaders{MessageID: providerid.MailgunHeader(in.Event.MessageID, in.Context.SenderDomain)}},
		Tags:          []string{},
		UserVariables: map[string]string{},
	}
	if r := in.Context.firstRecipient(); r != "" {
		ev.Recipient = r
		if at := strings.LastIndexByte(r, '@'); at >= 0 {
			ev.RecipientDomain = strings.ToLower(r[at+1:])
		}
	}
	if in.Context.SenderDomain != "" {
		ev.Domain = &mailgunDomain{in.Context.SenderDomain}
	}
	switch cls {
	case classAccepted:
		ev.Event = "delivered"
		ev.Message.Headers.Subject = in.Context.subject()
		ev.Message.Headers.From = in.Context.fromDisplay()
		ev.DeliveryStatus = &mailgunDeliveryStatus{Code: 250, Message: describe(cls, fact)}
	case classOpened:
		ev.Event = "opened"
	case classUnsubscribed:
		ev.Event = "unsubscribed"
	case classComplained:
		ev.Event, ev.LogLevel = "complained", "warn"
		ev.Message.Headers.Subject = in.Context.subject()
		ev.Message.Headers.From = in.Context.fromDisplay()
	default:
		ev.Event, ev.LogLevel = "failed", "error"
		ev.Message.Headers.Subject = in.Context.subject()
		ev.Message.Headers.From = in.Context.fromDisplay()
		ev.Severity, ev.Reason = "permanent", "generic"
		status := &mailgunDeliveryStatus{Code: in.Event.SMTPCode, Message: describe(cls, fact)}
		switch {
		case cls == classDeferred:
			ev.LogLevel, ev.Severity = "warn", "temporary"
		case cls == classPermanent && in.Event.SMTPCode >= 500:
			ev.Reason, status.BounceType = "bounce", "hard"
		case cls == classSuppressed:
			ev.Reason, status.Code = mailgunSuppression(in.Event.SuppressionReason)
		}
		ev.DeliveryStatus = status
	}
	rnd := in.Rand
	if rnd == nil {
		rnd = rand.Reader
	}
	var raw [25]byte
	if _, err := io.ReadFull(rnd, raw[:]); err != nil {
		return Request{}, errors.New("cannot generate webhook token")
	}
	timestamp := strconv.FormatInt(in.Now.Unix(), 10)
	token := hex.EncodeToString(raw[:])
	mac := hmac.New(sha256.New, in.Secret)
	_, _ = mac.Write([]byte(timestamp + token))
	body, err := marshal(mailgunBody{mailgunSignature{timestamp, token, hex.EncodeToString(mac.Sum(nil))}, ev})
	if err != nil {
		return Request{}, err
	}
	h := http.Header{"Content-Type": {"application/json"}}
	if in.Test {
		h.Set("X-Thinpost-Test", "true")
	}
	return Request{Body: body, Header: h}, nil
}

// mailgunSuppression maps a suppression reason to Mailgun's suppress-* failure
// reason and its delivery-status code. Mailgun has no manual reason, so manual
// and hard-bounce entries both render as suppress-bounce.
func mailgunSuppression(reason string) (string, int) {
	switch reason {
	case "unsubscribe":
		return "suppress-unsubscribe", 606 // verify against Mailgun docs
	case "spam_complaint":
		return "suppress-complaint", 607 // verify against Mailgun docs
	}
	return "suppress-bounce", 605 // verify against Mailgun docs
}
