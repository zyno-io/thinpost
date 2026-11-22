package rawmime

import (
	"bytes"
	"crypto"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/providerid"
	"github.com/zyno-io/thinpost/internal/receivedtrace"
)

const maxMessageIDLine = 300

// PrepareInput is everything preparation needs beyond the raw bytes.
type PrepareInput struct {
	// Scan is the result of Scan on the same raw bytes.
	Scan *Scanned
	// From is the decision made at acceptance. Its Domain is the DKIM d= domain
	// and the Message-ID domain.
	From FromDecision
	// MessageID is the message-scoped token for the Message-ID header.
	MessageID string
	// Date supplies the Date header when the message has none. Callers pass the
	// time the message is released for sending: acceptance, or the hold's
	// release. A Date the client sent is always kept.
	Date time.Time
	// Selector is the DKIM selector. Signer signs for From.Domain; PreparedSize
	// only reads its public key, so a SizingSigner works there.
	Selector string
	Signer   crypto.Signer
	// Received is Thinpost's own trace field (receivedtrace.Field), written at
	// the very top of the prepared header block. Empty adds none. It is not in
	// the DKIM h= list, and the client's own Received fields are still removed.
	Received []byte
	// ListUnsubscribe is the Thinpost one-click link, "<https://...>" (one
	// angle-bracketed https URI, printable ASCII, no spaces). Empty leaves the
	// message's own List-Unsubscribe fields as they are. When set, the client's
	// List-Unsubscribe and List-Unsubscribe-Post fields are removed, Thinpost's
	// two fields are written at the end of the header block, and any client DKIM
	// signature whose h= names either field is dropped.
	ListUnsubscribe string
}

// unsubscribePostField is the RFC 8058 field written beside List-Unsubscribe.
const unsubscribePostField = "List-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n"

// generated holds the lines preparation inserts, rendered once up front.
type generated struct {
	messageID []byte // "Message-ID: <...>\r\n"
	date      []byte // "Date: ...\r\n", empty if the message has a Date
	received  []byte // the trace field, empty for none
	// unsubscribe is both one-click fields, empty when the input has none.
	unsubscribe []byte
}

func (in *PrepareInput) check(raw []byte) (generated, error) {
	s := in.Scan
	if s == nil || s.Size != len(raw) || s.HeaderEnd < 0 || s.HeaderEnd > s.BodyStart || s.BodyStart > len(raw) ||
		s.BareLF < 0 || s.BareLF > len(raw)-s.BodyStart || in.Signer == nil {
		return generated{}, ErrInvalid
	}
	if in.From.Domain == "" || in.MessageID == "" || strings.ContainsAny(in.MessageID, " \t\r\n<>@") || in.Date.IsZero() {
		return generated{}, ErrInvalid
	}
	if in.From.Field == "" && s.FromFields != 1 {
		return generated{}, ErrInvalid // only a single original From may be kept
	}
	var g generated
	if len(in.Received) > 0 {
		if !receivedtrace.Valid(in.Received) {
			return generated{}, ErrInvalid
		}
		g.received = in.Received
	}
	if v := in.ListUnsubscribe; v != "" {
		if !validUnsubscribeLink(v) || len("List-Unsubscribe: ")+len(v) > MaxLine {
			return generated{}, ErrInvalid
		}
		g.unsubscribe = []byte("List-Unsubscribe: " + v + "\r\n" + unsubscribePostField)
	}
	g.messageID = []byte("Message-ID: " + providerid.MessageIDHeader(in.MessageID, in.From.Domain) + "\r\n")
	if len(g.messageID) > maxMessageIDLine {
		return generated{}, ErrInvalid
	}
	if !s.HasDate {
		g.date = []byte("Date: " + in.Date.UTC().Format(time.RFC1123Z) + "\r\n")
	}
	return g, nil
}

// validUnsubscribeLink accepts exactly one "<https://host...>" item of
// printable ASCII with no spaces, quotes or further angle brackets.
func validUnsubscribeLink(v string) bool {
	const prefix = "<https://"
	if len(v) <= len(prefix)+1 || v[:len(prefix)] != prefix || v[len(v)-1] != '>' {
		return false
	}
	for i := 1; i < len(v)-1; i++ {
		if c := v[i]; c <= ' ' || c >= 0x7f || c == '<' || c == '>' || c == '"' || c == ',' {
			return false
		}
	}
	return true
}

