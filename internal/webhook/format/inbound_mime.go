package format

import (
	"bytes"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-msgauth/authres"
)

// Bounds on parsing a stored inbound message for rendering. The raw size is
// already limited when the message is received; these limit what rendering a
// small hostile message can cost.
const (
	// MaxInboundBody is the largest decoded text or HTML body the payload
	// carries; a larger one is omitted and named in omitted[].
	MaxInboundBody = 1 << 20
	maxHeaderBlock = 64 << 10
	maxDepth       = 5
	maxParts       = 100
	maxHeaderList  = 200
	maxHeaderValue = 1024
	maxReferences  = 100
)

// headerEnd returns the offset of the first empty line: the end of the header
// block, with every field keeping its line terminator. It is len(raw) when the
// message has no blank line (all of it is then headers). It has no size bound;
// callers bound what they do with the result.
func headerEnd(raw []byte) int {
	for i := 0; i < len(raw); {
		if raw[i] == '\n' || raw[i] == '\r' && i+1 < len(raw) && raw[i+1] == '\n' {
			return i
		}
		j := bytes.IndexByte(raw[i:], '\n')
		if j < 0 {
			return len(raw)
		}
		i += j + 1
	}
	return len(raw)
}

// fieldLen is the length of the first header field of head: its line and any
// folded continuation lines, with their terminators.
func fieldLen(head []byte) int {
	n := 0
	for n < len(head) {
		i := bytes.IndexByte(head[n:], '\n')
		if i < 0 {
			return len(head)
		}
		n += i + 1
		if n >= len(head) || head[n] != ' ' && head[n] != '\t' {
			break
		}
	}
	return n
}

// authservID is the authserv-id of an Authentication-Results field value
// (RFC 8601): the first token after any white space and (nested) comments,
// before the first ';', white space or comment, without a trailing dot or
// quotes, lowercased.
func authservID(value string) string {
	i := 0
	for i < len(value) {
		switch c := value[i]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '(':
			depth := 0
			for i < len(value) {
				switch value[i] {
				case '\\':
					i++
				case '(':
					depth++
				case ')':
					depth--
				}
				i++
				if depth == 0 {
					break
				}
			}
		default:
			end := i
			for end < len(value) {
				if c := value[end]; c == ';' || c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '(' {
					break
				}
				end++
			}
			return strings.ToLower(strings.TrimSuffix(strings.Trim(value[i:end], `"`), "."))
		}
	}
	return ""
}

// span is a byte range of the stored message.
type span struct{ from, to int }

// claimWindow bounds how much of an Authentication-Results value is read to find
// its authserv-id. A value whose id is not found within it is treated as
// claiming ours: failing closed costs nothing, and the field is never needed.
const claimWindow = 4096

// claimsReceiver reports whether a header field (its line and folded
// continuations) is, or hides, an Authentication-Results field naming want.
// Some parsers treat a bare CR as a line break, so the field is also checked
// piece by piece after every CR that is not followed by LF.
func claimsReceiver(field []byte, want string) bool {
	for start := 0; start <= len(field); {
		end := len(field)
		next := len(field) + 1
		for i := start; i < len(field); i++ {
			if field[i] == '\r' && (i+1 >= len(field) || field[i+1] != '\n') {
				end, next = i, i+1
				break
			}
		}
		if claimsReceiverPiece(field[start:end], want) {
			return true
		}
		start = next
	}
	return false
}

func claimsReceiverPiece(piece []byte, want string) bool {
	colon := bytes.IndexByte(piece[:min(len(piece), 256)], ':')
	if colon <= 0 || !strings.EqualFold(strings.TrimSpace(string(piece[:colon])), "Authentication-Results") {
		return false
	}
	value := piece[colon+1:]
	truncated := len(value) > claimWindow
	if truncated {
		value = value[:claimWindow]
	}
	id := authservID(string(value))
	return id == want || truncated && id == ""
}

