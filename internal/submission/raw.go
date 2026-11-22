package submission

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/mail"
	"strings"
)

// Source version 3 stores a raw MIME message received over SMTP submission.
//
// The object is: rawSourceMagic, a big-endian uint32 header length (1 to
// MaxRawHeaderLen), a strict JSON RawHeader of that length, then the raw
// message bytes exactly as received. Version 1 and 2 objects are JSON and
// never start with the magic, so a reader that does not know version 3 fails
// closed on it.
//
// This package must not import internal/rawmime (rawmime -> providerid and
// message -> submission would make a cycle risk), so the limits below mirror
// rawmime's; a test in internal/preparation pins them together.
const (
	// RawSourceVersion is the version number of a raw MIME source.
	RawSourceVersion = 3

	rawSourceMagic = "thinpost-source-v3\n"
	// MaxRawHeaderLen bounds the JSON header.
	MaxRawHeaderLen = 65536
	// MaxRawBytes mirrors rawmime.MaxRawBytes.
	MaxRawBytes = 7_864_320
	// MaxRawFromField bounds the folded From field kept for rewrites.
	MaxRawFromField = 8192
)

// RawHeader is the strict JSON header of a version 3 source. It holds exactly
// the decisions made at acceptance that preparation needs to rebuild the
// message deterministically from the raw bytes.
type RawHeader struct {
	// MailFrom is the SMTP MAIL FROM address; "" or "<>" is the null path.
	MailFrom string `json:"mail_from"`
	// Recipients are the de-duplicated envelope recipients (1 to 100) and Kinds
	// the kind of each ("to", "cc" or "bcc"), ordered to, then cc, then bcc.
	// A delivery's recipient index is a position in Recipients.
	Recipients []string `json:"recipients"`
	Kinds      []string `json:"kinds"`
	// From is the effective From value (rendered mailbox) when the From field
	// is rewritten or inserted; "" when the message keeps its own From.
	From string `json:"from"`
	// FromField is the folded wire field "From: ...\r\n" to write when the
	// From is rewritten or inserted (rawmime.FromDecision.Field); "" otherwise.
	FromField string `json:"from_field"`
	// FromRewritten is set exactly when FromField is.
	FromRewritten bool `json:"from_rewritten"`
	// OriginalFrom is the address of the source mailbox that was rewritten away,
	// if there was one, for the record; only with FromRewritten.
	OriginalFrom string `json:"original_from"`
	// StripSender removes any Sender field; always set with FromRewritten.
	StripSender bool `json:"strip_sender"`
	// SenderDomain is the effective From domain, lowercase: the DKIM d= domain
	// and the Message-ID domain.
	SenderDomain string `json:"sender_domain"`
	// EnhancedProtection must equal the message's recorded flag.
	EnhancedProtection bool `json:"enhanced_protection"`
	// RawSHA256 is the lowercase hex SHA-256 of the raw bytes.
	RawSHA256 string `json:"raw_sha256"`
	// TrackOpens and TrackClicks are the open and click tracking decisions for
	// the message, resolved at acceptance: the kinds that survived the message's
	// own X-Thinpost-Track-* headers, the app's defaults, the tracking runtime
	// and the size check. An object written without them (before SMTP tracking,
	// or untracked) prepares untracked, and omitempty keeps it byte-identical
	// when it is encoded again.
	TrackOpens  bool `json:"track_opens,omitempty"`
	TrackClicks bool `json:"track_clicks,omitempty"`
}

// EncodeRawSource encodes a version 3 stored source. It sets h.RawSHA256 from
// raw and applies the same validation as DecodeSource.
func EncodeRawSource(h RawHeader, raw []byte) ([]byte, error) {
	sum := sha256.Sum256(raw)
	h.RawSHA256 = hex.EncodeToString(sum[:])
	if h.validate(h.EnhancedProtection, raw) != nil {
		return nil, ErrInvalidSource
	}
	header, err := json.Marshal(h)
	if err != nil || len(header) == 0 || len(header) > MaxRawHeaderLen {
		return nil, ErrInvalidSource
	}
	out := make([]byte, 0, len(rawSourceMagic)+4+len(header)+len(raw))
	out = append(out, rawSourceMagic...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(header)))
	out = append(out, header...)
	return append(out, raw...), nil
}

// decodeRawSource decodes a version 3 object; the caller has seen the magic.
func decodeRawSource(data []byte, enhancedProtection bool) (Source, error) {
	rest := data[len(rawSourceMagic):]
	if len(rest) < 4 {
		return Source{}, ErrInvalidSource
	}
	n := binary.BigEndian.Uint32(rest)
	rest = rest[4:]
	if n < 1 || n > MaxRawHeaderLen || uint64(n) > uint64(len(rest)) {
		return Source{}, ErrInvalidSource
	}
	var h RawHeader
	decoder := json.NewDecoder(bytes.NewReader(rest[:n]))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&h) != nil || decoder.Decode(new(any)) != io.EOF {
		return Source{}, ErrInvalidSource
	}
	raw := rest[n:]
	if h.validate(enhancedProtection, raw) != nil {
		return Source{}, ErrInvalidSource
	}
	return Source{Version: RawSourceVersion, Raw: raw, RawHeader: h}, nil
}