// Prepare applies the header surgery and returns the message to send:
// DKIM-Signature, the prepared fields, CRLF, the body, with every line ending
// CRLF. It signs with a streaming signer and allocates one output buffer.
// The result is deterministic except for the DKIM t= tag.
func Prepare(raw []byte, in PrepareInput) ([]byte, error) {
	g, err := in.check(raw)
	if err != nil {
		return nil, err
	}
	slack, err := signatureSlack(in.Signer.Public(), in.From.Domain, in.Selector)
	if err != nil {
		return nil, err
	}
	out, hlen, err := assemble(raw, &in, g, slack)
	if err != nil {
		return nil, err
	}
	sig, err := sign(out[slack:], hlen, &in)
	if err != nil {
		return nil, err
	}
	if len(sig) > slack {
		return nil, ErrSigning
	}
	start := slack - len(sig)
	copy(out[start:], sig)
	return out[start:], nil
}

// sink writes into buf, or only counts when buf is nil.
type sink struct {
	buf  []byte
	n    int
	over bool // a write would pass the end of buf; nothing was copied
}

func (k *sink) write(b []byte) {
	if k.buf != nil {
		if k.n+len(b) > len(k.buf) {
			k.over = true
		} else {
			copy(k.buf[k.n:], b)
		}
	}
	k.n += len(b)
}

func (k *sink) crlf() { k.write(crlf) }

var crlf = []byte("\r\n")

// writeField writes a field's lines, each ended with CRLF.
func (k *sink) writeField(head []byte, f field) {
	for pos := f.start; pos < f.end; {
		content, next := lineEnd(head, pos)
		k.write(head[pos:content])
		k.crlf()
		pos = next
	}
}

// assemble builds slack reserved bytes, the prepared header block, CRLF and the
// body into one buffer. It returns the buffer and the header block length.
func assemble(raw []byte, in *PrepareInput, g generated, slack int) ([]byte, int, error) {
	var count sink
	if err := writeHeaders(&count, raw, in, g); err != nil {
		return nil, 0, err
	}
	body := raw[in.Scan.BodyStart:]
	out := make([]byte, slack+count.n+2+len(body)+in.Scan.BareLF)
	k := sink{buf: out, n: slack}
	if err := writeHeaders(&k, raw, in, g); err != nil {
		return nil, 0, err
	}
	if k.over {
		return nil, 0, ErrInvalid
	}
	if longestLine(out[slack:k.n]) > MaxLine {
		return nil, 0, ErrInvalid
	}
	k.crlf()
	for pos := 0; pos < len(body); {
		i := bytes.IndexByte(body[pos:], '\n')
		if i < 0 {
			k.write(body[pos:])
			break
		}
		at := pos + i
		k.write(body[pos:at])
		if at == 0 || body[at-1] != '\r' {
			k.write([]byte{'\r'})
		}
		k.write([]byte{'\n'})
		pos = at + 1
	}
	if k.over || k.n != len(out) {
		return nil, 0, ErrInvalid // the scan does not match the bytes
	}
	return out, count.n, nil
}

// writeHeaders runs the header surgery of the plan, section 5.5.
func writeHeaders(k *sink, raw []byte, in *PrepareInput, g generated) error {
	head := raw[:in.Scan.HeaderEnd]
	rewrite := in.From.Field != ""
	firstFrom, firstID := -1, -1
	// Dropping any client DKIM-Signature changes the DKIM-Signature field set,
	// so every signature whose h= names dkim-signature is invalidated with it.
	dropsSignature := false
	for pos := 0; pos < len(head); {
		f, _, err := nextField(head, pos)
		if err != nil {
			return err
		}
		if !dropsSignature && nameIs(head, f, "DKIM-Signature") && dropSignature(head, f, in, false) {
			dropsSignature = true
		}
		if firstFrom < 0 && nameIs(head, f, "From") {
			firstFrom = f.start
		}
		if firstID < 0 && nameIs(head, f, "Message-ID") {
			firstID = f.start
		}
		pos = f.end
	}
	k.write(g.received)
	inserted := func() {
		k.write(g.messageID)
		k.write(g.date)
	}
	if rewrite && firstFrom < 0 {
		k.write([]byte(in.From.Field))
		if firstID < 0 {
			inserted()
		}
	}
	for pos := 0; pos < len(head); {
		f, _, _ := nextField(head, pos)
		pos = f.end
		switch {
		case nameIs(head, f, "From"):
			switch {
			case !rewrite:
				k.writeField(head, f)
			case f.start == firstFrom:
				k.write([]byte(in.From.Field))
			default:
				continue
			}
			if f.start == firstFrom && firstID < 0 {
				inserted()
			}
		case nameIs(head, f, "Message-ID"):
			if f.start == firstID {
				inserted()
			}
		case dropped(head, f, in, dropsSignature):
		default:
			k.writeField(head, f)
		}
	}
	k.write(g.unsubscribe)
	return nil
}

