// Package rawmime validates and prepares a raw RFC 5322 message submitted over
// SMTP. Scan checks the bounds and content policy and collects the facts the
// policy needs; DecideFrom, Kinds and Prepare apply that policy; PreparedSize
// measures the result exactly. The package never logs and its errors are
// constant sentinels that never quote the message.
package rawmime

import (
	"bytes"
	"errors"
)

// Errors are constants: none carries or quotes message content.
var (
	// ErrInvalid reports a message or argument that is malformed.
	ErrInvalid = errors.New("invalid message")
	// ErrContentNotSupported reports content outside the wire policy: non-ASCII
	// or NUL header bytes, bare CR, NUL in the body, or an over-long line.
	ErrContentNotSupported = errors.New("content not supported")
	// ErrTooLarge reports a message, header block or field count over its bound.
	ErrTooLarge = errors.New("message too large")
	// ErrFromNotPermitted reports a From the app's policy does not allow.
	ErrFromNotPermitted = errors.New("from not permitted")
	// ErrMisconfigured reports a From policy that cannot produce a usable
	// sender: an unverified effective domain or an invalid configured address.
	ErrMisconfigured = errors.New("sender policy misconfigured")
	// ErrSigning reports that DKIM signing failed or produced an unexpected size.
	ErrSigning = errors.New("signing failed")
)

// Bounds from the submission plan.
const (
	// MaxRawBytes is 7.5 MiB, the largest raw message.
	MaxRawBytes = 7_864_320
	// MaxHeaderBlock is the largest header block (without the blank line).
	MaxHeaderBlock = 56 << 10
	// MaxFields is the largest number of header fields.
	MaxFields = 200
	// MaxLine is the largest physical line, excluding its line ending.
	MaxLine = 998
)

// Scan checks raw against the bounds and content policy and returns its facts.
// The body is scanned in place and never copied; the headers are normalized
// into a small copy for decoding.
func Scan(raw []byte) (*Scanned, error) {
	if len(raw) == 0 {
		return nil, ErrInvalid
	}
	if len(raw) > MaxRawBytes {
		return nil, ErrTooLarge
	}
	hdrEnd, bodyStart, err := scanHeaderLines(raw)
	if err != nil {
		return nil, err
	}
	s := &Scanned{Size: len(raw), HeaderEnd: hdrEnd, BodyStart: bodyStart}
	if err := s.collect(raw[:hdrEnd]); err != nil {
		return nil, err
	}
	bare, err := scanBody(raw[bodyStart:])
	if err != nil {
		return nil, err
	}
	s.BareLF = bare
	return s, nil
}

// lineEnd returns where the line starting at pos ends (before CRLF or LF) and
// where the next line starts.
func lineEnd(b []byte, pos int) (content, next int) {
	i := bytes.IndexByte(b[pos:], '\n')
	if i < 0 {
		return len(b), len(b)
	}
	content, next = pos+i, pos+i+1
	if i > 0 && b[content-1] == '\r' {
		content--
	}
	return content, next
}

// scanHeaderLines finds the end of the header block (the first empty line, under
// any line-ending mix) and enforces the byte policy and size bounds on every
// header line. It returns the offset where the fields end and where the body
// starts. A message with no empty line is all headers.
func scanHeaderLines(raw []byte) (hdrEnd, bodyStart int, err error) {
	pos := 0
	for pos < len(raw) {
		content, next := lineEnd(raw, pos)
		if content == pos {
			// An empty line (CRLF or LF) ends the headers.
			if pos > MaxHeaderBlock {
				return 0, 0, ErrTooLarge
			}
			return pos, next, nil
		}
		if content-pos > MaxLine {
			return 0, 0, ErrContentNotSupported
		}
		blank := true
		for _, c := range raw[pos:content] {
			if c == 0 || c >= 0x80 || c == '\r' {
				return 0, 0, ErrContentNotSupported
			}
			blank = blank && (c == ' ' || c == '\t')
		}
		if blank {
			return 0, 0, ErrInvalid // a whitespace-only line is neither a field nor a continuation
		}
		if next > MaxHeaderBlock {
			return 0, 0, ErrTooLarge
		}
		pos = next
	}
	return len(raw), len(raw), nil
}

// scanBody enforces the body policy in place and returns the number of bare
// LFs, each of which preparation turns into CRLF.
func scanBody(body []byte) (bare int, err error) {
	if bytes.IndexByte(body, 0) >= 0 {
		return 0, ErrContentNotSupported
	}
	crlf := 0
	for pos := 0; ; {
		i := bytes.IndexByte(body[pos:], '\r')
		if i < 0 {
			break
		}
		at := pos + i
		if at+1 >= len(body) || body[at+1] != '\n' {
			return 0, ErrContentNotSupported
		}
		crlf++
		pos = at + 1
	}
	for pos := 0; pos < len(body); {
		content, next := lineEnd(body, pos)
		if content-pos > MaxLine {
			return 0, ErrContentNotSupported
		}
		pos = next
	}
	return bytes.Count(body, []byte{'\n'}) - crlf, nil
}

// field is one header field's byte range within the raw header block.
type field struct {
	start int // first byte of the field name
	name  int // end of the name, before trailing whitespace and the colon
	value int // first byte after the colon
	end   int // after the last line's terminator, so including any continuation lines
}

// nextField reads the field starting at pos. head is the header block, already
// checked by scanHeaderLines.
func nextField(head []byte, pos int) (f field, ok bool, err error) {
	f.start = pos
	content, next := lineEnd(head, pos)
	line := head[pos:content]
	if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
		return f, false, ErrInvalid // a leading continuation line
	}
	colon := bytes.IndexByte(line, ':')
	if colon <= 0 {
		return f, false, ErrInvalid
	}
	end := colon
	for end > 0 && (line[end-1] == ' ' || line[end-1] == '\t') {
		end--
	}
	if end == 0 {
		return f, false, ErrInvalid
	}
	for _, c := range line[:end] {
		if c < 33 || c > 126 {
			return f, false, ErrInvalid
		}
	}
	f.name, f.value = pos+end, pos+colon+1
	for next < len(head) && (head[next] == ' ' || head[next] == '\t') {
		_, next = lineEnd(head, next)
	}
	f.end = next
	return f, true, nil
}

// eachField calls fn for every field of the header block, in order.
func eachField(head []byte, fn func(i int, f field) error) error {
	n := 0
	for pos := 0; pos < len(head); {
		f, _, err := nextField(head, pos)
		if err != nil {
			return err
		}
		if n++; n > MaxFields {
			return ErrTooLarge
		}
		if err := fn(n-1, f); err != nil {
			return err
		}
		pos = f.end
	}
	return nil
}

// unfoldValue returns the field value with the folding line breaks removed.
func unfoldValue(head []byte, f field) string {
	var out []byte
	for pos := f.value; pos < f.end; {
		content, next := lineEnd(head, pos)
		out = append(out, head[pos:content]...)
		pos = next
	}
	return string(bytes.TrimSpace(out))
}

func nameIs(head []byte, f field, name string) bool {
	return equalFoldASCII(head[f.start:f.name], name)
}

func equalFoldASCII(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := range b {
		c, d := b[i], s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if d >= 'A' && d <= 'Z' {
			d += 'a' - 'A'
		}
		if c != d {
			return false
		}
	}
	return true
}

func hasPrefixFoldASCII(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && equalFoldASCII(b[:len(prefix)], prefix)
}