// rawKey is a mailbox identity for duplicate detection: the local part as
// written and the lowercased domain (the same rule as rawmime.MailboxKey).
func rawKey(value string) (string, bool) {
	if strings.ContainsAny(value, "\r\n") {
		return "", false
	}
	a, err := mail.ParseAddress(value)
	if err != nil {
		return "", false
	}
	at := strings.LastIndexByte(a.Address, '@')
	if at <= 0 || at == len(a.Address)-1 {
		return "", false
	}
	return a.Address[:at] + "@" + strings.ToLower(a.Address[at+1:]), true
}

// plainRawRecipient reports whether a stored recipient is a bare addr-spec with
// a dot-atom local part (mirrors rawmime.PlainRecipient): a quoted local part
// would be re-parsed into a different mailbox.
func plainRawRecipient(rcpt string) bool {
	at := strings.LastIndexByte(rcpt, '@')
	if at <= 0 || at == len(rcpt)-1 {
		return false
	}
	prevDot := true
	for i := 0; i < at; i++ {
		c := rcpt[i]
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
	if prevDot {
		return false
	}
	a, err := mail.ParseAddress(rcpt)
	return err == nil && a.Address == rcpt
}

func validRawDomain(d string) bool {
	if d == "" || len(d) > 253 || d != strings.ToLower(d) {
		return false
	}
	for _, label := range strings.Split(d, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func (h RawHeader) validate(enhancedProtection bool, raw []byte) error {
	if h.EnhancedProtection != enhancedProtection || len(raw) == 0 || len(raw) > MaxRawBytes {
		return ErrInvalidSource
	}
	sum := sha256.Sum256(raw)
	if h.RawSHA256 != hex.EncodeToString(sum[:]) {
		return ErrInvalidSource
	}
	if n := len(h.Recipients); n < 1 || n > MaxEnvelopeRecipients || len(h.Kinds) != n {
		return ErrInvalidSource
	}
	seen := make(map[string]struct{}, len(h.Recipients))
	rank := 0
	for i, rcpt := range h.Recipients {
		key, ok := rawKey(rcpt)
		if !ok || !plainRawRecipient(rcpt) {
			return ErrInvalidSource
		}
		if _, dup := seen[key]; dup {
			return ErrInvalidSource
		}
		seen[key] = struct{}{}
		r := 0
		switch h.Kinds[i] {
		case RecipientTo:
		case RecipientCc:
			r = 1
		case RecipientBcc:
			r = 2
		default:
			return ErrInvalidSource
		}
		if r < rank {
			return ErrInvalidSource
		}
		rank = r
	}
	if h.MailFrom != "" && h.MailFrom != "<>" {
		if _, ok := rawKey(h.MailFrom); !ok || len(h.MailFrom) > 998 {
			return ErrInvalidSource
		}
	}
	if !validRawDomain(h.SenderDomain) {
		return ErrInvalidSource
	}
	if h.FromRewritten != (h.FromField != "") || (h.From != "") != (h.FromField != "") || h.FromRewritten && !h.StripSender {
		return ErrInvalidSource
	}
	if h.FromRewritten {
		key, ok := rawKey(h.From)
		// The field is exactly rawmime.FoldField("From", From): unfolding it (a
		// fold only turns one space into CRLF SP) gives back "From: " + From, so
		// it can carry no other field and no other mailbox.
		if !ok || len(h.FromField) > MaxRawFromField || !strings.HasSuffix(h.FromField, "\r\n") ||
			strings.ReplaceAll(h.FromField, "\r\n", "") != "From: "+h.From ||
			key[strings.LastIndexByte(key, '@')+1:] != h.SenderDomain {
			return ErrInvalidSource
		}
		lines := strings.Split(strings.TrimSuffix(h.FromField, "\r\n"), "\r\n")
		for i, line := range lines {
			if len(line) > 998 || i > 0 && (line == "" || line[0] != ' ' && line[0] != '\t') {
				return ErrInvalidSource
			}
		}
	}
	if h.OriginalFrom != "" {
		if !h.FromRewritten || len(h.OriginalFrom) > 320 || strings.ContainsAny(h.OriginalFrom, "\r\n") {
			return ErrInvalidSource
		}
		// DecideFrom records the bare, unquoted addr-spec (team ops@example.org),
		// which ParseAddress only accepts once rendered again.
		if _, ok := rawKey((&mail.Address{Address: h.OriginalFrom}).String()); !ok {
			return ErrInvalidSource
		}
	}
	return nil
}
