// Package providerid renders a Thinpost message ID (32 lowercase hex
// characters, 128 bits) in the shape each provider-compatible adapter returns.
// The submission adapters and the webhook formats call the same functions, so
// the ID a sender receives at submission equals the ID in later webhooks.
// An input that is not a 32-character lowercase hex ID is returned unchanged.
package providerid

import "strings"

func canonical(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func uuid(id string) string {
	if !canonical(id) {
		return id
	}
	return id[0:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
}

// Postmark is the MessageID shape: UUID text of the same 128 bits.
func Postmark(id string) string { return uuid(id) }

// Resend is the id / email_id shape: UUID text of the same 128 bits.
func Resend(id string) string { return uuid(id) }

// MailgunHeader is the Mailgun message-id header shape, "id@domain". The
// domain is the From domain. Without a recorded domain the bare ID is used.
func MailgunHeader(id, domain string) string {
	if domain == "" {
		return id
	}
	return id + "@" + domain
}

// MessageIDHeader is the RFC 5322 Message-ID header value, with angle brackets:
// the message-scoped token plus the lowercased sender domain. It is the single
// implementation of the header, shared by message building and every reader.
func MessageIDHeader(token, domain string) string {
	return "<" + token + "@" + strings.ToLower(domain) + ">"
}

// MailgunResponse is the Mailgun send-response id shape, "<id@domain>".
func MailgunResponse(id, domain string) string {
	if domain == "" {
		return "<" + id + ">"
	}
	return MessageIDHeader(id, domain)
}

// Normalize returns the 32-hex message ID for any rendering above: bare hex,
// UUID text, "id@domain" or "<id@domain>". Input case is ignored.
func Normalize(s string) (string, bool) {
	if strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">") {
		s = s[1 : len(s)-1]
	}
	if at := strings.IndexByte(s, '@'); at >= 0 {
		if at == 0 || at == len(s)-1 || strings.Count(s, "@") != 1 {
			return "", false
		}
		s = s[:at]
	}
	s = strings.ToLower(s)
	if len(s) == 36 && s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-' {
		s = strings.ReplaceAll(s, "-", "")
	}
	if !canonical(s) {
		return "", false
	}
	return s, true
}