// forgedSpans finds every Authentication-Results field in the header block whose
// authserv-id is ours (RFC 8601 section 5): a sender cannot vouch for results as
// this receiver. Fields of other authserv-ids are untouched, and so is
// everything after the blank line. The header block is scanned to its real end
// however large it is. The spans are whole fields, in order; nothing is copied.
func forgedSpans(raw []byte, receiver string) []span {
	want := strings.ToLower(strings.TrimSuffix(receiver, "."))
	end := headerEnd(raw)
	var drop []span
	for pos := 0; pos < end; {
		n := fieldLen(raw[pos:end])
		if claimsReceiver(raw[pos:pos+n], want) {
			drop = append(drop, span{pos, pos + n})
		}
		pos += n
	}
	return drop
}

// keptSegments is raw without the dropped spans, as slices of raw.
func keptSegments(raw []byte, drop []span) [][]byte {
	out := make([][]byte, 0, len(drop)+1)
	last := 0
	for _, d := range drop {
		if d.from > last {
			out = append(out, raw[last:d.from])
		}
		last = d.to
	}
	if last < len(raw) {
		out = append(out, raw[last:])
	}
	return out
}

func spanBytes(drop []span) int {
	n := 0
	for _, d := range drop {
		n += d.to - d.from
	}
	return n
}

// removeForgedResults returns the message without the forged fields (the input
// itself when there are none) and how many were removed. Rendering uses
// forgedSpans and keptSegments instead, to avoid the copy.
func removeForgedResults(raw []byte, receiver string) ([]byte, int) {
	drop := forgedSpans(raw, receiver)
	if len(drop) == 0 {
		return raw, 0
	}
	out := make([]byte, 0, len(raw)-spanBytes(drop))
	for _, seg := range keptSegments(raw, drop) {
		out = append(out, seg...)
	}
	return out, len(drop)
}

// oversizedPartHeader reports whether a line that starts like a multipart
// delimiter ("--") is followed by more than maxHeaderBlock bytes without a blank
// line: a part header block over the bound, which the parser would otherwise
// read whole. A body that merely has such a line is treated the same way; the
// bodies are then left out, and raw still carries everything.
func oversizedPartHeader(body []byte) bool {
	for i := 0; i < len(body); {
		if bytes.HasPrefix(body[i:], []byte("--")) {
			window := body[i:min(len(body), i+maxHeaderBlock+4)]
			if len(window) > maxHeaderBlock && !bytes.Contains(window, []byte("\n\n")) && !bytes.Contains(window, []byte("\n\r\n")) {
				return true
			}
		}
		j := bytes.IndexByte(body[i:], '\n')
		if j < 0 {
			return false
		}
		i += j + 1
	}
	return false
}

func cleanHeaderText(s string, max int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max]
		for len(s) > 0 && !validUTF8Suffix(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

func validUTF8Suffix(s string) bool { return strings.ToValidUTF8(s, "") == s }

// authResults builds the Authentication-Results value Thinpost vouches for,
// from the results it recorded. It names only what was evaluated.
func authResults(receiver string, a inboundAuth) string {
	value := func(s string) authres.ResultValue {
		switch s {
		case "pass", "fail", "softfail", "neutral", "temperror", "permerror", "none":
			return authres.ResultValue(s)
		}
		return authres.ResultNone
	}
	var results []authres.Result
	spf := &authres.SPFResult{Value: value(a.SPF.Result)}
	if a.SPF.Identity == "helo" {
		spf.Helo = a.SPF.Domain
	} else {
		spf.From = a.SPF.Domain
	}
	results = append(results, spf)
	for _, d := range a.DKIM {
		results = append(results, &authres.DKIMResult{Value: value(d.Result), Domain: d.Domain})
	}
	results = append(results, &authres.DMARCResult{Value: value(a.DMARC.Result), From: a.DMARC.Domain})
	parts := strings.Split(authres.Format(receiver, results), "; ")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ";\r\n\t")
}

// receivedHeader is the trace field Thinpost adds for the hop it received.
func receivedHeader(in *InboundInput, receiver string) string {
	var b strings.Builder
	b.WriteString("Received:")
	if in.Helo != "" || in.ClientIP != "" {
		b.WriteString(" from ")
		if in.Helo != "" {
			b.WriteString(in.Helo)
		} else {
			b.WriteString("unknown")
		}
		if in.ClientIP != "" {
			b.WriteString(" ([" + in.ClientIP + "])")
		}
		b.WriteString("\r\n\t")
	}
	b.WriteString("by " + receiver + " with ")
	if in.TLSVersion != "" {
		b.WriteString("ESMTPS")
	} else {
		b.WriteString("ESMTP")
	}
	b.WriteString(" id " + in.ID + ";\r\n\t" + in.ReceivedAt.UTC().Format(time.RFC1123Z))
	return b.String()
}

