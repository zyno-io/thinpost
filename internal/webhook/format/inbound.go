package format

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ThinpostInbound is the native format for inbound mail. It represents only
// inbound_received, and no other format does; every other native or provider
// format never represents it. Signing is identical to the native format.
const ThinpostInbound = "thinpost_inbound"

// InboundReceived is the only event of an inbound format.
const InboundReceived = "inbound_received"

// InboundFormat reports whether the named format delivers inbound mail.
func InboundFormat(name string) bool { return name == ThinpostInbound }

// InboundCorrelation is the recorded link from a reply to the app's own
// outbound mail. It is a claim made by the message's headers, not
// authentication.
type InboundCorrelation struct {
	MessageID  string
	DeliveryID string // empty when only the message is known
	Header     string // in_reply_to or references
	// FromMatches is nil when it could not be determined.
	FromMatches *bool
}

// InboundInput is what Thinpost recorded about one received message and one
// of its envelope recipients, plus the stored raw MIME. It carries no content
// beyond Raw by construction.
type InboundInput struct {
	ID         string
	ReceivedAt time.Time
	Recipient  string
	Recipients []string // every envelope recipient of the message
	MailFrom   string   // empty for the null reverse path
	ClientIP   string
	Helo       string
	TLSVersion string
	TLSCipher  string
	Receiver   string // the authserv-id: the name of the receiving host
	Raw        []byte
	// Authentication is the stored per-method document; the three result
	// columns are the fallback when it is absent or unreadable.
	Authentication   json.RawMessage
	SPF, DKIM, DMARC string
	Correlation      *InboundCorrelation
}

type thinpostInboundFormat struct{}

func (thinpostInboundFormat) EventTypes() []string { return []string{InboundReceived} }

func (thinpostInboundFormat) Classify(f Fact) (string, bool) {
	return InboundReceived, f.Outcome == InboundReceived
}

func (thinpostInboundFormat) NewSecret() (string, error) { return thinpostFormat{}.NewSecret() }

type inboundSPF struct {
	Result   string `json:"result"`
	Domain   string `json:"domain,omitempty"`
	Identity string `json:"identity,omitempty"`
}
type inboundDKIM struct {
	Result string `json:"result"`
	Domain string `json:"domain,omitempty"`
}
type inboundDMARC struct {
	Result string `json:"result"`
	Domain string `json:"domain,omitempty"`
	Policy string `json:"policy,omitempty"`
}
type inboundARC struct {
	Present   bool `json:"present"`
	Evaluated bool `json:"evaluated"`
}
type inboundAuth struct {
	SPF   inboundSPF    `json:"spf"`
	DKIM  []inboundDKIM `json:"dkim"`
	DMARC inboundDMARC  `json:"dmarc"`
	ARC   inboundARC    `json:"arc"`
}

// authentication reads the stored document, falling back to the result columns
// when it is missing or unreadable. ARC is never evaluated.
func (in *InboundInput) authentication() inboundAuth {
	var a inboundAuth
	var stored struct {
		SPF   inboundSPF      `json:"spf"`
		DKIM  []inboundDKIM   `json:"dkim"`
		DMARC inboundDMARC    `json:"dmarc"`
		ARC   json.RawMessage `json:"arc"`
	}
	if len(in.Authentication) > 0 && json.Unmarshal(in.Authentication, &stored) == nil && stored.SPF.Result != "" && stored.DMARC.Result != "" {
		a.SPF, a.DKIM, a.DMARC = stored.SPF, stored.DKIM, stored.DMARC
		var arc struct {
			Present bool `json:"present"`
		}
		if json.Unmarshal(stored.ARC, &arc) == nil {
			a.ARC.Present = arc.Present
		}
	} else {
		a.SPF.Result, a.DMARC.Result = in.SPF, in.DMARC
		if in.DKIM != "" && in.DKIM != "none" {
			a.DKIM = []inboundDKIM{{Result: in.DKIM}}
		}
	}
	if a.SPF.Result == "" {
		a.SPF.Result = "none"
	}
	if a.DMARC.Result == "" {
		a.DMARC.Result = "none"
	}
	if a.DKIM == nil {
		a.DKIM = []inboundDKIM{}
	}
	return a
}