// dropped reports the fields preparation always removes, besides From and
// Message-ID, which have their own handling.
func dropped(head []byte, f field, in *PrepareInput, dropsSignature bool) bool {
	name := head[f.start:f.name]
	for _, n := range alwaysDropped {
		if equalFoldASCII(name, n) {
			return true
		}
	}
	switch {
	case hasPrefixFoldASCII(name, "X-Thinpost-"):
		return true
	case equalFoldASCII(name, "Sender"):
		return in.From.StripSender
	case equalFoldASCII(name, "List-Unsubscribe"), equalFoldASCII(name, "List-Unsubscribe-Post"):
		return in.ListUnsubscribe != ""
	case equalFoldASCII(name, "DKIM-Signature"):
		return dropSignature(head, f, in, dropsSignature)
	}
	return false
}

var alwaysDropped = [...]string{
	"Bcc", "Resent-Bcc", "Return-Path", "Received",
	"ARC-Seal", "ARC-Message-Signature", "ARC-Authentication-Results", "Authentication-Results",
}

// dropSignature reports a DKIM-Signature that cannot survive the surgery: the
// From was rewritten, its h= names a field preparation removes, replaces or
// inserts, or its h= cannot be read. With dropsSignature set (some client
// signature is dropped), a signature whose h= names DKIM-Signature goes too,
// because the set of DKIM-Signature fields it covered has changed.
func dropSignature(head []byte, f field, in *PrepareInput, dropsSignature bool) bool {
	if in.From.Rewritten {
		return true
	}
	value := head[f.value:f.end]
	found := false
	for len(value) > 0 {
		tag := value
		if i := bytes.IndexByte(value, ';'); i >= 0 {
			tag, value = value[:i], value[i+1:]
		} else {
			value = nil
		}
		tag = trimWSP(tag)
		if len(tag) < 2 || tag[0] != 'h' {
			continue
		}
		if eq := trimWSP(tag[1:]); len(eq) == 0 || eq[0] != '=' {
			continue
		} else {
			tag = eq[1:]
		}
		found = true
		names := 0
		for len(tag) > 0 {
			item := tag
			if i := bytes.IndexByte(tag, ':'); i >= 0 {
				item, tag = tag[:i], tag[i+1:]
			} else {
				tag = nil
			}
			var buf [64]byte
			name, ok := compactName(buf[:0], item)
			if !ok || len(name) == 0 {
				return true
			}
			names++
			if namesChangedField(name, in) || dropsSignature && equalFoldASCII(name, "DKIM-Signature") {
				return true
			}
		}
		if names == 0 {
			return true
		}
	}
	return !found
}

// namesChangedField reports an h= entry naming a field preparation removes,
// replaces or inserts.
func namesChangedField(name []byte, in *PrepareInput) bool {
	for _, n := range alwaysDropped {
		if equalFoldASCII(name, n) {
			return true
		}
	}
	switch {
	case hasPrefixFoldASCII(name, "X-Thinpost-"), equalFoldASCII(name, "Message-ID"):
		return true
	case equalFoldASCII(name, "Sender"):
		return in.From.StripSender
	case equalFoldASCII(name, "From"):
		return in.From.Field != ""
	case equalFoldASCII(name, "Date"):
		return !in.Scan.HasDate
	case equalFoldASCII(name, "List-Unsubscribe"), equalFoldASCII(name, "List-Unsubscribe-Post"):
		return in.ListUnsubscribe != ""
	}
	return false
}

func isWSP(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func trimWSP(b []byte) []byte {
	for len(b) > 0 && isWSP(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && isWSP(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}

// compactName appends item without folding whitespace to dst, which it does not
// grow beyond its capacity.
func compactName(dst, item []byte) ([]byte, bool) {
	for _, c := range item {
		if isWSP(c) {
			continue
		}
		if len(dst) == cap(dst) {
			return nil, false
		}
		dst = append(dst, c)
	}
	return dst, true
}
