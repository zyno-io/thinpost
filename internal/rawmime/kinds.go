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
// recipient is ErrInvalid.
func Kinds(envelope []string, s *Scanned) (recipients []string, kinds []Kind, err error) {
	if s == nil {
		return nil, nil, ErrInvalid
	}
	seen := make(map[string]struct{}, len(envelope))
	var to, cc, bcc []string
	for _, rcpt := range envelope {
		key, ok := MailboxKey(rcpt)
		if !ok {
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