// renderPrefix is what Thinpost prepends to the stored message: its
// Authentication-Results and Received fields. The delivered message is the
// prefix followed by the stored bytes after removeForgedResults.
func renderPrefix(in *InboundInput, a inboundAuth, receiver string) []byte {
	return []byte("Authentication-Results: " + authResults(receiver, a) + "\r\n" + receivedHeader(in, receiver) + "\r\n")
}

type address struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

type headerField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type inboundHeaders struct {
	From          []address     `json:"from"`
	To            []address     `json:"to"`
	Cc            []address     `json:"cc"`
	ReplyTo       []address     `json:"reply_to"`
	Subject       string        `json:"subject"`
	Date          string        `json:"date"`
	MessageID     string        `json:"message_id"`
	InReplyTo     string        `json:"in_reply_to"`
	References    []string      `json:"references"`
	AutoSubmitted string        `json:"auto_submitted"`
	HeaderList    []headerField `json:"header_list"`
}

type inboundClass struct {
	AutoSubmitted        bool `json:"auto_submitted"`
	DeliveryStatusReport bool `json:"delivery_status_report"`
	FeedbackReport       bool `json:"feedback_report"`
}

type parsedInbound struct {
	Headers        inboundHeaders
	Text, HTML     string
	Omitted        []string
	Classification inboundClass
}

func addresses(h mail.Header, key string) []address {
	list, err := h.AddressList(key)
	out := make([]address, 0, len(list))
	if err != nil {
		return out
	}
	for _, a := range list {
		if a == nil || a.Address == "" || len(out) >= 100 {
			continue
		}
		out = append(out, address{Name: cleanHeaderText(a.Name, 256), Address: cleanHeaderText(a.Address, 320)})
	}
	return out
}

// listHeader lists the header fields in order (the prefix, then the stored
// message without the dropped fields), at most maxHeaderList fields.
func listHeader(prefix, raw []byte, drop []span) []headerField {
	out := []headerField{}
	add := func(field []byte) {
		// A field can be megabytes long; only its start is ever shown.
		field = field[:min(len(field), 8*maxHeaderValue)]
		colon := bytes.IndexByte(field, ':')
		if colon <= 0 {
			return
		}
		value := strings.Join(strings.Fields(string(field[colon+1:])), " ")
		out = append(out, headerField{Name: cleanHeaderText(string(field[:colon]), 128), Value: cleanHeaderText(value, maxHeaderValue)})
	}
	for head := prefix; len(head) > 0 && len(out) < maxHeaderList; {
		n := fieldLen(head)
		add(head[:n])
		head = head[n:]
	}
	end := headerEnd(raw)
	for pos, di := 0, 0; pos < end && len(out) < maxHeaderList; {
		n := fieldLen(raw[pos:end])
		if di < len(drop) && drop[di].from == pos {
			di++
		} else {
			add(raw[pos : pos+n])
		}
		pos += n
	}
	return out
}

