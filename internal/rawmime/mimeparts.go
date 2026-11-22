package rawmime

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/quotedprintable"
	"sort"
	"strings"
	"unicode/utf8"

	preparedrecipe "github.com/zyno-io/thinpost/internal/prepared"
)

// Reason says why a message has no HTML part that can be rewritten. The values
// are constants safe to log and to put in an oplog line; none quotes content.
type Reason string

const (
	// ReasonNone is the empty Reason: at least one part is eligible.
	ReasonNone Reason = ""
	// ReasonNotMIME: no top-level MIME-Version field.
	ReasonNotMIME Reason = "not_mime"
	// ReasonSignedOrEncrypted: multipart/signed, multipart/encrypted or
	// application/(x-)pkcs7-mime anywhere. The whole message stays untouched.
	ReasonSignedOrEncrypted Reason = "signed_or_encrypted"
	// ReasonMalformed: a structure the walker will not trust (see HTMLParts).
	ReasonMalformed Reason = "malformed"
	// ReasonNoHTML: nothing in the message is an HTML part in the first place
	// (or only attachments), or the structure holds a multipart subtype the
	// walker does not enter.
	ReasonNoHTML Reason = "no_html"
	// ReasonUnsupportedEncoding: HTML parts exist but their transfer encoding is
	// not usable or the content does not decode.
	ReasonUnsupportedEncoding Reason = "unsupported_encoding"
	// ReasonUnsupportedCharset: HTML parts exist but their charset is not
	// us-ascii or utf-8, or the decoded content is not valid UTF-8.
	ReasonUnsupportedCharset Reason = "unsupported_charset"
	// ReasonTooLarge: HTML parts exist but each decodes to over MaxHTMLBytes.
	ReasonTooLarge Reason = "too_large"
	// ReasonTooManyParts: more than MaxMIMEParts parts or MaxMIMEDepth levels.
	ReasonTooManyParts Reason = "too_many_parts"
)

// Bounds of the MIME walker.
const (
	// MaxHTMLBytes is the largest decoded HTML part that is rewritten.
	MaxHTMLBytes = 2 << 20
	// MaxMIMEDepth is the deepest multipart nesting the walker enters.
	MaxMIMEDepth = 8
	// MaxMIMEParts is the most parts (leaf or multipart) the walker visits.
	MaxMIMEParts = 128
)

// Canonical replacement header values for a rewritten HTML part.
const (
	HTMLContentType      = "text/html; charset=utf-8"
	HTMLTransferEncoding = "quoted-printable"
)

// Splice is one byte replacement in source coordinates (replace raw[Start:End]
// with Data). It is the form prepared.New takes.
type Splice = preparedrecipe.Replacement

// Span is a byte range [Start, End) of the source.
type Span struct{ Start, End int }

// HTMLPart is one eligible text/html part. All offsets are source coordinates
// (offsets into the raw bytes given to HTMLParts).
type HTMLPart struct {
	// Start and End bound the part's encoded content, excluding the line break
	// that precedes the next boundary delimiter. For a top-level single-part
	// message the content runs to the end of the source.
	Start, End int
	// ContentType and TransferEncoding are the byte ranges of those header
	// fields, whole fields (name through the line terminator, folded lines
	// included). An absent Content-Transfer-Encoding is a zero-length span at
	// the insertion point (the end of the Content-Type field for a part, the
	// end of the top-level fields otherwise); HasCTE is false then.
	ContentType, TransferEncoding Span
	HasCTE                        bool
	// TopLevel is set for a single-part top-level text/html message. Its header
	// fields are not spliced: Replace returns a TopLevelHTML for the top-level
	// header surgery instead.
	TopLevel bool
	// HTML is the decoded content, with every line break CRLF (a bare LF is
	// widened). Replace(HTML) round-trips exactly.
	HTML string
}

// TopLevelHTML holds the two top-level field values a single-part HTML message
// needs after its body is replaced. The caller applies them in the top-level
// header surgery, replacing the existing fields or adding them.
type TopLevelHTML struct {
	ContentType, TransferEncoding string
}

