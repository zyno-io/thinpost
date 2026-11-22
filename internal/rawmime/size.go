package rawmime

import "crypto"

// Acceptance limits applied to a Size (plan section 5.7).
const (
	// MaxPreparedMessage is edgeprotocol.MaxMIMEBytes; a prepared message plus
	// SizeSlack must fit.
	MaxPreparedMessage = 8 << 20
	// SizeSlack is the margin acceptance keeps below MaxPreparedMessage.
	SizeSlack = 4096
	// MaxPreparedHeaders bounds the prepared header block plus the DKIM header,
	// under the console's 64 KiB header parser.
	MaxPreparedHeaders = 60 << 10
)

// Size is the exact size of a prepared message.
type Size struct {
	// Headers is the prepared header block, without DKIM, and DKIM the length
	// of the DKIM-Signature field.
	Headers, DKIM int
	// Total is the whole prepared message.
	Total int
}

// Fits reports whether the message is within the acceptance limits.
func (s Size) Fits() bool {
	return s.Total+SizeSlack <= MaxPreparedMessage && s.Headers+s.DKIM <= MaxPreparedHeaders
}

// PreparedSize returns the exact size Prepare would produce for the same key
// size, without building the body: the arithmetic total plus the DKIM header
// measured by signing the prepared header block and an empty body with a
// sizing signer of the same width as in.Signer's key. in.Signer is never asked
// to sign.
func PreparedSize(raw []byte, in PrepareInput) (Size, error) {
	g, err := in.check(raw)
	if err != nil {
		return Size{}, err
	}
	n, err := signatureBytes(in.Signer.Public())
	if err != nil {
		return Size{}, err
	}
	var count sink
	if err := writeHeaders(&count, raw, &in, g); err != nil {
		return Size{}, err
	}
	head := make([]byte, count.n+2)
	k := sink{buf: head}
	if err := writeHeaders(&k, raw, &in, g); err != nil {
		return Size{}, err
	}
	if k.over || longestLine(head[:count.n]) > MaxLine {
		return Size{}, ErrInvalid
	}
	k.crlf()
	var signer crypto.Signer = sizingSigner{pub: in.Signer.Public(), n: n}
	sig, err := signWith(head, head[:count.n], &in, signer)
	if err != nil {
		return Size{}, err
	}
	body := in.bodyLen(raw)
	return Size{Headers: count.n, DKIM: len(sig), Total: len(sig) + count.n + 2 + body}, nil
}
