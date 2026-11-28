package inbound

import (
	"errors"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// MaxBytes is the largest inbound message. The Edge enforces it first; control
// refuses anything larger as a second line of defense.
const MaxBytes = 10 << 20

// MaxRecipients bounds the envelope recipients of one message.
const MaxRecipients = 50

// Message is one inbound message as the Edge's SMTP receiver observed it. Every
// field is untrusted input.
type Message struct {
	ID       string
	MailFrom string
	Rcpts    []string
	ClientIP string
	Helo     string
	TLS      TLSInfo
	Receiver string
	Received time.Time
	Group    uint32
	Body     []byte
}

// TLSInfo is the TLS session of the SMTP connection, empty when not encrypted.
type TLSInfo struct{ Version, Cipher string }

// envelope is a Message after validation and sanitizing.
type envelope struct {
	id       string
	mailFrom string
	rcpts    []string
	domains  []string
	clientIP netip.Addr
	helo     string
	tlsVer   string
	tlsCiph  string
	receiver string
}

var errInvalidEnvelope = errors.New("invalid inbound envelope")

func validHexID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// SanitizeHelo bounds a client-supplied HELO/EHLO name for storage: printable
// ASCII only (anything else is dropped), at most 255 bytes. An empty result is
// stored as NULL.
func SanitizeHelo(helo string) string {
	var b strings.Builder
	for i := 0; i < len(helo) && b.Len() < 255; i++ {
		if c := helo[i]; c > ' ' && c < 0x7f {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// printable keeps a short ASCII label (a TLS version or cipher name) or drops it.
func printable(s string, max int) string {
	if len(s) > max {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < ' ' || c >= 0x7f {
			return ""
		}
	}
	return s
}

// normalizeAddress validates an SMTP address (no angle brackets): printable
// ASCII, one @ separating a non-empty local part from a domain, at most 320
// bytes, and returns it with a lowercased domain.
func normalizeAddress(address string) (string, bool) {
	if len(address) < 3 || len(address) > 320 {
		return "", false
	}
	for i := 0; i < len(address); i++ {
		if c := address[i]; c <= ' ' || c >= 0x7f {
			return "", false
		}
	}
	at := strings.LastIndexByte(address, '@')
	if at <= 0 || at == len(address)-1 {
		return "", false
	}
	domain := strings.ToLower(address[at+1:])
	if strings.ContainsAny(domain, "@,:[]!%") {
		return "", false
	}
	return address[:at+1] + domain, true
}

func addressDomain(address string) string {
	return address[strings.LastIndexByte(address, '@')+1:]
}

// normalize validates and sanitizes the envelope. Recipients are deduplicated
// case-insensitively, keeping the first spelling, and at least one is required.
func normalize(m Message) (envelope, error) {
	var e envelope
	if !validHexID(m.ID) || len(m.Rcpts) == 0 || len(m.Rcpts) > MaxRecipients {
		return e, errInvalidEnvelope
	}
	e.id = m.ID
	if m.MailFrom != "" {
		from, ok := normalizeAddress(m.MailFrom)
		if !ok {
			return e, errInvalidEnvelope
		}
		e.mailFrom = from
	}
	seen := make(map[string]struct{}, len(m.Rcpts))
	domains := make(map[string]struct{}, len(m.Rcpts))
	for _, rcpt := range m.Rcpts {
		address, ok := normalizeAddress(rcpt)
		if !ok {
			return e, errInvalidEnvelope
		}
		key := strings.ToLower(address)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		e.rcpts = append(e.rcpts, address)
		domains[addressDomain(address)] = struct{}{}
	}
	for domain := range domains {
		e.domains = append(e.domains, domain)
	}
	sort.Strings(e.domains)
	if m.ClientIP != "" {
		ip, err := netip.ParseAddr(m.ClientIP)
		if err != nil {
			return e, errInvalidEnvelope
		}
		e.clientIP = ip.Unmap()
	}
	e.helo = SanitizeHelo(m.Helo)
	e.tlsVer, e.tlsCiph = printable(m.TLS.Version, 32), printable(m.TLS.Cipher, 128)
	e.receiver = strings.ToLower(printable(m.Receiver, 255))
	return e, nil
}
