package format

import (
	"net/http"

	"github.com/zyno-io/thinpost/internal/providerid"
)

// postmarkTime matches the submission response's SubmittedAt.
const postmarkTime = "2006-01-02T15:04:05.0000000Z"

type postmarkFormat struct{}

func (postmarkFormat) EventTypes() []string {
	return []string{"Delivery", "Bounce", "Open", "SubscriptionChange", "SpamComplaint"}
}

func (postmarkFormat) Classify(f Fact) (string, bool) {
	switch classify(f) {
	case classAccepted:
		return "Delivery", true
	case classPermanent, classSoft:
		return "Bounce", true
	case classOpened:
		return "Open", true
	case classUnsubscribed:
		return "SubscriptionChange", true
	case classComplained:
		return "SpamComplaint", true
	}
	// Deferred deliveries are retried silently by Postmark; everything else
	// has no Postmark event.
	return "", false
}

// NewSecret: Postmark webhooks are unsigned.
func (postmarkFormat) NewSecret() (string, error) { return "", ErrNoSecret }

type postmarkDelivery struct {
	RecordType    string            `json:"RecordType"`
	MessageStream string            `json:"MessageStream"`
	MessageID     string            `json:"MessageID"`
	Recipient     string            `json:"Recipient,omitempty"`
	Tag           string            `json:"Tag"`
	DeliveredAt   string            `json:"DeliveredAt"`
	Details       string            `json:"Details"`
	Metadata      map[string]string `json:"Metadata"`
}

type postmarkBounce struct {
	RecordType    string            `json:"RecordType"`
	MessageStream string            `json:"MessageStream"`
	Type          string            `json:"Type"`
	TypeCode      int               `json:"TypeCode"`
	Name          string            `json:"Name"`
	Tag           string            `json:"Tag"`
	MessageID     string            `json:"MessageID"`
	Metadata      map[string]string `json:"Metadata"`
	Description   string            `json:"Description"`
	Details       string            `json:"Details"`
	Email         string            `json:"Email,omitempty"`
	BouncedAt     string            `json:"BouncedAt"`
	DumpAvailable bool              `json:"DumpAvailable"`
	Inactive      bool              `json:"Inactive"`
	From          string            `json:"From,omitempty"`
	Subject       string            `json:"Subject,omitempty"`
}

type postmarkOpen struct {
	RecordType    string            `json:"RecordType"`
	MessageStream string            `json:"MessageStream"`
	FirstOpen     *bool             `json:"FirstOpen,omitempty"`
	MessageID     string            `json:"MessageID"`
	Metadata      map[string]string `json:"Metadata"`
	ReceivedAt    string            `json:"ReceivedAt"`
	Tag           string            `json:"Tag"`
	Recipient     string            `json:"Recipient,omitempty"`
}

// postmarkSubscriptionChange is a recipient's one-click unsubscribe. ServerID
// is a Postmark account number Thinpost does not have, so it is omitted.
type postmarkSubscriptionChange struct {
	RecordType        string            `json:"RecordType"`
	MessageID         string            `json:"MessageID"`
	MessageStream     string            `json:"MessageStream"`
	ChangedAt         string            `json:"ChangedAt"`
	Recipient         string            `json:"Recipient,omitempty"`
	Origin            string            `json:"Origin"`
	SuppressSending   bool              `json:"SuppressSending"`
	SuppressionReason string            `json:"SuppressionReason"`
	Tag               string            `json:"Tag"`
	Metadata          map[string]string `json:"Metadata"`
}

// postmarkSpamComplaint is Postmark's bounce-shaped spam complaint record. Its
// Content (the abuse report dump) is omitted: Thinpost never stores reports.
type postmarkSpamComplaint struct {
	RecordType    string            `json:"RecordType"`
	MessageStream string            `json:"MessageStream"`
	Type          string            `json:"Type"`
	TypeCode      int               `json:"TypeCode"`
	Name          string            `json:"Name"`
	Tag           string            `json:"Tag"`
	MessageID     string            `json:"MessageID"`
	Metadata      map[string]string `json:"Metadata"`
	Description   string            `json:"Description"`
	Details       string            `json:"Details"`
	Email         string            `json:"Email,omitempty"`
	From          string            `json:"From,omitempty"`
	BouncedAt     string            `json:"BouncedAt"`
	DumpAvailable bool              `json:"DumpAvailable"`
	Inactive      bool              `json:"Inactive"`
	CanActivate   bool              `json:"CanActivate"`
	Subject       string            `json:"Subject,omitempty"`
}

func (postmarkFormat) Build(in Input) (Request, error) {
	fact := Fact{in.Event.Outcome, in.Event.SMTPCode, in.Event.DeliveryState}
	cls := classify(fact)
	if _, ok := (postmarkFormat{}).Classify(fact); !ok || !opaque(in.Event.ID) || in.Event.MessageID == "" {
		return Request{}, errInvalidInput
	}
	at := in.Event.OccurredAt.UTC().Format(postmarkTime)
	id := providerid.Postmark(in.Event.MessageID)
	recipient := in.Context.firstRecipient()
	var body []byte
	var err error
	switch cls {
	case classAccepted:
		body, err = marshal(postmarkDelivery{"Delivery", "outbound", id, recipient, "", at, describe(cls, fact), map[string]string{}})
	case classUnsubscribed:
		body, err = marshal(postmarkSubscriptionChange{"SubscriptionChange", id, "outbound", at, recipient, "Recipient", true, "ManualSuppression", "", map[string]string{}})
	case classComplained:
		body, err = marshal(postmarkSpamComplaint{RecordType: "SpamComplaint", MessageStream: "outbound", Type: "SpamComplaint", TypeCode: 100001, Name: "Spam complaint",
			MessageID: id, Metadata: map[string]string{}, Description: "The subscriber explicitly marked this message as spam.", Details: describe(cls, fact),
			Email: recipient, From: in.Context.SenderAddress, BouncedAt: at, Inactive: true, Subject: in.Context.subject()})
	case classOpened:
		body, err = marshal(postmarkOpen{"Open", "outbound", in.Context.FirstOpen, id, map[string]string{}, at, "", recipient})
	default: // classPermanent, classSoft
		b := postmarkBounce{RecordType: "Bounce", MessageStream: "outbound", Tag: "", MessageID: id, Metadata: map[string]string{},
			Details: describe(cls, fact), Email: recipient, BouncedAt: at, From: in.Context.SenderAddress, Subject: in.Context.subject()}
		if cls == classPermanent {
			b.Type, b.TypeCode, b.Name = "HardBounce", 1, "Hard bounce"
			b.Description = "The server was unable to deliver your message (ex: unknown user, mailbox not found)."
		} else {
			b.Type, b.TypeCode, b.Name = "SoftBounce", 4096, "Soft bounce"
			b.Description = "Unable to temporarily deliver message (i.e. mailbox full, account disabled, exceeds quota, out of disk space)."
		}
		body, err = marshal(b)
	}
	if err != nil {
		return Request{}, err
	}
	h := http.Header{"Content-Type": {"application/json"}}
	// Set directly to keep Postmark's documented header capitalization.
	h["X-PM-Webhook-Trace-Id"] = []string{in.Event.ID}
	if in.Test {
		h.Set("X-Thinpost-Test", "true")
	}
	return Request{Body: body, Header: h}, nil
}
