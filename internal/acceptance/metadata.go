package acceptance

import (
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/zyno-io/thinpost/internal/submission"
)

// Bounds mirror the schema 28 CHECK constraints. Every value is validated here
// and again by the repository so a CHECK violation, whose diagnostics would
// quote the failing row into the database log, can never fire.
const (
	maxRecordedSubjectBytes = 998
	maxRecordedAddressBytes = 998
	maxSenderDomainBytes    = 253
	maxSenderNameBytes      = 998
)

// recordedMetadata is the operator-visible metadata stored with a message.
// Its contents are never logged or placed in an error string.
type recordedMetadata struct {
	Subject        string
	Recipients     []string
	RecipientKinds []string
	SenderDomain   string
	// SenderAddress is the bare From address, case preserved. SenderName is
	// the decoded display name, empty when none.
	SenderAddress string
	SenderName    string
}

func newRecordedMetadata(m submission.Message) (recordedMetadata, error) {
	var out recordedMetadata
	if len(m.Subject) == 0 || len(m.Subject) > maxRecordedSubjectBytes || !utf8.ValidString(m.Subject) || strings.ContainsAny(m.Subject, "\x00\r\n") {
		return out, submission.ErrInvalidMessage
	}
	recipients, err := envelopeRecipients(m.Envelope())
	if err != nil {
		return out, err
	}
	from, err := mail.ParseAddress(m.From)
	if err != nil {
		return out, submission.ErrInvalidMessage
	}
	domain, err := senderDomain(from)
	if err != nil {
		return out, err
	}
	if len(from.Address) < 3 || len(from.Address) > maxRecordedAddressBytes || !utf8.ValidString(from.Address) || strings.IndexByte(from.Address, 0) >= 0 {
		return out, submission.ErrInvalidMessage
	}
	return recordedMetadata{Subject: m.Subject, Recipients: recipients, RecipientKinds: m.RecipientKinds(), SenderDomain: domain, SenderAddress: from.Address, SenderName: recordedSenderName(from.Name)}, nil
}

// recordedSenderName cleans a decoded display name instead of rejecting the
// message: RFC 2047 can decode to NUL, CR, LF or invalid UTF-8 that the raw
// header never contained, and such mail was accepted before schema 31. Those
// become spaces or U+FFFD; a name still out of bounds is not recorded.
func recordedSenderName(name string) string {
	name = strings.ToValidUTF8(name, "\uFFFD")
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == 0 || r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, name))
	if len(name) > maxSenderNameBytes {
		return ""
	}
	return name
}

// envelopeRecipients returns the bare envelope address of each To, Cc and Bcc
// entry in submission order, exactly as dispatch derives it. Duplicates are kept.
func envelopeRecipients(to []string) ([]string, error) {
	if len(to) < 1 || len(to) > submission.MaxEnvelopeRecipients {
		return nil, submission.ErrInvalidMessage
	}
	out := make([]string, len(to))
	for i, raw := range to {
		address, err := mail.ParseAddress(raw)
		if err != nil {
			return nil, submission.ErrInvalidMessage
		}
		value := address.Address
		at := strings.LastIndexByte(value, '@')
		lowered := strings.ToLower(value)
		if len(value) < 3 || len(value) > maxRecordedAddressBytes || len(lowered) > maxRecordedAddressBytes || at < 1 || at == len(value)-1 || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return nil, submission.ErrInvalidMessage
		}
		out[i] = value
	}
	return out, nil
}

// senderDomain is the lowercased text after the last "@" of the From address.
func senderDomain(address *mail.Address) (string, error) {
	at := strings.LastIndexByte(address.Address, '@')
	if at < 1 || at == len(address.Address)-1 {
		return "", submission.ErrInvalidMessage
	}
	domain := strings.ToLower(address.Address[at+1:])
	if len(domain) < 1 || len(domain) > maxSenderDomainBytes || !utf8.ValidString(domain) || strings.IndexByte(domain, 0) >= 0 {
		return "", submission.ErrInvalidMessage
	}
	return domain, nil
}