// Replace returns the splices that make this part carry html, quoted-printable
// encoded with lines of at most 76 octets and CRLF breaks.
//
// A multipart leaf gets splices for its Content-Type and Content-Transfer-
// Encoding fields (canonical values; the second may be a zero-length insertion)
// and its content; the content has no trailing CRLF, the boundary line's
// preceding break supplies it. A top-level part returns only the content
// splice, ending with a CRLF (or a soft break when html has no final line
// break, so it decodes to exactly html), and a non-nil TopLevelHTML. Splices
// are in source order, disjoint, lie after the body start and on line
// boundaries: none divides a CRLF. Data is CRLF-only.
func (p HTMLPart) Replace(html string) ([]Splice, *TopLevelHTML, error) {
	if p.Start < 0 || p.End < p.Start || len(html) > MaxRawBytes {
		return nil, nil, ErrInvalid
	}
	body := Splice{Start: p.Start, End: p.End, Data: encodeQP(nil, html, p.TopLevel)}
	if p.TopLevel {
		return []Splice{body}, &TopLevelHTML{HTMLContentType, HTMLTransferEncoding}, nil
	}
	ct := Splice{Start: p.ContentType.Start, End: p.ContentType.End, Data: []byte("Content-Type: " + HTMLContentType + "\r\n")}
	cte := Splice{Start: p.TransferEncoding.Start, End: p.TransferEncoding.End, Data: []byte("Content-Transfer-Encoding: " + HTMLTransferEncoding + "\r\n")}
	out := []Splice{ct, cte, body}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	for i := 1; i < len(out); i++ {
		if out[i].Start < out[i-1].End {
			return nil, nil, ErrInvalid
		}
	}
	return out, nil, nil
}

// encodeQP appends the quoted-printable encoding of s to dst. Line breaks
// (CRLF or LF) become hard CRLF breaks, a lone CR is =0D, and white space
// before a break or at the end is encoded. Soft breaks keep every line within
// 76 octets, the "=" included. A "-" that would begin an output line is
// written as =2D so that decoded content can never form a boundary delimiter. With final set the output always ends with a
// line terminator: a hard break already there, or else a soft break, so the
// decoded bytes are unchanged.
func encodeQP(dst []byte, s string, final bool) []byte {
	const hexd = "0123456789ABCDEF"
	col := 0
	isBreak := func(i int) (n int) {
		if i < len(s) && s[i] == '\n' {
			return 1
		}
		if i+1 < len(s) && s[i] == '\r' && s[i+1] == '\n' {
			return 2
		}
		return 0
	}
	for i := 0; i < len(s); {
		if n := isBreak(i); n > 0 {
			dst = append(dst, '\r', '\n')
			col = 0
			i += n
			continue
		}
		c := s[i]
		i++
		var tok [3]byte
		w := 0
		switch {
		case c == ' ' || c == '\t':
			if i >= len(s) || isBreak(i) > 0 {
				tok = [3]byte{'=', hexd[c>>4], hexd[c&15]}
				w = 3
			} else {
				tok[0], w = c, 1
			}
		case c >= 33 && c <= 126 && c != '=':
			tok[0], w = c, 1
		default:
			tok = [3]byte{'=', hexd[c>>4], hexd[c&15]}
			w = 3
		}
		if col+w > 75 {
			dst = append(dst, '=', '\r', '\n')
			col = 0
		}
		// No output line may start with "-": "--boundary" at a line start would
		// be read as a MIME delimiter. Every splice starts at a line start, and
		// a soft break starts a new line, so col == 0 here is exactly the set of
		// places a line begins (after the soft-break decision above).
		if c == '-' && col == 0 {
			tok = [3]byte{'=', '2', 'D'}
			w = 3
		}
		dst = append(dst, tok[:w]...)
		col += w
	}
	if final && !(len(s) > 0 && isBreakEnd(s)) {
		dst = append(dst, '=', '\r', '\n')
	}
	return dst
}

func isBreakEnd(s string) bool { return s[len(s)-1] == '\n' }

