// Package smtptransport performs a single SMTP attempt over an already
// connected stream. Routing, destination filtering, durable attempt recording,
// and retries belong to the worker and are not implemented here.
package smtptransport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"regexp"
	"strings"
	"time"
)

type Outcome int

// ErrRouteRejected identifies an SMTP reply to connection setup, before any
// envelope has been presented. It does not establish recipient failure.
var ErrRouteRejected = errors.New("SMTP route rejected connection setup")

// ReplyError reports a 4xx/5xx SMTP reply by stage and code only. Remote reply
// text often echoes the recipient, so it is deliberately never retained.
type ReplyError struct {
	Stage string
	Code  int
	// Route marks a reply to connection setup, before any envelope.
	Route bool
	// Enhanced is the RFC 3463 status code (digits only) parsed from the start
	// of the reply text, or empty. The rest of the text is discarded.
	Enhanced string
}

// ParseEnhanced extracts an RFC 3463 enhanced status code from the start of a
// reply's text. It returns "" unless the text begins with class.subject.detail
// (class 2, 4 or 5, subject and detail 1-3 digits) followed by whitespace or the
// end of the text, and the class agrees with the reply code's first digit.
func ParseEnhanced(code int, msg string) string {
	match := enhancedPrefix.FindStringSubmatch(msg)
	if match == nil || int(match[1][0]-'0') != code/100 {
		return ""
	}
	return match[1]
}

var enhancedPrefix = regexp.MustCompile(`^([245]\.\d{1,3}\.\d{1,3})(?:[ \t\r\n]|$)`)

func (e *ReplyError) Error() string { return fmt.Sprintf("SMTP %s rejected (%d)", e.Stage, e.Code) }

func (e *ReplyError) Is(target error) bool { return e.Route && target == ErrRouteRejected }

// stageError names a failed stage without its cause text. The cause stays
// reachable through Unwrap for errors.Is/As but never appears in Error().
type stageError struct {
	stage string
	cause error
}

func (e *stageError) Error() string { return "SMTP " + e.stage + " failed" }
func (e *stageError) Unwrap() error { return e.cause }

const (
	// NotAccepted means no DATA terminator was sent or the server explicitly
	// rejected the attempt; a retry does not risk duplicating this attempt.
	NotAccepted Outcome = iota
	// Accepted means the server returned a successful final DATA response.
	Accepted
	// Uncertain means the DATA terminator may have reached the server, but the
	// final response was not received. Retrying may duplicate the message.
	Uncertain
)

type Request struct {
	MXName       string
	HeloName     string
	EnvelopeFrom string
	Recipient    string
	RequireTLS   bool
	// MIME must already be signed, immutable and formatted with CRLF endings.
	MIME io.Reader
	// Roots may be nil to use system roots. This never disables verification.
	Roots *x509.CertPool
	// Timeouts defaults to the protocol's recommended minimum stage bounds.
	Timeouts *Timeouts
}

