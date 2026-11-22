package rawmime

import (
	"mime"
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message/charset"
	gomail "github.com/emersion/go-message/mail"
)

const (
	maxSubject   = 998
	maxMessageID = 998
	maxAddress   = 320
)

// Scanned is what Scan learned about a message. It is read-only after Scan and
// is tied to the exact bytes it was produced from.
type Scanned struct {
	// Size is the raw length. HeaderEnd is where the last field ends and
	// BodyStart where the body begins (both are len(raw) for a message that is
	// all headers).
	Size, HeaderEnd, BodyStart int
	// BareLF counts body line feeds not preceded by CR; preparation adds a CR
	// to each.
	BareLF int

	// FromFields counts From fields. From is the single mailbox when there is
	// exactly one field holding exactly one mailbox (and no group), else nil.
	FromFields int
	From       *mail.Address
	// SenderPresent is set when a Sender field exists. SenderAddress and
	// SenderDomain (lowercase) are set only if the first Sender is a mailbox.
	SenderPresent bool
	SenderAddress string
	SenderDomain  string
	// SenderDomains has one entry per Sender field, in order: the lowercase
	// domain, or "" if the field is not a single mailbox.
	SenderDomains []string
	Subject       string
	MessageID     string
	HasMessageID  bool
	HasDate       bool
	RequireTLS    bool                // X-Thinpost-Require-TLS is present; callers reject it
	Enhanced      bool                // X-Thinpost-Enhanced-Protection: true
	Hold          bool                // X-Thinpost-Hold: true
	To, Cc        map[string]struct{} // mailbox keys

	// ListUnsubscribe is set when the message carries a List-Unsubscribe or a
	// List-Unsubscribe-Post field. UnsubscribeFields and UnsubscribePostFields
	// count them. UnsubscribeValue and UnsubscribePostValue are the unfolded
	// value of the field when there is exactly one of that name, else "". They
	// are facts only: a malformed value is never an error here, because
	// preparation replaces the fields when the app has one-click on.
	ListUnsubscribe                          bool
	UnsubscribeFields, UnsubscribePostFields int
	UnsubscribeValue, UnsubscribePostValue   string
}