type inboundRecipient struct {
	Address   string `json:"address"`
	LocalPart string `json:"local_part"`
	Domain    string `json:"domain"`
	Tag       string `json:"tag"`
}
type inboundTLS struct {
	Version string `json:"version,omitempty"`
	Cipher  string `json:"cipher,omitempty"`
}
type inboundEnvelope struct {
	MailFrom string      `json:"mail_from"`
	RcptTo   []string    `json:"rcpt_to"`
	ClientIP string      `json:"client_ip,omitempty"`
	Helo     string      `json:"helo,omitempty"`
	TLS      *inboundTLS `json:"tls"`
}
type inboundCorrelationBody struct {
	Matched     bool   `json:"matched"`
	MessageID   string `json:"message_id,omitempty"`
	DeliveryID  string `json:"delivery_id,omitempty"`
	Header      string `json:"header,omitempty"`
	FromMatches *bool  `json:"from_matches_recipient,omitempty"`
}
type inboundBody struct {
	ID             string                 `json:"id"`
	ReceivedAt     time.Time              `json:"received_at"`
	Recipient      inboundRecipient       `json:"recipient"`
	Envelope       inboundEnvelope        `json:"envelope"`
	Headers        inboundHeaders         `json:"headers"`
	Text           string                 `json:"text,omitempty"`
	HTML           string                 `json:"html,omitempty"`
	Omitted        []string               `json:"omitted"`
	Raw            string                 `json:"raw"`
	Authentication inboundAuth            `json:"authentication"`
	Correlation    inboundCorrelationBody `json:"correlation"`
	Classification inboundClass           `json:"classification"`
}
type inboundEvent struct {
	ID         string      `json:"id"`
	Outcome    string      `json:"outcome"`
	OccurredAt time.Time   `json:"occurred_at"`
	AppID      string      `json:"app_id"`
	Inbound    inboundBody `json:"inbound"`
}

func splitRecipient(address string) (local, domain, tag string) {
	local = address
	if i := strings.LastIndexByte(address, '@'); i >= 0 {
		local, domain = address[:i], strings.ToLower(address[i+1:])
	}
	if i := strings.IndexByte(local, '+'); i >= 0 {
		tag = local[i+1:]
	}
	return local, domain, tag
}

// testInbound is the fixed sample of the console's test send: reserved
// addresses and addresses from documentation ranges, and no real content.
func testInbound(now time.Time) *InboundInput {
	const zero = "00000000000000000000000000000000"
	raw := "From: Sender <sender@example.org>\r\nTo: inbound-test@example.com\r\nSubject: Thinpost test message\r\nMessage-ID: <test@example.org>\r\nDate: Thu, 05 Nov 2026 12:00:00 +0000\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nThis is a test message sent by Thinpost. It is not real mail.\r\n"
	return &InboundInput{
		ID: zero, ReceivedAt: now, Recipient: "inbound-test@example.com", Recipients: []string{"inbound-test@example.com"},
		MailFrom: "sender@example.org", ClientIP: "192.0.2.1", Helo: "mail.example.org", TLSVersion: "TLS1.3", TLSCipher: "TLS_AES_128_GCM_SHA256",
		Receiver: "mx.example.com", Raw: []byte(raw), SPF: "none", DKIM: "none", DMARC: "none",
	}
}

// The large fields of the payload (the two bodies and the base64 message) are
// marshaled as short placeholders and spliced in afterwards, straight into one
// buffer of the final size: they are never held again as a second copy. Inside a
// JSON string a quote is always escaped, so the exact text `"key":"@@NAME@@"`
// can occur only as that field.
const (
	textPlaceholder = "@@TEXT@@"
	htmlPlaceholder = "@@HTML@@"
	rawPlaceholder  = "@@RAW@@"
)

const hexDigits = "0123456789abcdef"