// partHeader is what the walker needs from one part's (or the message's)
// header block.
type partHeader struct {
	hdrEnd    int // where the fields end (the blank line, or the part end)
	bodyStart int // first content byte; == hdrEnd when there is no separator
	sep       bool

	ct, cte, cd    field
	nCT, nCTE, nCD int
	nMIMEVersion   int
	hasContentLen  bool
	head           []byte // raw[:hdrEnd]
}

// readPartHeader parses the header block of the part whose text is
// raw[start:end]. A part may be headers only (no blank line).
func readPartHeader(raw []byte, start, end int) (ph partHeader, err error) {
	ph.hdrEnd, ph.bodyStart = end, end
	region := raw[:end]
	for pos := start; pos < end; {
		content, next := lineEnd(region, pos)
		if content == pos {
			ph.hdrEnd, ph.bodyStart, ph.sep = pos, next, true
			break
		}
		pos = next
	}
	ph.head = raw[:ph.hdrEnd]
	n := 0
	for pos := start; pos < ph.hdrEnd; {
		f, _, ferr := nextField(ph.head, pos)
		if ferr != nil {
			return ph, ErrInvalid
		}
		if n++; n > MaxFields {
			return ph, ErrTooLarge
		}
		switch {
		case nameIs(ph.head, f, "Content-Type"):
			ph.ct, ph.nCT = f, ph.nCT+1
		case nameIs(ph.head, f, "Content-Transfer-Encoding"):
			ph.cte, ph.nCTE = f, ph.nCTE+1
		case nameIs(ph.head, f, "Content-Disposition"):
			ph.cd, ph.nCD = f, ph.nCD+1
		case nameIs(ph.head, f, "MIME-Version"):
			ph.nMIMEVersion++
		case nameIs(ph.head, f, "Content-Length"):
			ph.hasContentLen = true
		}
		pos = f.end
	}
	return ph, nil
}

type walker struct {
	raw   []byte
	parts int
	out   []HTMLPart
	skip  Reason // first per-part skip reason, if any
	whole Reason // set when the whole message is untracked
}

// HTMLParts finds the HTML parts of a message that can be rewritten (plan D6).
// raw must be the bytes s was produced from.
//
// Whole-message rule: when the message as a whole cannot be trusted, nothing is
// returned and the Reason says why: ReasonNotMIME (no top-level MIME-Version),
// ReasonSignedOrEncrypted (multipart/signed or /encrypted, application/pkcs7-mime
// or x-pkcs7-mime anywhere), ReasonMalformed (several top-level or per-part
// Content-Type / Content-Transfer-Encoding fields, an unparsable part header, a
// multipart without a usable boundary or without its closing delimiter),
// ReasonTooManyParts (over MaxMIMEParts parts or MaxMIMEDepth levels) and
// ReasonNoHTML for a multipart subtype other than mixed, alternative or
// related. The walker descends only into those three; it never enters message/*.
//
// Per-part rule: a part that is merely not eligible is skipped and the rest of
// the message is still used: an attachment or named HTML file, a Content-Length
// field, a transfer encoding that is not 7bit/8bit/quoted-printable/base64 or
// does not decode (ReasonUnsupportedEncoding), a charset other than
// us-ascii/utf-8 or content that is not UTF-8 (ReasonUnsupportedCharset), a
// decoded size over MaxHTMLBytes (ReasonTooLarge), an HTML part with unparsable
// parameters or no header/body separator (ReasonMalformed). When at least one
// part is eligible the Reason is ReasonNone. When none is, the Reason is the
// first per-part skip reason in message order, or ReasonNoHTML.
func HTMLParts(raw []byte, s *Scanned) ([]HTMLPart, Reason) {
	if s == nil || s.Size != len(raw) || s.HeaderEnd > s.BodyStart || s.BodyStart > len(raw) {
		return nil, ReasonMalformed
	}
	w := &walker{raw: raw}
	ph, err := readPartHeader(raw, 0, s.HeaderEnd)
	if err != nil {
		return nil, ReasonMalformed
	}
	if ph.nMIMEVersion == 0 {
		return nil, ReasonNotMIME
	}
	// The top-level header block ends at HeaderEnd, which readPartHeader found
	// again; the body starts where Scan says.
	ph.bodyStart, ph.sep = s.BodyStart, s.BodyStart > s.HeaderEnd
	if w.part(ph, len(raw), 0, true); w.whole != ReasonNone {
		return nil, w.whole
	}
	if len(w.out) == 0 {
		if w.skip != ReasonNone {
			return nil, w.skip
		}
		return nil, ReasonNoHTML
	}
	return w.out, ReasonNone
}

