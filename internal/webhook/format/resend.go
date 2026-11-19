package format

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"

	"github.com/zyno-io/thinpost/internal/providerid"
)

const (
	resendTime       = "2006-01-02T15:04:05.000Z"
	resendSecretPfx  = "whsec_"
	minResendKeySize = 24
)

type resendFormat struct{}

func (resendFormat) EventTypes() []string {
	return []string{"email.delivered", "email.delivery_delayed", "email.bounced", "email.failed", "email.opened", "email.suppressed", "email.complained"}
}

func (resendFormat) Classify(f Fact) (string, bool) {
	switch classify(f) {
	case classAccepted:
		return "email.delivered", true
	case classDeferred:
		return "email.delivery_delayed", true
	case classPermanent, classSoft:
		return "email.bounced", true
	case classTrackingAbandoned, classPreparationFailed, classHoldExpired:
		return "email.failed", true
	case classOpened:
		return "email.opened", true
	case classSuppressed:
		return "email.suppressed", true
	case classComplained:
		return "email.complained", true
	}
	return "", false
}

// NewSecret matches the shape of Svix signing secrets: "whsec_" and the
// standard base64 of 24 random bytes.
func (resendFormat) NewSecret() (string, error) {
	random := make([]byte, minResendKeySize)
	if _, err := rand.Read(random); err != nil {
		return "", errors.New("cannot generate webhook secret")
	}
	return resendSecretPfx + base64.StdEncoding.EncodeToString(random), nil
}

type resendEvent struct {
	Type      string     `json:"type"`
	CreatedAt string     `json:"created_at"`
	Data      resendData `json:"data"`
}

type resendData struct {
	CreatedAt  string            `json:"created_at,omitempty"`
	EmailID    string            `json:"email_id"`
	From       string            `json:"from,omitempty"`
	To         []string          `json:"to,omitempty"`
	Subject    string            `json:"subject,omitempty"`
	Bounce     *resendBounce     `json:"bounce,omitempty"`
	Failed     *resendFailed     `json:"failed,omitempty"`
	Suppressed *resendSuppressed `json:"suppressed,omitempty"`
}

type resendSuppressed struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type resendBounce struct {
	Message        string   `json:"message"`
	SubType        string   `json:"subType"`
	Type           string   `json:"type"`
	DiagnosticCode []string `json:"diagnosticCode,omitempty"`
}

type resendFailed struct {
	Reason string `json:"reason"`
}

// SignSvix returns the svix-signature header value: "v1," and the base64
// HMAC-SHA256 of "id.timestamp.body". The key length is the caller's concern.
func SignSvix(key []byte, id, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(id + "." + timestamp + "."))
	_, _ = mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (resendFormat) Build(in Input) (Request, error) {
	fact := Fact{in.Event.Outcome, in.Event.SMTPCode, in.Event.DeliveryState}
	eventType, ok := (resendFormat{}).Classify(fact)
	if !ok || in.Now.IsZero() || !opaque(in.Event.ID) || in.Event.MessageID == "" || !bytes.HasPrefix(in.Secret, []byte(resendSecretPfx)) {
		return Request{}, errInvalidInput
	}
	key := make([]byte, base64.StdEncoding.DecodedLen(len(in.Secret)-len(resendSecretPfx)))
	n, err := base64.StdEncoding.Decode(key, in.Secret[len(resendSecretPfx):])
	if err != nil || n < minResendKeySize {
		return Request{}, errInvalidInput
	}
	cls := classify(fact)
	data := resendData{EmailID: providerid.Resend(in.Event.MessageID), From: in.Context.fromDisplay(), To: in.Context.Recipients, Subject: in.Context.subject()}
	if !in.Context.AcceptedAt.IsZero() {
		data.CreatedAt = in.Context.AcceptedAt.UTC().Format(resendTime)
	}
	switch cls {
	case classPermanent, classSoft:
		b := &resendBounce{Message: describe(cls, fact), SubType: "General", Type: "Permanent"}
		if cls == classSoft {
			b.Type = "Transient"
		}
		if in.Event.SMTPCode > 0 {
			b.DiagnosticCode = []string{"smtp; " + strconv.Itoa(in.Event.SMTPCode)}
		}
		data.Bounce = b
	case classSuppressed:
		data.Suppressed = &resendSuppressed{Message: describe(cls, fact), Type: "OnAccountSuppressionList"}
	case classTrackingAbandoned:
		data.Failed = &resendFailed{Reason: "tracking_abandoned"}
	case classPreparationFailed:
		data.Failed = &resendFailed{Reason: "preparation_failed"}
	case classHoldExpired:
		data.Failed = &resendFailed{Reason: "hold_expired"}
	}
	body, err := marshal(resendEvent{eventType, in.Event.OccurredAt.UTC().Format(resendTime), data})
	if err != nil {
		return Request{}, err
	}
	timestamp := strconv.FormatInt(in.Now.Unix(), 10)
	h := http.Header{"Content-Type": {"application/json"}}
	h["svix-id"] = []string{in.Event.ID}
	h["svix-timestamp"] = []string{timestamp}
	h["svix-signature"] = []string{SignSvix(key[:n], in.Event.ID, timestamp, body)}
	if in.Test {
		h.Set("X-Thinpost-Test", "true")
	}
	return Request{Body: body, Header: h}, nil
}
