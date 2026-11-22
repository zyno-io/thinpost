// Package message builds and DKIM-signs immutable per-recipient MIME. Callers
// must store the returned bytes durably and reuse them unchanged for retries.
package message

import (
	"bytes"
	"crypto"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/zyno-io/thinpost/internal/providerid"
	"github.com/zyno-io/thinpost/internal/receivedtrace"
	"github.com/zyno-io/thinpost/internal/submission"
)

// MaxDKIMRSABits bounds the signature space reserved during admission.
const MaxDKIMRSABits = 8192

type Options struct {
	Recipient     string
	ID            string // Message-scoped token (the message ID), shared by every recipient's copy; no domain or angle brackets.
	Date          time.Time
	SigningDomain string
	Selector      string
	Signer        crypto.Signer
	// ListRecipients is true for messages stored as source version 2: every
	// copy then lists all To and all Cc recipients and never a Bcc header. When
	// false (version 1) the copy has only the recipient's own To header and the
	// message cannot carry Cc or Bcc.
	ListRecipients bool
	// Received is Thinpost's trace field (receivedtrace.Field), placed right
	// after the DKIM-Signature, as in rawmime.Prepare. It is added after signing
	// and is not covered by it. Empty adds none.
	Received []byte
}

// MaxRecipientHeaderBytes bounds the folded To and Cc headers together. Header
// block parsers elsewhere (the console's delivery headers, spam reports) read
// at most 64 KiB, so recipients must leave room for the other headers.
const MaxRecipientHeaderBytes = 16 << 10

// maxAddressHeaderBytes is the longest rendered address that fits the first
// header line ("To: " + address + ",") within 998 bytes.
const maxAddressHeaderBytes = 998 - len("To: ") - 1

