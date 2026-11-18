package feedback

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"regexp"
	"strings"

	"github.com/zyno-io/thinpost/internal/messageid"
)

// Parser bounds. The 16 MiB message limit is enforced before this package runs.
const (
	maxMIMEDepth   = 3
	maxMIMEParts   = 16
	maxHeaderBlock = 64 << 10
)

var errUnparsable = errors.New("unparsable feedback report")

// report is what the parser keeps of an ARF message: the feedback type and the
// two headers of the embedded original that correlation needs. Nothing else of
// the body is retained.
type report struct {
	feedbackType string
	seenType     bool
	messageID    string
	returnPath   string
	hasReturn    bool
	seenOriginal bool
}

// parseARF parses an RFC 5965 multipart/report in memory. It never returns or
// stores the report text, the original body or any other header.
func parseARF(data []byte) (*report, error) {
	head, body, ok := splitHeaderBlock(data, false)
	if !ok {
		return nil, errUnparsable
	}
	header, err := readHeader(head)
	if err != nil {
		return nil, err
	}
	// Exactly one top-level Content-Type: the parser would read the first while
	// DKIM covers the last, so a prepended unsigned one could re-frame the
	// signed body.
	if len(header["Content-Type"]) != 1 {
		return nil, errUnparsable
	}
	media, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || media != "multipart/report" || !strings.EqualFold(params["report-type"], "feedback-report") || params["boundary"] == "" {
		return nil, errUnparsable
	}
	r := &report{}
	p := &walker{report: r}
	if err = p.walk(body, params["boundary"], 1); err != nil {
		return nil, err
	}
	if !r.seenType || !r.seenOriginal {
		return nil, errUnparsable
	}
	return r, nil
}

type walker struct {
	parts  int
	report *report
}

func (w *walker) walk(body []byte, boundary string, depth int) error {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errUnparsable
		}
		w.parts++
		if w.parts > maxMIMEParts || headerSize(part.Header) > maxHeaderBlock {
			return errUnparsable
		}
		contentType := part.Header.Get("Content-Type")
		if contentType == "" {
			continue
		}
		media, params, err := mime.ParseMediaType(contentType)
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(media, "multipart/"):
			if depth+1 > maxMIMEDepth || params["boundary"] == "" {
				return errUnparsable
			}
			inner, readErr := io.ReadAll(part)
			if readErr != nil {
				return errUnparsable
			}
			if err = w.walk(inner, params["boundary"], depth+1); err != nil {
				return err
			}
		case media == "message/feedback-report" && !w.report.seenType:
			block, readErr := readBounded(part)
			if readErr != nil {
				return readErr
			}
			fields, readErr := readHeader(firstBlock(block))
			if readErr != nil {
				return readErr
			}
			w.report.seenType = true
			w.report.feedbackType = strings.ToLower(strings.TrimSpace(fields.Get("Feedback-Type")))
		case (media == "message/rfc822" || media == "text/rfc822-headers") && !w.report.seenOriginal:
			block, readErr := readBounded(part)
			if readErr != nil {
				return readErr
			}
			head, _, ok := splitHeaderBlock(block, true)
			if !ok {
				return errUnparsable
			}
			// Lenient: a malformed line in the original must not hide the two
			// headers correlation needs.
			w.report.seenOriginal = true
			w.report.messageID, w.report.returnPath, w.report.hasReturn = scanOriginal(head)
		}
	}
}

// readBounded reads at most one header block's worth of a part. A message/rfc822
// original can be large, but only its headers are needed, so the rest is dropped
// unread; splitHeaderBlock then rejects a block that never ends within the bound.
func readBounded(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxHeaderBlock+4))
	if err != nil {
		return nil, errUnparsable
	}
	return data, nil
}

func headerSize(h textproto.MIMEHeader) int {
	size := 0
	for key, values := range h {
		for _, value := range values {
			size += len(key) + len(value) + 4
		}
	}
	return size
}

// splitHeaderBlock splits at the first blank line. The header block must end
// within maxHeaderBlock bytes; with unterminated true a headers-only part with no
// blank line is accepted when it fits the bound.
func splitHeaderBlock(data []byte, unterminated bool) (head, rest []byte, ok bool) {
	// Tolerate leading blank lines some reporters emit before the first field.
	data = bytes.TrimLeft(data, "\r\n")
	end, sep := -1, 0
	if i := bytes.Index(data, []byte("\r\n\r\n")); i >= 0 {
		end, sep = i, 4
	}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 && (end < 0 || i < end) {
		end, sep = i, 2
	}
	if end < 0 {
		if unterminated && len(data) <= maxHeaderBlock {
			return data, nil, true
		}
		return nil, nil, false
	}
	if end > maxHeaderBlock {
		return nil, nil, false
	}
	return data[:end], data[end+sep:], true
}

// firstBlock returns the first header block of a feedback-report part, which can
// carry further per-recipient blocks after a blank line.
func firstBlock(data []byte) []byte {
	if head, _, ok := splitHeaderBlock(data, true); ok {
		return head
	}
	return data
}

func readHeader(block []byte) (textproto.MIMEHeader, error) {
	reader := textproto.NewReader(bufio.NewReader(io.MultiReader(bytes.NewReader(block), strings.NewReader("\r\n\r\n"))))
	header, err := reader.ReadMIMEHeader()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, errUnparsable
	}
	return header, nil
}

var (
	// A Return-Path local part is b+<token>, possibly inside an SRS0/SRS1
	// rewrite added by a forwarder (SRS0=hash=tt=domain=local, with "=" or "-" or
	// "+" separators).
	returnPathPattern = regexp.MustCompile(`(?i)^<(?:(?:srs[01][=+-][^<>@\s]*=)?b\+([0-9a-f]{32}))@[^<>@\s]+>$`)
)

// scanOriginal scans the unfolded header lines of the embedded original for the
// first Message-ID and Return-Path, ignoring any line it cannot read.
func scanOriginal(head []byte) (messageID, returnPath string, hasReturn bool) {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(string(head), "\r\n", "\n"), "\n") {
		if line != "" && (line[0] == ' ' || line[0] == '\t') && len(lines) > 0 {
			lines[len(lines)-1] += " " + strings.TrimSpace(line)
			continue
		}
		lines = append(lines, line)
	}
	seenID := false
	for _, line := range lines {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "message-id":
			if !seenID {
				seenID, messageID = true, strings.TrimSpace(value)
			}
		case "return-path":
			if !hasReturn {
				hasReturn, returnPath = true, strings.TrimSpace(value)
			}
		}
	}
	return
}

// originalIDs extracts the Thinpost delivery ID and sender domain from the
// original's Message-ID. When the original has a Return-Path, bounceToken is the
// lowercased b+token it carries (directly or inside SRS), or empty when the
// Return-Path is anything else; the caller must treat present-without-token as
// a conflict.
func (r *report) originalIDs() (deliveryID, domain, bounceToken string, hasReturn, ok bool) {
	id, domain, found := messageid.ParseThinpost(r.messageID)
	if !found {
		return "", "", "", false, false
	}
	if p := returnPathPattern.FindStringSubmatch(r.returnPath); p != nil {
		bounceToken = strings.ToLower(p[1])
	}
	return id, domain, bounceToken, r.hasReturn, true
}