// jsonEscapedLen is the length of s as the inside of a JSON string, as
// encoding/json writes it without HTML escaping. s must be valid UTF-8.
func jsonEscapedLen(s string) int {
	n := len(s)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\' || c == '\n' || c == '\r' || c == '\t' || c == '\b' || c == '\f':
			n++
		case c < 0x20:
			n += 5
		case c == 0xE2 && i+2 < len(s) && s[i+1] == 0x80 && s[i+2]&^1 == 0xA8:
			n += 3
		}
	}
	return n
}

// shortEscape is the letter of a two-byte JSON escape.
func shortEscape(c byte) byte {
	switch c {
	case '\n':
		return 'n'
	case '\r':
		return 'r'
	case '\t':
		return 't'
	case '\b':
		return 'b'
	}
	return 'f'
}

// writeJSONString writes s as the inside of a JSON string, as encoding/json does.
func writeJSONString(out *bytes.Buffer, s string) {
	start := 0
	flush := func(i int) { out.WriteString(s[start:i]) }
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			flush(i)
			out.WriteByte('\\')
			out.WriteByte(c)
		case c == '\n' || c == '\r' || c == '\t' || c == '\b' || c == '\f':
			flush(i)
			out.WriteByte('\\')
			out.WriteByte(shortEscape(c))
		case c < 0x20:
			flush(i)
			out.WriteString(`\u00`)
			out.WriteByte(hexDigits[c>>4])
			out.WriteByte(hexDigits[c&0xF])
		case c == 0xE2 && i+2 < len(s) && s[i+1] == 0x80 && s[i+2]&^1 == 0xA8:
			flush(i)
			out.WriteString(`\u202`)
			out.WriteByte(hexDigits[s[i+2]&0xF])
			i += 2
		default:
			continue
		}
		start = i + 1
	}
	flush(len(s))
}

// splicePart replaces `"key":"placeholder"` with `"key":"<content>"`.
type splicePart struct {
	key, placeholder string
	size             int // bytes of the content
	write            func(*bytes.Buffer) error
}

// assemble returns skeleton with each part spliced in, in the order they occur
// in the skeleton, in a buffer allocated once at the final size.
func assemble(skeleton []byte, parts []splicePart) ([]byte, error) {
	total := len(skeleton)
	type at struct{ from, to int }
	spans := make([]at, len(parts))
	last := 0
	for i, p := range parts {
		marker := []byte(`"` + p.key + `":"` + p.placeholder + `"`)
		pos := bytes.Index(skeleton, marker)
		if pos < last || bytes.LastIndex(skeleton, marker) != pos {
			return nil, errInvalidInput
		}
		spans[i] = at{pos + len(marker) - len(p.placeholder) - 1, pos + len(marker) - 1}
		last = spans[i].to
		total += p.size - len(p.placeholder)
	}
	out := bytes.NewBuffer(make([]byte, 0, total))
	prev := 0
	for i, p := range parts {
		out.Write(skeleton[prev:spans[i].from])
		if err := p.write(out); err != nil {
			return nil, err
		}
		prev = spans[i].to
	}
	out.Write(skeleton[prev:])
	return out.Bytes(), nil
}

func rawPart(prefix []byte, stored [][]byte) splicePart {
	n := len(prefix)
	for _, seg := range stored {
		n += len(seg)
	}
	return splicePart{key: "raw", placeholder: rawPlaceholder, size: base64.StdEncoding.EncodedLen(n),
		write: func(out *bytes.Buffer) error {
			enc := base64.NewEncoder(base64.StdEncoding, out)
			if _, err := enc.Write(prefix); err != nil {
				return err
			}
			for _, seg := range stored {
				if _, err := enc.Write(seg); err != nil {
					return err
				}
			}
			return enc.Close()
		}}
}

func stringPart(key, placeholder, value string) splicePart {
	return splicePart{key: key, placeholder: placeholder, size: jsonEscapedLen(value), write: func(out *bytes.Buffer) error {
		writeJSONString(out, value)
		return nil
	}}
}