// Send closes conn. The caller must resolve the MX securely as appropriate,
// filter and pin destination IPs, persist the attempt and enforce bounds before
// opening conn. Successful TLS verifies the certificate against MXName; this
// alone does not authenticate an MX name obtained from insecure DNS.
func Send(ctx context.Context, conn net.Conn, req Request) (Outcome, error) {
	defer conn.Close()
	if err := ctx.Err(); err != nil {
		return NotAccepted, err
	}
	if req.MXName == "" || strings.ContainsAny(req.MXName, "\r\n") || req.HeloName == "" || strings.ContainsAny(req.HeloName, "\r\n ") || req.MIME == nil || !validMailbox(req.Recipient) || (req.EnvelopeFrom != "" && !validMailbox(req.EnvelopeFrom)) {
		return NotAccepted, errors.New("invalid SMTP request")
	}
	timeouts := DefaultTimeouts()
	if req.Timeouts != nil {
		timeouts = *req.Timeouts
	}
	if err := timeouts.Validate(); err != nil {
		return NotAccepted, err
	}
	if err := setStageDeadline(ctx, conn, timeouts.Greeting); err != nil {
		return NotAccepted, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	client, err := smtp.NewClient(conn, req.MXName)
	if err != nil {
		return NotAccepted, stageFailure("greeting", err, true)
	}
	defer client.Close()
	if err = setStageDeadline(ctx, conn, timeouts.Command); err != nil {
		return NotAccepted, err
	}
	if err = client.Hello(req.HeloName); err != nil {
		return NotAccepted, stageFailure("EHLO", err, true)
	}
	supportsTLS, _ := client.Extension("STARTTLS")
	if !supportsTLS && req.RequireTLS {
		return NotAccepted, errors.New("destination does not offer STARTTLS")
	}
	if supportsTLS {
		config := &tls.Config{ServerName: req.MXName, RootCAs: req.Roots, MinVersion: tls.VersionTLS12}
		if err = setStageDeadline(ctx, conn, timeouts.Command); err != nil {
			return NotAccepted, err
		}
		if err = client.StartTLS(config); err != nil {
			return NotAccepted, stageFailure("STARTTLS", err, true)
		}
	}
	if !isASCII(req.EnvelopeFrom) || !isASCII(req.Recipient) {
		if supported, _ := client.Extension("SMTPUTF8"); !supported {
			return NotAccepted, errors.New("destination does not support SMTPUTF8")
		}
	}
	if err = setStageDeadline(ctx, conn, timeouts.Command); err != nil {
		return NotAccepted, err
	}
	if err = client.Mail(req.EnvelopeFrom); err != nil {
		return NotAccepted, stageFailure("MAIL FROM", err, false)
	}
	if err = setStageDeadline(ctx, conn, timeouts.Command); err != nil {
		return NotAccepted, err
	}
	if err = client.Rcpt(req.Recipient); err != nil {
		return NotAccepted, stageFailure("RCPT TO", err, false)
	}
	if err = setStageDeadline(ctx, conn, timeouts.DataInit); err != nil {
		return NotAccepted, err
	}
	writer, err := client.Data()
	if err != nil {
		return NotAccepted, stageFailure("DATA", err, false)
	}
	// Hide optional WriterTo/ReaderFrom fast paths so each bounded block gets
	// its own deadline even when MIME is supplied as a bytes.Reader.
	blocks := blockTimeoutWriter{ctx: ctx, conn: conn, timeout: timeouts.DataBlock, writer: writer}
	if _, err = io.CopyBuffer(blocks, struct{ io.Reader }{req.MIME}, make([]byte, 32<<10)); err != nil {
		return NotAccepted, &stageError{stage: "write MIME", cause: err}
	}
	if err = setStageDeadline(ctx, conn, timeouts.DataFinal); err != nil {
		return NotAccepted, err
	}
	if err = writer.Close(); err != nil {
		if replyErr := stageFailure("DATA", err, false); isReply(replyErr) {
			return NotAccepted, replyErr
		}
		return Uncertain, &stageError{stage: "DATA result unknown", cause: err}
	}
	// A successful DATA response is final even if QUIT or the connection fails.
	quitDeadline := time.Now().Add(time.Second)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(quitDeadline) {
		quitDeadline = deadline
	}
	_ = conn.SetDeadline(quitDeadline)
	_ = client.Quit()
	return Accepted, nil
}

func stageFailure(stage string, err error, route bool) error {
	var reply *textproto.Error
	if errors.As(err, &reply) && reply.Code >= 400 && reply.Code <= 599 {
		return &ReplyError{Stage: stage, Code: reply.Code, Route: route, Enhanced: ParseEnhanced(reply.Code, reply.Msg)}
	}
	return &stageError{stage: stage, cause: err}
}

func isReply(err error) bool {
	var reply *ReplyError
	return errors.As(err, &reply)
}

func validMailbox(value string) bool {
	if value == "" || strings.ContainsAny(value, "\r\n<>") {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Name == "" && address.Address == value && strings.Contains(value, "@")
}

func isASCII(value string) bool {
	for _, c := range value {
		if c > 127 {
			return false
		}
	}
	return true
}