// collect reads the header block, validates the field syntax and fills the facts.
func (s *Scanned) collect(head []byte) error {
	var h gomail.Header
	var enhanced, hold []string
	var subject, messageID string
	var haveSubject, haveMessageID bool
	err := eachField(head, func(_ int, f field) error {
		switch {
		case nameIs(head, f, "From"):
			s.FromFields++
			h.Add("From", unfoldValue(head, f))
		case nameIs(head, f, "Sender"):
			domain := ""
			if a := parseMailbox(unfoldValue(head, f)); a != nil {
				domain = strings.ToLower(a.Address[strings.LastIndexByte(a.Address, '@')+1:])
				if !s.SenderPresent {
					s.SenderAddress, s.SenderDomain = a.Address, domain
				}
			}
			s.SenderPresent = true
			s.SenderDomains = append(s.SenderDomains, domain)
		case nameIs(head, f, "Subject"):
			if !haveSubject {
				subject, haveSubject = unfoldValue(head, f), true
			}
		case nameIs(head, f, "To"):
			h.Add("To", unfoldValue(head, f))
		case nameIs(head, f, "Cc"):
			h.Add("Cc", unfoldValue(head, f))
		case nameIs(head, f, "Message-ID"):
			if !haveMessageID {
				messageID, haveMessageID = unfoldValue(head, f), true
			}
		case nameIs(head, f, "Date"):
			s.HasDate = true
		case nameIs(head, f, "List-Unsubscribe"):
			s.ListUnsubscribe = true
			s.UnsubscribeFields++
			s.UnsubscribeValue = unfoldValue(head, f)
		case nameIs(head, f, "List-Unsubscribe-Post"):
			s.ListUnsubscribe = true
			s.UnsubscribePostFields++
			s.UnsubscribePostValue = unfoldValue(head, f)
		case nameIs(head, f, "X-Thinpost-Require-TLS"):
			s.RequireTLS = true
		case nameIs(head, f, "X-Thinpost-Enhanced-Protection"):
			enhanced = append(enhanced, unfoldValue(head, f))
		case nameIs(head, f, "X-Thinpost-Hold"):
			hold = append(hold, unfoldValue(head, f))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if s.UnsubscribeFields != 1 {
		s.UnsubscribeValue = ""
	}
	if s.UnsubscribePostFields != 1 {
		s.UnsubscribePostValue = ""
	}
	if s.Enhanced, err = flag(enhanced); err != nil {
		return err
	}
	if s.Hold, err = flag(hold); err != nil {
		return err
	}

	if s.FromFields == 1 {
		s.From = parseMailbox(h.Get("From"))
	}
	if decoded, err := decodeSubject(subject); err == nil {
		s.Subject = cleanText(decoded, maxSubject)
	} else {
		s.Subject = cleanText(subject, maxSubject)
	}
	if validMessageID(messageID) {
		s.MessageID, s.HasMessageID = messageID, true
	}
	s.To, s.Cc = addressKeys(h, "To"), addressKeys(h, "Cc")
	return nil
}

// decodeSubject RFC 2047-decodes a header value like mail.Header.Text.
func decodeSubject(v string) (string, error) {
	var h gomail.Header
	h.Add("Subject", v)
	return h.Text("Subject")
}

var addressParser = &mail.AddressParser{WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader}}

// parseMailbox parses exactly one mailbox, like mail.ParseAddress, and rejects
// a group. It returns nil for anything else.
func parseMailbox(value string) *mail.Address {
	if value == "" || strings.ContainsAny(value, "\r\n") || hasGroupColon(value) {
		return nil
	}
	a, err := addressParser.Parse(value)
	if err != nil || len(a.Address) > maxAddress {
		return nil
	}
	if at := strings.LastIndexByte(a.Address, '@'); at <= 0 || at == len(a.Address)-1 {
		return nil
	}
	return a
}

// hasGroupColon reports a ':' outside quotes, comments and angle brackets: the
// start of an RFC 5322 group.
func hasGroupColon(s string) bool {
	quoted, angle, comment := false, false, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && (quoted || comment > 0):
			i++
		case quoted:
			quoted = c != '"'
		case comment > 0:
			if c == '(' {
				comment++
			} else if c == ')' {
				comment--
			}
		case c == '"':
			quoted = true
		case c == '(':
			comment++
		case c == '<':
			angle = true
		case c == '>':
			angle = false
		case c == ':' && !angle:
			return true
		}
	}
	return false
}

// addressKeys returns the mailbox keys of every address in the named fields.
// A field that does not parse contributes nothing.
func addressKeys(h gomail.Header, name string) map[string]struct{} {
	keys := map[string]struct{}{}
	for fields := h.FieldsByKey(name); fields.Next(); {
		list, err := addressParser.ParseList(fields.Value())
		if err != nil {
			continue
		}
		for _, a := range list {
			if key, ok := addressKey(a.Address); ok {
				keys[key] = struct{}{}
			}
		}
	}
	return keys
}

func validMessageID(id string) bool {
	if len(id) < 3 || len(id) > maxMessageID || id[0] != '<' || id[len(id)-1] != '>' {
		return false
	}
	return !strings.ContainsAny(id, " \t\r\n")
}

// cleanText makes a value safe for a PostgreSQL text column: valid UTF-8, no
// NUL, no other control characters except tab, cut to max bytes on a rune
// boundary, with surrounding white space trimmed. It is a copy of the helper in
// internal/inbound, kept separate so this package imports nothing of it.
func cleanText(s string, max int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r == 0 || r < ' ' && r != '\t' || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// flag reads an X-Thinpost-* option header, which may appear at most once and
// must be "true" or "false" (case-insensitive), like the HTTP option headers.
// Absent is false.
func flag(values []string) (bool, error) {
	switch len(values) {
	case 0:
		return false, nil
	case 1:
		switch strings.ToLower(values[0]) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, ErrInvalid
}