// Build validates the recipient and signs the final MIME after message
// transformations. Tracking rewrites must be completed before calling it.
func Build(input submission.Message, options Options) ([]byte, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	if options.Date.IsZero() || options.Signer == nil || !validToken(options.ID) || !validDomain(options.SigningDomain) || !validToken(options.Selector) {
		return nil, errors.New("invalid message signing options")
	}
	if len(options.Received) > 0 && !receivedtrace.Valid(options.Received) {
		return nil, errors.New("invalid message signing options")
	}
	from, err := mail.ParseAddress(input.From)
	if err != nil {
		// The parser error can quote the input address.
		return nil, errors.New("invalid sender")
	}
	recipient, err := mail.ParseAddress(options.Recipient)
	if err != nil || strings.ContainsAny(options.Recipient, "\r\n") {
		return nil, errors.New("invalid recipient")
	}
	if !options.ListRecipients && (len(input.Cc) > 0 || len(input.Bcc) > 0) {
		return nil, errors.New("recipient lists need a version 2 source")
	}
	allowed := false
	for _, value := range input.Envelope() {
		candidate, err := mail.ParseAddress(value)
		if err == nil && candidate.Address == recipient.Address {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, errors.New("recipient is not in submission")
	}
	at := strings.LastIndexByte(from.Address, '@')
	if at < 1 || !strings.EqualFold(from.Address[at+1:], options.SigningDomain) {
		return nil, errors.New("signing domain does not match sender")
	}
	var message bytes.Buffer
	headers := [][2]string{{"From", from.String()}, {"To", recipient.String()}, {"Subject", mime.QEncoding.Encode("utf-8", input.Subject)}, {"Date", options.Date.UTC().Format(time.RFC1123Z)}, {"Message-ID", providerid.MessageIDHeader(options.ID, options.SigningDomain)}, {"MIME-Version", "1.0"}}
	// Pre-folded recipient headers replace the single-line To header.
	folded := map[string]string{}
	if options.ListRecipients {
		total := 0
		for _, list := range []struct {
			name      string
			addresses []string
		}{{"To", input.To}, {"Cc", input.Cc}} {
			if len(list.addresses) == 0 {
				continue
			}
			rendered := make([]string, len(list.addresses))
			for i, value := range list.addresses {
				address, err := mail.ParseAddress(value)
				if err != nil {
					return nil, errors.New("invalid recipient")
				}
				rendered[i] = address.String()
				if len(rendered[i]) > maxAddressHeaderBytes {
					return nil, errors.New("invalid or oversized header")
				}
			}
			folded[list.name] = foldAddresses(list.name, rendered)
			total += len(folded[list.name])
		}
		if total > MaxRecipientHeaderBytes {
			return nil, errors.New("invalid or oversized header")
		}
		headers[1][1] = ""
		if folded["Cc"] != "" {
			headers = append(headers[:2], append([][2]string{{"Cc", ""}}, headers[2:]...)...)
		}
	}
	if len(input.ReplyTo) > 0 {
		addresses := make([]string, 0, len(input.ReplyTo))
		for _, value := range input.ReplyTo {
			address, err := mail.ParseAddress(value)
			if err != nil {
				return nil, errors.New("invalid reply address")
			}
			addresses = append(addresses, address.String())
		}
		headers = append(headers, [2]string{"Reply-To", strings.Join(addresses, ", ")})
	}
	if input.ListUnsubscribe != "" || input.ListUnsubscribePost {
		if err := submission.ValidateListUnsubscribe(input.ListUnsubscribe, input.ListUnsubscribePost); err != nil || input.ListUnsubscribe == "" {
			return nil, errors.New("invalid list-unsubscribe header")
		}
		headers = append(headers, [2]string{"List-Unsubscribe", input.ListUnsubscribe})
		if input.ListUnsubscribePost {
			headers = append(headers, [2]string{"List-Unsubscribe-Post", submission.ListUnsubscribePostValue})
		}
	}
	if input.InReplyTo != "" {
		if err := submission.ValidateInReplyTo(input.InReplyTo); err != nil {
			return nil, errors.New("invalid in-reply-to header")
		}
		headers = append(headers, [2]string{"In-Reply-To", input.InReplyTo})
	}
	var references string
	if input.References != "" {
		if err := submission.ValidateReferences(input.References); err != nil {
			return nil, errors.New("invalid references header")
		}
		references = foldReferences(input.References)
	}
	for _, h := range headers {
		if block, ok := folded[h[0]]; ok {
			message.WriteString(block)
			continue
		}
		if strings.ContainsAny(h[1], "\r\n") || len(h[0])+2+len(h[1]) > 998 {
			return nil, errors.New("invalid or oversized header")
		}
		fmt.Fprintf(&message, "%s: %s\r\n", h[0], h[1])
	}
	message.WriteString(references)
	if len(input.Attachments) > 0 {
		if err = writeAttachments(&message, input); err != nil {
			return nil, err
		}
	} else if input.Text != "" && input.HTML != "" {
		multipartWriter := multipart.NewWriter(&message)
		contentType := mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": multipartWriter.Boundary()})
		fmt.Fprintf(&message, "Content-Type: %s\r\n\r\n", contentType)
		for _, part := range []struct{ contentType, body string }{{"text/plain; charset=utf-8", input.Text}, {"text/html; charset=utf-8", input.HTML}} {
			header := textproto.MIMEHeader{}
			header.Set("Content-Type", part.contentType)
			header.Set("Content-Transfer-Encoding", "quoted-printable")
			writer, err := multipartWriter.CreatePart(header)
			if err != nil {
				return nil, err
			}
			if err = writeBody(writer, part.body); err != nil {
				return nil, err
			}
		}
		if err = multipartWriter.Close(); err != nil {
			return nil, err
		}
	} else {
		body, kind := input.Text, "text/plain"
		if body == "" {
			body, kind = input.HTML, "text/html"
		}
		fmt.Fprintf(&message, "Content-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", kind)
		if err = writeBody(&message, body); err != nil {
			return nil, err
		}
	}
	var signed bytes.Buffer
	err = dkim.Sign(&signed, bytes.NewReader(message.Bytes()), &dkim.SignOptions{Domain: strings.ToLower(options.SigningDomain), Selector: options.Selector, Signer: options.Signer, Hash: crypto.SHA256, HeaderCanonicalization: dkim.CanonicalizationRelaxed, BodyCanonicalization: dkim.CanonicalizationRelaxed})
	if err != nil {
		return nil, fmt.Errorf("sign message: %w", err)
	}
	if len(options.Received) == 0 {
		return signed.Bytes(), nil
	}
	// The signer writes its own field first; the trace goes after its last fold.
	sig := signed.Bytes()
	end := 0
	for end < len(sig) {
		i := bytes.Index(sig[end:], []byte("\r\n"))
		if i < 0 {
			return nil, errors.New("sign message: unexpected signature layout")
		}
		end += i + 2
		if end < len(sig) && sig[end] != ' ' && sig[end] != '\t' {
			break
		}
	}
	out := make([]byte, 0, len(options.Received)+len(sig))
	return append(append(append(out, sig[:end]...), options.Received...), sig[end:]...), nil
}

// writeAttachments emits mixed -> alternative -> related as needed. Inline
// parts remain related to the HTML representation; ordinary files remain
// siblings of the message body. All content is signed after this completes.
func writeAttachments(out *bytes.Buffer, input submission.Message) error {
	plainHeader, plainBody, err := encodedBody("text/plain", input.Text)
	if err != nil {
		return err
	}
	htmlHeader, htmlBody, err := encodedBody("text/html", input.HTML)
	if err != nil {
		return err
	}
	var inline, files []submission.Attachment
	for _, attachment := range input.Attachments {
		if attachment.Inline {
			inline = append(inline, attachment)
		} else {
			files = append(files, attachment)
		}
	}
	if len(inline) > 0 {
		var related bytes.Buffer
		writer := multipart.NewWriter(&related)
		if err = writePart(writer, htmlHeader, htmlBody); err != nil {
			return err
		}
		for _, attachment := range inline {
			if err = writeAttachmentPart(writer, attachment); err != nil {
				return err
			}
		}
		if err = writer.Close(); err != nil {
			return err
		}
		htmlHeader = multipartHeader("related", writer.Boundary())
		htmlBody = related.Bytes()
	}
	bodyHeader, body := plainHeader, plainBody
	if input.Text == "" {
		bodyHeader, body = htmlHeader, htmlBody
	} else if input.HTML != "" {
		var alternative bytes.Buffer
		writer := multipart.NewWriter(&alternative)
		if err = writePart(writer, plainHeader, plainBody); err != nil {
			return err
		}
		if err = writePart(writer, htmlHeader, htmlBody); err != nil {
			return err
		}
		if err = writer.Close(); err != nil {
			return err
		}
		bodyHeader, body = multipartHeader("alternative", writer.Boundary()), alternative.Bytes()
	}
	mixed := multipart.NewWriter(out)
	fmt.Fprintf(out, "Content-Type: %s\r\n\r\n", mime.FormatMediaType("multipart/mixed", map[string]string{"boundary": mixed.Boundary()}))
	if err = writePart(mixed, bodyHeader, body); err != nil {
		return err
	}
	for _, attachment := range files {
		if err = writeAttachmentPart(mixed, attachment); err != nil {
			return err
		}
	}
	return mixed.Close()
}

func encodedBody(kind, body string) (textproto.MIMEHeader, []byte, error) {
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", kind+"; charset=utf-8")
	header.Set("Content-Transfer-Encoding", "quoted-printable")
	var data bytes.Buffer
	if err := writeBody(&data, body); err != nil {
		return nil, nil, err
	}
	return header, data.Bytes(), nil
}

func multipartHeader(kind, boundary string) textproto.MIMEHeader {
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", mime.FormatMediaType("multipart/"+kind, map[string]string{"boundary": boundary}))
	return header
}

func writePart(writer *multipart.Writer, header textproto.MIMEHeader, body []byte) error {
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	_, err = part.Write(body)
	return err
}

func writeAttachmentPart(writer *multipart.Writer, attachment submission.Attachment) error {
	mediaType, _, err := mime.ParseMediaType(attachment.ContentType)
	if err != nil {
		return errors.New("invalid attachment content type")
	}
	disposition := "attachment"
	if attachment.Inline {
		disposition = "inline"
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", mediaType)
	header.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": attachment.Filename}))
	header.Set("Content-Transfer-Encoding", "base64")
	if attachment.Inline {
		header.Set("Content-ID", "<"+attachment.ContentID+">")
	}
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	// 57 input bytes encode to a 76-column MIME line.
	for offset := 0; offset < len(attachment.Content); offset += 57 {
		end := min(offset+57, len(attachment.Content))
		if _, err = io.WriteString(part, base64.StdEncoding.EncodeToString(attachment.Content[offset:end])+"\r\n"); err != nil {
			return err
		}
	}
	return nil
}

func writeBody(out interface{ Write([]byte) (int, error) }, body string) error {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	if !strings.HasSuffix(body, "\r\n") {
		body += "\r\n"
	}
	writer := quotedprintable.NewWriter(out)
	if _, err := writer.Write([]byte(body)); err != nil {
		return err
	}
	return writer.Close()
}
func validToken(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func validDomain(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// foldReferences renders a validated, single-space-separated msg-id list as a
// References header with its trailing CRLF. It folds only between msg-ids (CRLF
// plus one space), preferring lines of at most 78 bytes. An id too long for the
// first line moves to the next one; validation guarantees every id fits a line.
func foldReferences(ids string) string {
	var out strings.Builder
	line := "References:"
	for _, id := range strings.Split(ids, " ") {
		if len(line)+1+len(id) <= 78 || line == "References:" && len(line)+1+len(id) <= 998 {
			line += " " + id
			continue
		}
		out.WriteString(line + "\r\n")
		line = " " + id
	}
	return out.String() + line + "\r\n"
}

// foldAddresses renders an address-list header with its trailing CRLF. It folds
// only after a comma (CRLF plus one space), preferring lines of at most 78
// bytes; an address too long for the first line moves to the next one.
// Callers guarantee every address fits a line.
func foldAddresses(name string, addresses []string) string {
	var out strings.Builder
	first := name + ":"
	line := first
	for i, address := range addresses {
		token := address
		if i < len(addresses)-1 {
			token += ","
		}
		if len(line)+1+len(token) <= 78 || line == first && len(line)+1+len(token) <= 998 {
			line += " " + token
			continue
		}
		out.WriteString(line + "\r\n")
		line = " " + token
	}
	return out.String() + line + "\r\n"
}
