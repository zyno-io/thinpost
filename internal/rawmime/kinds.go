package rawmime

import (
	"net/mail"
	"strings"
)

// MailboxKey is the identity of a mailbox for envelope de-duplication, the
// replay digest and To/Cc kind matching: the local part as written and the
// domain lowercased. It follows internal/recipientpolicy's address key. It
// accepts what mail.ParseAddress accepts, ignoring any display name.
func MailboxKey(value string) (string, bool) {
	if strings.ContainsAny(value, "\r\n") {
		return "", false
	}
	a, err := mail.ParseAddress(value)
	if err != nil {
		return "", false
	}
	return addressKey(a.Address)
}

// addressKey keys a parsed address (the Address field of a mail.Address).
func addressKey(address string) (string, bool) {
	at := strings.LastIndexByte(address, '@')
	if at <= 0 || at == len(address)-1 {
		return "", false
	}
	return address[:at] + "@" + strings.ToLower(address[at+1:]), true
}

// DotAtomLocalPart reports whether local is a plain dot-atom local part: one or
// more runs of printable ASCII atext characters separated by single dots. A
// local part that needs quoting (a quote, a backslash, a space, ...) is not one.
func DotAtomLocalPart(local string) bool {
	if local == "" {
		return false
	}
	prevDot := true // a leading dot is invalid
	for i := 0; i < len(local); i++ {
		c := local[i]
		switch {
		case c == '.':
			if prevDot {
				return false
			}
			prevDot = true
			continue
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte("!#$%&'*+-/=?^_`{|}~", c) >= 0:
		default:
			return false
		}
		prevDot = false
	}
	return !prevDot
}

// PlainRecipient reports whether an envelope recipient is a bare addr-spec with
// a dot-atom local part, so that re-parsing it can never name another mailbox
// than the one the client sent to.
func PlainRecipient(rcpt string) bool {
	at := strings.LastIndexByte(rcpt, '@')
	if at <= 0 || at == len(rcpt)-1 {
		return false
	}
	if !DotAtomLocalPart(rcpt[:at]) {
		return false
	}
	a, err := mail.ParseAddress(rcpt)
	return err == nil && a.Address == rcpt
}

// Kind says how a recipient appears in the message.
type Kind string

// The recipient kinds, in their stored order.
const (
	KindTo  Kind = "to"
	KindCc  Kind = "cc"
	KindBcc Kind = "bcc"
)

// Kinds de-duplicates the envelope recipients by mailbox key (the first
// spelling is kept) and classifies each: to if its exact key is in the To
// header, cc if it is in Cc, otherwise bcc. The result is stably partitioned
// to, then cc, then bcc; kinds[i] belongs to recipients[i]. An unparsable
// recipient, or one that is not a plain addr-spec with a dot-atom local part
// (a quoted local part such as "ops"@example.net), is ErrInvalid.
func Kinds(envelope []string, s *Scanned) (recipients []string, kinds []Kind, err error) {
	if s == nil {
		return nil, nil, ErrInvalid
	}
	seen := make(map[string]struct{}, len(envelope))
	var to, cc, bcc []string
	for _, rcpt := range envelope {
		key, ok := MailboxKey(rcpt)
		if !ok || !PlainRecipient(rcpt) {
			return nil, nil, ErrInvalid
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if _, ok := s.To[key]; ok {
			to = append(to, rcpt)
		} else if _, ok := s.Cc[key]; ok {
			cc = append(cc, rcpt)
		} else {
			bcc = append(bcc, rcpt)
		}
	}
	recipients = make([]string, 0, len(seen))
	kinds = make([]Kind, 0, len(seen))
	for _, group := range []struct {
		kind  Kind
		items []string
	}{{KindTo, to}, {KindCc, cc}, {KindBcc, bcc}} {
		for _, rcpt := range group.items {
			recipients = append(recipients, rcpt)
			kinds = append(kinds, group.kind)
		}
	}
	return recipients, kinds, nil
}
