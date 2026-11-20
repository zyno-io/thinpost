package rawmime

import (
	"net/mail"
	"strings"
)

// maxName bounds the display name, matching the recorded sender name column.
const maxName = 998

// FromMode is the app's From policy mode (apps.smtp_from_mode).
type FromMode string

// The From policy modes. The empty mode is treated as FromOff.
const (
	FromOff     FromMode = "off"
	FromDomain  FromMode = "domain"
	FromAddress FromMode = "address"
)

// Policy mirrors the apps.smtp_from_* columns.
type Policy struct {
	Mode FromMode
	// Domain is smtp_from_domain, the target T of FromDomain mode.
	Domain string
	// KeepOrigin is smtp_from_keep_origin: generate local+origin-domain@T.
	KeepOrigin bool
	// Address is smtp_from_address: the fixed sender of FromAddress mode, and
	// the fallback of FromDomain mode.
	Address string
	// OnlyUnauthorized is smtp_from_only_unauthorized: keep a From whose domain
	// the app has verified instead of rewriting it.
	OnlyUnauthorized bool
}

// FromDecision is the effective From of a submission.
type FromDecision struct {
	// Value is the single-line rendering mail.Address{Name, Addr}.String(),
	// always, even for an original that was folded on the wire.
	Value string
	// Field is the folded wire field "From: ...\r\n". It is set only when the
	// field is rewritten or inserted; an original From keeps its own bytes.
	Field string
	// Address is the effective addr-spec and Name its display name.
	Address, Name string
	// Domain is the effective domain, lowercase: the sender domain.
	Domain string
	// Rewritten reports that the From field in the message is replaced or
	// inserted (Field is set).
	Rewritten bool
	// Original is the address of the source mailbox S when Rewritten, else "".
	Original string
	// StripSender reports that the Sender field must be removed.
	StripSender bool
}

// DecideFrom applies the app's From policy. mailFrom is the SMTP MAIL FROM
// address ("" or "<>" for the null path). verified reports whether a lowercase
// domain is an active, verified sender domain of the app (exact match).
func DecideFrom(s *Scanned, mailFrom string, p Policy, verified func(domain string) bool) (FromDecision, error) {
	if s == nil || verified == nil {
		return FromDecision{}, ErrInvalid
	}
	var src *mail.Address // the source mailbox S
	missing := false
	switch {
	case s.FromFields == 0:
		if mf := strings.TrimSpace(mailFrom); mf == "" || mf == "<>" {
			missing = true
		} else if a := parseMailbox(mf); a != nil && isASCII(a.Address) {
			src = &mail.Address{Address: a.Address}
		} else {
			missing = true
		}
	case s.From != nil && isASCII(s.From.Address):
		src = s.From
	}
	authorized := src != nil && verified(domainOf(src.Address))
	insert := s.FromFields == 0

	var addr, name string
	rewritten := true
	switch p.Mode {
	case FromOff, "":
		if !authorized {
			if missing {
				return FromDecision{}, ErrInvalid
			}
			return FromDecision{}, ErrFromNotPermitted
		}
		rewritten = false
	case FromDomain, FromAddress:
		if authorized && p.OnlyUnauthorized {
			rewritten = false
			break
		}
		if src != nil {
			name = src.Name
		}
		var err error
		if p.Mode == FromDomain {
			addr, err = generateAddress(src, p)
		} else {
			addr, err = configuredAddress(p.Address)
		}
		if err != nil {
			return FromDecision{}, err
		}
	default:
		return FromDecision{}, ErrMisconfigured
	}
	if !rewritten {
		addr, name = src.Address, src.Name
	}

	name = cleanText(name, maxName)
	d := FromDecision{
		Address: addr,
		Name:    name,
		Domain:  domainOf(addr),
		Value:   (&mail.Address{Name: name, Address: addr}).String(),
	}
	if strings.ContainsAny(d.Value, "\r\n") {
		return FromDecision{}, ErrInvalid
	}
	if back, err := mail.ParseAddress(d.Value); err != nil || back.Address != addr {
		return FromDecision{}, ErrInvalid
	}
	if !verified(d.Domain) {
		return FromDecision{}, ErrMisconfigured
	}
	if rewritten || insert {
		field, err := FoldField("From", d.Value)
		if err != nil {
			return FromDecision{}, err
		}
		d.Field, d.Rewritten = field, true
		if src != nil {
			d.Original = src.Address
		}
	}
	d.StripSender = d.Rewritten || len(s.SenderDomains) > 1
	for _, sd := range s.SenderDomains {
		if sd == "" || !verified(sd) {
			d.StripSender = true
		}
	}
	return d, nil
}

// generateAddress builds the domain-mode sender local@T from S, or falls back
// to the configured address. It fails with ErrFromNotPermitted when neither
// works and ErrMisconfigured when T or the fallback is not a valid generated
// address.
func generateAddress(src *mail.Address, p Policy) (string, error) {
	target := p.Domain
	if !validLDH(target) {
		return "", ErrMisconfigured
	}
	if src != nil {
		local := src.Address[:strings.LastIndexByte(src.Address, '@')]
		origin := domainOf(src.Address)
		if isDotAtom(local) && len(local) <= 64 {
			if p.KeepOrigin && origin != target && validLDH(origin) {
				if kept := local + "+" + origin; len(kept) <= 64 && isDotAtom(kept) {
					local = kept
				}
			}
			return local + "@" + target, nil
		}
	}
	if p.Address == "" {
		return "", ErrFromNotPermitted
	}
	return configuredAddress(p.Address)
}

// configuredAddress validates a configured sender: an ASCII dot-atom local part
// of at most 64 octets at a lowercase LDH domain, 254 octets at most.
func configuredAddress(a string) (string, error) {
	at := strings.LastIndexByte(a, '@')
	if at <= 0 || len(a) > 254 || at > 64 || !isDotAtom(a[:at]) || !validLDH(a[at+1:]) {
		return "", ErrMisconfigured
	}
	return a, nil
}

// domainOf returns the lowercase domain of an addr-spec.
func domainOf(address string) string {
	return strings.ToLower(address[strings.LastIndexByte(address, '@')+1:])
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func isAtext(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-/=?^_`{|}~", c) >= 0
}

// isDotAtom is the RFC 5322 dot-atom text, as in internal/submission.
func isDotAtom(s string) bool {
	if s == "" {
		return false
	}
	prevDot := true
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '.':
			if prevDot {
				return false
			}
			prevDot = true
		case isAtext(s[i]):
			prevDot = false
		default:
			return false
		}
	}
	return !prevDot
}

// validLDH reports a lowercase letters-digits-hyphen domain of dot-separated
// labels of 1 to 63 octets, 253 octets at most.
func validLDH(d string) bool {
	if d == "" || len(d) > 253 {
		return false
	}
	for _, label := range strings.Split(d, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			if c := label[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