// parseInbound reads the headers and the first text and HTML bodies of the
// message within bounds. It never fails: a field it cannot read stays empty,
// and a body it cannot decode to UTF-8 or that is larger than MaxInboundBody is
// omitted and named in Omitted.
//
// prefix is what Thinpost prepends; raw is the received message and drop the
// forged fields to leave out. The header-size bound applies to raw: Thinpost's own
// fields never push a message over it.
func parseInbound(prefix, raw []byte, drop []span) parsedInbound {
	p := parsedInbound{Headers: inboundHeaders{From: []address{}, To: []address{}, Cc: []address{}, ReplyTo: []address{}, References: []string{}, HeaderList: []headerField{}}, Omitted: []string{}}
	if headerEnd(raw)-spanBytes(drop) > maxHeaderBlock {
		p.Omitted = append(p.Omitted, "headers")
		return p
	}
	readers := []io.Reader{bytes.NewReader(prefix)}
	for _, seg := range keptSegments(raw, drop) {
		readers = append(readers, bytes.NewReader(seg))
	}
	reader := io.MultiReader(readers...)
	entity, err := message.ReadWithOptions(reader, &message.ReadOptions{MaxHeaderBytes: int64(maxHeaderBlock + 16 + len(prefix))})
	if err != nil && (entity == nil || !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err)) {
		p.Omitted = append(p.Omitted, "headers")
		return p
	}
	header := mail.Header{Header: entity.Header}
	h := &p.Headers
	h.From, h.To, h.Cc, h.ReplyTo = addresses(header, "From"), addresses(header, "To"), addresses(header, "Cc"), addresses(header, "Reply-To")
	if subject, err := header.Text("Subject"); err == nil {
		h.Subject = cleanHeaderText(subject, 1024)
	} else {
		h.Subject = cleanHeaderText(header.Get("Subject"), 1024)
	}
	h.Date = cleanHeaderText(header.Get("Date"), 128)
	h.MessageID = cleanHeaderText(header.Get("Message-ID"), 998)
	h.InReplyTo = cleanHeaderText(header.Get("In-Reply-To"), 4*998)
	for i, ref := range strings.Fields(cleanHeaderText(header.Get("References"), 16<<10)) {
		if i >= maxReferences {
			break
		}
		h.References = append(h.References, ref)
	}
	h.AutoSubmitted = cleanHeaderText(header.Get("Auto-Submitted"), 128)
	h.HeaderList = listHeader(prefix, raw, drop)
	p.Classification.AutoSubmitted = h.AutoSubmitted != "" && !strings.EqualFold(strings.SplitN(h.AutoSubmitted, ";", 2)[0], "no")
	if media, params, err := header.ContentType(); err == nil && media == "multipart/report" {
		switch strings.ToLower(params["report-type"]) {
		case "delivery-status":
			p.Classification.DeliveryStatusReport = true
		case "feedback-report":
			p.Classification.FeedbackReport = true
		}
	}
	if oversizedPartHeader(raw[headerEnd(raw):]) {
		p.Omitted = append(p.Omitted, "text", "html")
		return p
	}
	w := &bodyWalker{}
	w.walk(entity, 1, err)
	p.Text, p.HTML = w.text, w.html
	if w.omitText {
		p.Omitted = append(p.Omitted, "text")
	}
	if w.omitHTML {
		p.Omitted = append(p.Omitted, "html")
	}
	return p
}

type bodyWalker struct {
	text, html         string
	haveText, haveHTML bool
	omitText, omitHTML bool
	parts              int
}

// readBody returns the decoded body, or ok false when it cannot be decoded (the
// charset or transfer encoding is unknown) or is larger than MaxInboundBody.
func readBody(e *message.Entity, decodeErr error) (string, bool) {
	if decodeErr != nil {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(e.Body, MaxInboundBody+1))
	if err != nil || len(data) > MaxInboundBody {
		return "", false
	}
	return strings.ToValidUTF8(string(data), "�"), true
}

func (w *bodyWalker) done() bool { return w.parts > maxParts || w.haveText && w.haveHTML }

func (w *bodyWalker) walk(e *message.Entity, depth int, decodeErr error) {
	w.parts++
	if w.done() || depth > maxDepth {
		return
	}
	if mr := e.MultipartReader(); mr != nil {
		for !w.done() {
			part, err := mr.NextPart()
			if part == nil || err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
				return
			}
			w.walk(part, depth+1, err)
		}
		return
	}
	if disposition, _, err := e.Header.ContentDisposition(); err == nil && strings.EqualFold(disposition, "attachment") {
		return
	}
	media, _, err := e.Header.ContentType()
	if err != nil {
		media = "text/plain"
	}
	switch media {
	case "text/plain":
		if !w.haveText {
			w.haveText = true
			var ok bool
			if w.text, ok = readBody(e, decodeErr); !ok {
				w.text, w.omitText = "", true
			}
		}
	case "text/html":
		if !w.haveHTML {
			w.haveHTML = true
			var ok bool
			if w.html, ok = readBody(e, decodeErr); !ok {
				w.html, w.omitHTML = "", true
			}
		}
	}
}