func (w *walker) noteSkip(r Reason) {
	if w.skip == ReasonNone {
		w.skip = r
	}
}

// part handles one part (or the top-level message): ph is its header, its
// content runs from ph.bodyStart to end.
func (w *walker) part(ph partHeader, end, depth int, top bool) {
	if w.parts++; w.parts > MaxMIMEParts {
		w.whole = ReasonTooManyParts
		return
	}
	if ph.nCT > 1 || ph.nCTE > 1 {
		w.whole = ReasonMalformed
		return
	}
	mt, params := "text/plain", map[string]string(nil)
	if ph.nCT == 1 {
		var err error
		mt, params, err = mime.ParseMediaType(unfoldValue(ph.head, ph.ct))
		if err != nil {
			switch {
			case isSignedOrEncrypted(mt):
				w.whole = ReasonSignedOrEncrypted
			case strings.HasPrefix(mt, "multipart/"):
				w.whole = ReasonMalformed
			case mt == "text/html":
				w.noteSkip(ReasonMalformed)
			}
			return // anything else is not HTML and nothing the walker needs
		}
	}
	switch {
	case isSignedOrEncrypted(mt):
		w.whole = ReasonSignedOrEncrypted
	case strings.HasPrefix(mt, "multipart/"):
		switch mt {
		case "multipart/mixed", "multipart/alternative", "multipart/related":
			if ph.hasContentLen {
				// A length that no longer matches a rewritten body.
				w.whole = ReasonMalformed
				return
			}
			w.multipart(ph.bodyStart, end, params["boundary"], depth+1)
		default:
			w.whole = ReasonNoHTML
		}
	case mt == "text/html":
		w.html(ph, end, params, top)
	}
}

func isSignedOrEncrypted(mt string) bool {
	switch mt {
	case "multipart/signed", "multipart/encrypted", "application/pkcs7-mime", "application/x-pkcs7-mime":
		return true
	}
	return false
}

// multipart splits raw[start:end] at its boundary delimiter lines and visits
// every part.
func (w *walker) multipart(start, end int, boundary string, depth int) {
	if depth > MaxMIMEDepth {
		w.whole = ReasonTooManyParts
		return
	}
	if !validBoundary(boundary) {
		w.whole = ReasonMalformed
		return
	}
	delim := "--" + boundary
	region := w.raw[:end]
	partStart, closed := -1, false
	for pos := start; pos < end && !closed; {
		content, next := lineEnd(region, pos)
		isDelim, isClose := delimiterLine(w.raw[pos:content], delim)
		if isDelim {
			if partStart >= 0 {
				// The break before the delimiter belongs to it, not to the part.
				pend := pos
				if pend > partStart && w.raw[pend-1] == '\n' {
					pend--
					if pend > partStart && w.raw[pend-1] == '\r' {
						pend--
					}
				}
				if !w.child(partStart, pend, depth) {
					return
				}
			}
			partStart, closed = next, isClose
		}
		pos = next
	}
	if !closed {
		w.whole = ReasonMalformed
	}
}

// child visits the part whose text is raw[start:end]; false stops the walk.
func (w *walker) child(start, end, depth int) bool {
	ph, err := readPartHeader(w.raw, start, end)
	if err != nil {
		w.whole = ReasonMalformed
		return false
	}
	w.part(ph, end, depth, false)
	return w.whole == ReasonNone
}

// delimiterLine reports whether line (without its break) is the boundary
// delimiter "--boundary" or the close delimiter "--boundary--", each followed
// only by transport padding (spaces and tabs).
func delimiterLine(line []byte, delim string) (isDelim, isClose bool) {
	if len(line) < len(delim) || string(line[:len(delim)]) != delim {
		return false, false
	}
	rest := line[len(delim):]
	if bytes.HasPrefix(rest, []byte("--")) {
		isClose, rest = true, rest[2:]
	}
	for _, c := range rest {
		if c != ' ' && c != '\t' {
			return false, false
		}
	}
	return true, isClose
}

