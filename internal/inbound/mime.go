package inbound

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
)

// Bounds on hostile MIME. The raw size is already limited to MaxBytes; these
// limit what parsing a small message can cost.
const (
	maxHeaderBlock = 64 << 10
	maxDepth       = 5
	maxParts       = 100
	maxSubject     = 1024
	maxIDHeader    = 998
	maxAddress     = 320
)

// Parsed holds what control reads from an inbound message. Nothing here is
// trusted: the headers are whatever the sender wrote.
type Parsed struct {
	// FromAddress is the first RFC 5322 From address, lowercased, empty when the
	// message has no parsable From. FromCount counts From fields (more than one
	// makes the From ambiguous and DMARC a permanent error).
	FromAddress string
	FromCount   int
	Subject     string
	MessageID   string
	InReplyTo   string
	References  string
	ARCPresent  bool
	// Unparsable is set when the header block cannot be read within bounds (too
	// large, or no end). The message is still stored; no header is trusted.
	Unparsable bool
	// Truncated is set when the body walk stopped at a limit (depth, part count
	// or a read error). Headers are unaffected.
	Truncated bool
	Parts     int
	Depth     int
	// Classification from the top-level multipart/report type.
	DeliveryStatusReport bool
	FeedbackReport       bool
}

// cleanText makes a value safe for a PostgreSQL text column: valid UTF-8, no
// NUL, no other control characters except tab, cut to max bytes on a rune
// boundary, with surrounding white space trimmed.
func cleanText(s string, max int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r == 0 || r < ' ' && r != '\t' || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

// headerBlockEnd returns the length of the header block and whether it ends
// (a blank line) within maxHeaderBlock bytes.
func headerBlockEnd(raw []byte) (int, bool) {
	limit := min(len(raw), maxHeaderBlock+4)
	head := raw[:limit]
	end := -1
	if i := bytes.Index(head, []byte("\r\n\r\n")); i >= 0 {
		end = i
	}
	if i := bytes.Index(head, []byte("\n\n")); i >= 0 && (end < 0 || i < end) {
		end = i
	}
	if end < 0 {
		// A message that is all headers and fits the bound has no blank line.
		return len(raw), len(raw) <= maxHeaderBlock
	}
	return end, end <= maxHeaderBlock
}

// Parse reads the header block and walks the body structure within bounds. It
// never fails on malformed mail: a field it cannot read stays empty.
func Parse(raw []byte) *Parsed {
	p := &Parsed{}
	if n, ok := headerBlockEnd(raw); !ok || n == 0 {
		p.Unparsable = true
		return p
	}
	entity, err := message.ReadWithOptions(bytes.NewReader(raw), &message.ReadOptions{MaxHeaderBytes: maxHeaderBlock + 16})
	if err != nil && (entity == nil || !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err)) {
		p.Unparsable = true
		return p
	}
	header := mail.Header{Header: entity.Header}
	for fields := header.FieldsByKey("From"); fields.Next(); {
		p.FromCount++
	}
	if p.FromCount == 1 {
		if addresses, err := header.AddressList("From"); err == nil && len(addresses) > 0 {
			if a := strings.ToLower(addresses[0].Address); len(a) > 0 && len(a) <= maxAddress {
				p.FromAddress = cleanText(a, maxAddress)
			}
		}
	}
	if subject, err := header.Text("Subject"); err == nil {
		p.Subject = cleanText(subject, maxSubject)
	} else {
		p.Subject = cleanText(header.Get("Subject"), maxSubject)
	}
	p.MessageID = cleanText(header.Get("Message-ID"), maxIDHeader)
	p.InReplyTo = cleanText(header.Get("In-Reply-To"), 4*maxIDHeader)
	p.References = cleanText(header.Get("References"), 16<<10)
	p.ARCPresent = header.Has("ARC-Seal")
	if media, params, err := header.ContentType(); err == nil && media == "multipart/report" {
		switch strings.ToLower(params["report-type"]) {
		case "delivery-status":
			p.DeliveryStatusReport = true
		case "feedback-report":
			p.FeedbackReport = true
		}
	}
	w := &walker{parsed: p, budget: int64(len(raw))}
	w.walk(entity, 1)
	return p
}

type walker struct {
	parsed *Parsed
	// budget is the decoded bytes the walk may still read, the raw size: no
	// transfer encoding yields more than it was given, so this only stops a
	// misbehaving decoder.
	budget int64
}

func (w *walker) walk(e *message.Entity, depth int) {
	p := w.parsed
	if depth > p.Depth {
		p.Depth = depth
	}
	p.Parts++
	if p.Parts > maxParts || depth > maxDepth {
		p.Truncated = true
		return
	}
	if mr := e.MultipartReader(); mr != nil {
		for {
			part, err := mr.NextPart()
			if part == nil || err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
				if err != nil && !errors.Is(err, io.EOF) {
					p.Truncated = true
				}
				return
			}
			w.walk(part, depth+1)
			if p.Truncated {
				return
			}
		}
	}
	n, err := io.Copy(io.Discard, io.LimitReader(e.Body, w.budget+1))
	w.budget -= n
	if err != nil || w.budget < 0 {
		p.Truncated = true
	}
}