func (thinpostInboundFormat) Build(in Input) (Request, error) {
	if len(in.Secret) < minThinpostSecret || in.Now.IsZero() || !opaque(in.Event.ID) {
		return Request{}, errInvalidInput
	}
	m := in.Inbound
	if in.Test {
		m = testInbound(in.Now)
	}
	if m == nil || m.Recipient == "" || m.ReceivedAt.IsZero() || len(m.Raw) == 0 || !opaque(m.ID) {
		return Request{}, errInvalidInput
	}
	_, drop, prefix := inboundRender(m)
	auth := m.authentication()
	parsed := parseInbound(prefix, m.Raw, drop)
	local, domain, tag := splitRecipient(m.Recipient)
	rcpts := m.Recipients
	if len(rcpts) == 0 {
		rcpts = []string{m.Recipient}
	}
	body := inboundBody{
		ID: m.ID, ReceivedAt: m.ReceivedAt,
		Recipient:      inboundRecipient{Address: m.Recipient, LocalPart: local, Domain: domain, Tag: tag},
		Envelope:       inboundEnvelope{MailFrom: m.MailFrom, RcptTo: rcpts, ClientIP: m.ClientIP, Helo: m.Helo},
		Headers:        parsed.Headers,
		Omitted:        parsed.Omitted,
		Raw:            rawPlaceholder,
		Authentication: auth,
		Correlation:    inboundCorrelationBody{},
		Classification: parsed.Classification,
	}
	if m.TLSVersion != "" || m.TLSCipher != "" {
		body.Envelope.TLS = &inboundTLS{Version: m.TLSVersion, Cipher: m.TLSCipher}
	}
	if c := m.Correlation; c != nil && c.MessageID != "" {
		body.Correlation = inboundCorrelationBody{Matched: true, MessageID: c.MessageID, DeliveryID: c.DeliveryID, Header: c.Header, FromMatches: c.FromMatches}
	}
	var parts []splicePart
	if parsed.Text != "" {
		body.Text = textPlaceholder
		parts = append(parts, stringPart("text", textPlaceholder, parsed.Text))
	}
	if parsed.HTML != "" {
		body.HTML = htmlPlaceholder
		parts = append(parts, stringPart("html", htmlPlaceholder, parsed.HTML))
	}
	parts = append(parts, rawPart(prefix, keptSegments(m.Raw, drop)))
	skeleton, err := marshal(inboundEvent{ID: in.Event.ID, Outcome: InboundReceived, OccurredAt: m.ReceivedAt, AppID: in.AppID, Inbound: body})
	if err != nil {
		return Request{}, err
	}
	payload, err := assemble(skeleton, parts)
	if err != nil {
		return Request{}, err
	}
	timestamp := strconv.FormatInt(in.Now.Unix(), 10)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Thinpost-Event-ID", in.Event.ID)
	h.Set("X-Thinpost-Timestamp", timestamp)
	h.Set("X-Thinpost-Signature", "v1="+thinpostMAC(in.Secret, timestamp, in.Event.ID, payload))
	if in.Test {
		h.Set("X-Thinpost-Test", "true")
	}
	return Request{Body: payload, Header: h}, nil
}

// RenderPeakBytes is the memory one render and send of a stored message of the
// given size holds, counting the message itself; the inbound dispatcher reserves
// it from its budget. The parts, with S the stored size:
//
//   - S: the message, resident (read and decrypted before the render);
//   - 4/3 S: the payload body, which carries the message as base64;
//   - 12 MiB: the two decoded bodies of at most 1 MiB, JSON-escaped in the body at
//     up to six bytes per byte (a body of control characters is the worst case);
//   - 8 MiB: headers, the skeleton and parse buffers, with margin.
//
// Forged Authentication-Results fields are dropped by span, not copied out, and
// a part header over 64 KiB leaves the bodies out instead of being parsed, so
// hostile messages stay below these parts. TestRenderPeakStaysWithinTheBudgetFormula
// measures real peaks, hostile cases included: the worst is about 38.9 MiB for a
// 10 MiB message (bodies of control characters), against 43.8 MiB here.
func RenderPeakBytes(stored int) int64 { return int64(stored)*7/3 + 20<<20 }