// validBoundary applies RFC 2046: 1 to 70 bchars, not ending in a space.
func validBoundary(b string) bool {
	if len(b) < 1 || len(b) > 70 || b[len(b)-1] == ' ' {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case strings.IndexByte("'()+_,-./:=? ", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// html judges a text/html leaf and records it when eligible.
func (w *walker) html(ph partHeader, end int, params map[string]string, top bool) {
	if !ph.sep {
		// Headers only: nowhere to put content.
		w.noteSkip(ReasonMalformed)
		return
	}
	if _, named := params["name"]; named || ph.hasContentLen || ph.nCD > 1 {
		return
	}
	if _, named := params["filename"]; named {
		return
	}
	if ph.nCD == 1 {
		disp, dparams, err := mime.ParseMediaType(unfoldValue(ph.head, ph.cd))
		if err != nil {
			w.noteSkip(ReasonMalformed)
			return
		}
		_, filename := dparams["filename"]
		_, name := dparams["name"]
		if disp != "inline" || filename || name {
			return
		}
	}
	enc := "7bit"
	if ph.nCTE == 1 {
		enc = strings.ToLower(unfoldValue(ph.head, ph.cte))
	}
	switch enc {
	case "7bit", "8bit", "quoted-printable", "base64":
	default:
		w.noteSkip(ReasonUnsupportedEncoding)
		return
	}
	if cs, ok := params["charset"]; ok {
		switch strings.ToLower(cs) {
		case "us-ascii", "utf-8":
		default:
			w.noteSkip(ReasonUnsupportedCharset)
			return
		}
	}
	content := w.raw[ph.bodyStart:end]
	if len(content) > MaxHTMLBytes*3+4096 { // QP can triple, base64 grows by a third
		w.noteSkip(ReasonTooLarge)
		return
	}
	decoded, ok := decodeBody(content, enc)
	if !ok {
		w.noteSkip(ReasonUnsupportedEncoding)
		return
	}
	if len(decoded) > MaxHTMLBytes {
		w.noteSkip(ReasonTooLarge)
		return
	}
	if !utf8.Valid(decoded) {
		w.noteSkip(ReasonUnsupportedCharset)
		return
	}
	p := HTMLPart{Start: ph.bodyStart, End: end, TopLevel: top, HTML: string(widenLF(decoded)), HasCTE: ph.nCTE == 1}
	p.ContentType = Span{ph.ct.start, ph.ct.end}
	if p.HasCTE {
		p.TransferEncoding = Span{ph.cte.start, ph.cte.end}
	} else if top {
		p.TransferEncoding = Span{ph.hdrEnd, ph.hdrEnd}
	} else {
		p.TransferEncoding = Span{ph.ct.end, ph.ct.end}
	}
	w.out = append(w.out, p)
}

// decodeBody decodes content by its transfer encoding.
func decodeBody(content []byte, enc string) ([]byte, bool) {
	switch enc {
	case "quoted-printable":
		b, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(content)))
		return b, err == nil
	case "base64":
		clean := make([]byte, 0, len(content))
		for _, c := range content {
			if c != '\r' && c != '\n' && c != ' ' && c != '\t' {
				clean = append(clean, c)
			}
		}
		clean = bytes.TrimRight(clean, "=")
		b, err := base64.RawStdEncoding.DecodeString(string(clean))
		return b, err == nil
	}
	return content, true
}

// widenLF turns every bare LF into CRLF.
func widenLF(b []byte) []byte {
	if bytes.IndexByte(b, '\n') < 0 {
		return b
	}
	out := make([]byte, 0, len(b)+bytes.Count(b, []byte{'\n'}))
	for i, c := range b {
		if c == '\n' && (i == 0 || b[i-1] != '\r') {
			out = append(out, '\r')
		}
		out = append(out, c)
	}
	return out
}
