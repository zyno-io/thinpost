// Package httpapi contains incomplete provider compatibility adapters. No
// production server is wired until a durable submission service is available.
package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zyno-io/thinpost/internal/providerid"
	"github.com/zyno-io/thinpost/internal/submission"
	"golang.org/x/sync/semaphore"
)

// maxBody is the request limit for batch routes and suppression routes.
const maxBody = 2 << 20

// singleBodyExtra is the request room, beyond the base64 attachment payload, for
// the rest of a single-message request: headers, text, HTML (tracking accepts up
// to 2 MiB of HTML) and JSON or multipart framing.
const singleBodyExtra = 4 << 20

// largeBodyBudget bounds the request bytes of in-flight single-message requests
// whose body exceeds maxBody. One such request can hold several times its body
// in memory (the decoded copy, two JSON encodings that escape "<" to six bytes,
// a MIME rendering), and the control pod, with a 512Mi limit, also runs the
// operator API and the workers. 32 MiB admits two requests at the 5 MiB default
// (11184812 bytes each) and two at the 5.5 MiB ceiling (11883864 bytes each);
// requests up to maxBody never wait. Waiting requests queue in arrival order.
const largeBodyBudget = 32 << 20

// largeWaitTimeout bounds the wait for budget, since a waiting client holds a
// connection and an unread body (HTTP/1.1 does not cancel the request context
// on disconnect until the body is read). On expiry the answer is 503 with
// Retry-After, which SDKs retry; it is never 400.
const largeWaitTimeout = 10 * time.Second

// largeReadBudget is the renewed read deadline once budget is held: 60s for an
// upload of about 12 MB, a floor of 200 KB/s. largeWriteExtra is added to it
// for the write deadline (WriteTimeout is 30s from the headers) so acceptance
// and the response still fit after a slow upload.
const (
	largeReadBudget = 60 * time.Second
	largeWriteExtra = 30 * time.Second
)

// limits are the per-request size limits applied while decoding: the request
// body, and the decoded bytes of attachments in one message.
type limits struct{ body, attachment int }

// SingleBodyLimit is the request body limit for single-message routes: the base64 encoding of the configured attachment
// limit plus singleBodyExtra. At the 5 MiB default that is 11184812 bytes.
func SingleBodyLimit(attachmentLimit int) int {
	return base64.StdEncoding.EncodedLen(attachmentLimit) + singleBodyExtra
}

func (s *Server) singleLimits() limits {
	return limits{body: SingleBodyLimit(s.attachmentLimit), attachment: s.attachmentLimit}
}

// batchLimits keeps batch routes at maxBody; with the acceptance batch source
// cap (2 MiB), a batch carries at most about 2 MiB of attachment data.
func (s *Server) batchLimits() limits {
	return limits{body: maxBody, attachment: s.attachmentLimit}
}

// errBodyRead marks a failure to receive the body (timeout, dropped
// connection), as opposed to a body that was received and is invalid.
var errBodyRead = errors.New("request body not received")

// replayBody reads a prefix already consumed from the body, then the rest.
type replayBody struct {
	io.Reader
	io.Closer
}

// countingBody counts the bytes a decoder read.
type countingBody struct {
	io.ReadCloser
	n int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

// admits is the admission check for a new request: the strict validation every
// stored message passes, plus the configured attachment limit. Readers of
// stored sources use Validate alone, which enforces only the fixed ceiling, so
// lowering the configured limit never makes an accepted message unreadable.
func (s *Server) admits(m submission.Message) bool {
	return m.Validate() == nil && m.ValidateAttachmentLimit(s.attachmentLimit) == nil
}

type Server struct {
	service         submission.Service
	tracking        bool
	attachmentLimit int
	suppressions    submission.SuppressionStore
	large           *semaphore.Weighted
	waitTimeout     time.Duration
}

// Options control features that require a coordinated reader rollout.
type Options struct {
	// Tracking requires the signed-link preparation/runtime configuration and
	// public ingress controls. It is disabled by default. When false, explicit
	// per-message opt-ins get 400 and app tracking defaults are ignored (by
	// acceptance, which is configured separately from the same setting).
	Tracking bool
	// MaxAttachmentBytes is the admission limit on the total decoded attachment
	// bytes per message. Zero means submission.DefaultMaxAttachmentBytes.
	MaxAttachmentBytes int
	// Suppressions enables the Postmark message-stream suppression routes and
	// Resend GET /suppressions. Without it they answer 503.
	Suppressions submission.SuppressionStore
}

func New(service submission.Service) *Server {
	return &Server{service: service, attachmentLimit: submission.DefaultMaxAttachmentBytes, large: semaphore.NewWeighted(largeBodyBudget), waitTimeout: largeWaitTimeout}
}

func NewWithOptions(service submission.Service, options Options) *Server {
	limit := options.MaxAttachmentBytes
	if limit <= 0 || limit > submission.MaxAttachmentBytes {
		limit = submission.DefaultMaxAttachmentBytes
	}
	return &Server{service: service, attachmentLimit: limit, large: semaphore.NewWeighted(largeBodyBudget), waitTimeout: largeWaitTimeout, tracking: options.Tracking, suppressions: options.Suppressions}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/suppressions" || strings.HasPrefix(r.URL.Path, "/message-streams/") {
		s.serveSuppressions(w, r)
		return
	}
	provider := ""
	domain := ""
	switch r.URL.Path {
	case "/email", "/email/batch":
		provider = "postmark"
	case "/emails", "/emails/batch":
		provider = "resend"
	case "/v3/mail/send":
		provider = "sendgrid"
	case nativePath:
		provider = "thinpost"
		nativeHeaders(w)
		// The route has one canonical spelling; any percent-encoding is 404.
		if strings.Contains(r.URL.EscapedPath(), "%") {
			nativeFail(w, http.StatusNotFound, "Not Found")
			return
		}
	default:
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) != 4 || parts[1] != "v3" || parts[3] != "messages" || !validDomain(parts[2]) {
			http.NotFound(w, r)
			return
		}
		provider, domain = "mailgun", strings.ToLower(parts[2])
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		fail(w, provider, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if provider == "thinpost" && (r.URL.RawQuery != "" || r.URL.ForceQuery || nativeRejectedHeader(r.Header)) {
		fail(w, provider, http.StatusBadRequest, "Bad Request")
		return
	}
	if s.service == nil {
		fail(w, provider, http.StatusServiceUnavailable, "submission unavailable")
		return
	}
	key, ok := credential(r, provider)
	if !ok {
		fail(w, provider, http.StatusUnauthorized, "invalid API key")
		return
	}
	app, err := s.service.Authenticate(r.Context(), key)
	if err != nil || app.ID == "" {
		if errors.Is(err, submission.ErrInvalidKey) || err == nil {
			fail(w, provider, http.StatusUnauthorized, "invalid API key")
		} else {
			fail(w, provider, http.StatusServiceUnavailable, "authentication unavailable")
		}
		return
	}
	// Authenticate returns API-key principals only. An operator principal is
	// created solely by AuthorizeOperator for the console route, so one here
	// means a misbehaving service: refuse rather than submit it.
	if app.Operator != "" {
		fail(w, provider, http.StatusServiceUnavailable, "authentication unavailable")
		return
	}
	if provider != "resend" && (len(r.Header.Values("X-Thinpost-Track-Opens")) > 0 || len(r.Header.Values("X-Thinpost-Track-Clicks")) > 0) {
		fail(w, provider, http.StatusBadRequest, "unsupported tracking headers")
		return
	}
	hold, err := holdFlag(r.Header)
	if err != nil {
		fail(w, provider, http.StatusBadRequest, "invalid X-Thinpost-Hold header")
		return
	}
	if r.URL.Path == "/email/batch" {
		s.postmarkBatch(w, r, app, hold)
		return
	}
	if r.URL.Path == "/emails/batch" {
		s.resendBatch(w, r, app, hold)
		return
	}
	lim := s.singleLimits()
	// Requests that may exceed maxBody wait for budget, then hold it until the
	// response is written. The weight is the declared Content-Length; for an
	// unknown length (chunked) the first maxBody bytes are read without the
	// limiter, so a small chunked request never waits, and the full limit is the
	// weight of one that goes on. The server never delivers more bytes than a
	// declared length, and decoding never reads more than lim.body.
	if lim.body > maxBody && (r.ContentLength < 0 || r.ContentLength > maxBody) {
		weight := int64(lim.body)
		if r.ContentLength < 0 {
			prefix, readErr := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
			if readErr != nil {
				fail(w, provider, http.StatusRequestTimeout, "request body not received")
				return
			}
			r.Body = replayBody{io.MultiReader(bytes.NewReader(prefix), r.Body), r.Body}
			if len(prefix) <= maxBody {
				weight = 0
			}
		} else if r.ContentLength < weight {
			weight = r.ContentLength
		}
		if weight > 0 {
			weight = min(weight, largeBodyBudget)
			waitCtx, cancel := context.WithTimeout(r.Context(), s.waitTimeout)
			err = s.large.Acquire(waitCtx, weight)
			cancel()
			if err != nil {
				w.Header().Set("Retry-After", "5")
				fail(w, provider, http.StatusServiceUnavailable, "submission unavailable")
				return
			}
			defer s.large.Release(weight)
			// The server's deadlines started when the headers arrived, and the
			// wait may have used them up. Renew them for the upload and response.
			rc := http.NewResponseController(w)
			_ = rc.SetReadDeadline(time.Now().Add(largeReadBudget))
			_ = rc.SetWriteDeadline(time.Now().Add(largeReadBudget + largeWriteExtra))
		}
	}
	body := &countingBody{ReadCloser: r.Body}
	r.Body = body
	var msg submission.Message
	var postmarkTo string
	var gridMessages []submission.Message
	var nativeOpts nativeOptions
	switch provider {
	case "postmark":
		msg, postmarkTo, err = decodePostmark(r, lim)
	case "resend":
		msg, err = decodeResend(r, lim)
	case "sendgrid":
		gridMessages, err = decodeSendGrid(r, lim)
		if err == nil && len(gridMessages) == 1 {
			msg = gridMessages[0]
		}
	case "mailgun":
		msg, err = decodeMailgun(r, lim)
	case "thinpost":
		var data []byte
		if data, err = readBody(r, lim); err == nil {
			msg, nativeOpts, err = decodeNative(data, lim, nativeKeys)
		}
	}
	if errors.Is(err, errBodyRead) {
		// A timeout or dropped connection is not a verdict on the message, and
		// SDKs treat 400 as permanent.
		fail(w, provider, http.StatusRequestTimeout, "request body not received")
		return
	}
	if err != nil {
		fail(w, provider, http.StatusBadRequest, "invalid or unsupported message")
		return
	}
	if provider == "sendgrid" && len(gridMessages) > 1 {
		// Batches keep the batch body limit even though the request was read
		// under the single-message one.
		if body.n > maxBody {
			fail(w, provider, http.StatusBadRequest, "invalid or unsupported message")
			return
		}
		s.sendGridBatch(w, r, app, gridMessages, hold)
		return
	}
	if provider == "thinpost" {
		s.serveNative(w, r, app, msg, nativeOpts)
		return
	}
	if provider == "resend" {
		opens, clicks, trackingErr := resendTrackingHeaders(r)
		if trackingErr != nil {
			fail(w, provider, http.StatusBadRequest, "invalid tracking headers")
			return
		}
		msg.RequestTracking(opens, clicks)
	}
	flag, err := protectionFlag(r.Header)
	if err != nil {
		fail(w, provider, http.StatusBadRequest, "invalid X-Thinpost-Enhanced-Protection header")
		return
	}
	// A provider-native request and the Thinpost extension can only strengthen
	// one another; neither may downgrade an explicit TLS requirement.
	msg.EnhancedProtection = msg.EnhancedProtection || flag
	if !s.admits(msg) || msg.Tracking != nil && !s.tracking {
		fail(w, provider, http.StatusBadRequest, "invalid or unsupported message")
		return
	}
	var idem string
	if provider == "resend" {
		idem, err = idempotencyKeyHeader(r)
		if err != nil {
			fail(w, provider, http.StatusBadRequest, "invalid idempotency key")
			return
		}
	}
	receipt, err := s.service.Accept(r.Context(), app, submission.Request{Message: msg, IdempotencyKey: idem, Adapter: provider, SendingDomain: domain, RejectAllSuppressed: provider == "postmark", Hold: hold})
	if err != nil {
		switch {
		case errors.Is(err, submission.ErrInvalidKey):
			fail(w, provider, http.StatusUnauthorized, "invalid API key")
		case errors.Is(err, submission.ErrAllRecipientsSuppressed):
			postmarkError(w, http.StatusUnprocessableEntity, 406, postmarkInactiveMessage)
		case errors.Is(err, submission.ErrInvalidMessage):
			fail(w, provider, http.StatusBadRequest, "invalid or unauthorized message")
		case errors.Is(err, submission.ErrIdempotencyConflict):
			fail(w, provider, http.StatusConflict, "idempotency conflict")
		case errors.Is(err, submission.ErrQuotaExceeded):
			fail(w, provider, http.StatusTooManyRequests, "recipient quota exceeded")
		case errors.Is(err, submission.ErrRateLimited):
			fail(w, provider, http.StatusTooManyRequests, "submission rate exceeded")
		case errors.Is(err, submission.ErrBacklogExceeded):
			fail(w, provider, http.StatusTooManyRequests, "pending message limit exceeded")
		default:
			fail(w, provider, http.StatusServiceUnavailable, "submission unavailable")
		}
		return
	}
	if receipt.ID == "" || receipt.AcceptedAt.IsZero() {
		fail(w, provider, http.StatusServiceUnavailable, "submission unavailable")
		return
	}
	markHeld(w, hold)
	switch provider {
	case "postmark":
		writeJSON(w, http.StatusOK, map[string]any{"To": postmarkTo, "SubmittedAt": receipt.AcceptedAt.UTC().Format("2006-01-02T15:04:05.0000000Z"), "MessageID": providerid.Postmark(receipt.ID), "ErrorCode": 0, "Message": "OK"})
	case "resend":
		writeJSON(w, http.StatusOK, map[string]any{"id": providerid.Resend(receipt.ID)})
	case "sendgrid":
		// Like SendGrid, the single-message response carries the message ID in
		// X-Message-Id. A multi-personalization request is a batch of messages
		// and sends none (see sendGridBatch).
		w.Header().Set("X-Message-Id", receipt.ID)
		w.WriteHeader(http.StatusAccepted)
	case "mailgun":
		writeJSON(w, http.StatusOK, map[string]any{"id": providerid.MailgunResponse(receipt.ID, senderDomain(msg.From)), "message": "Queued. Thank you."})
	}
}

// senderDomain is the lowercase From domain, as the repository records it, so
// a Mailgun response ID matches the webhook message-id for the same message.
func senderDomain(from string) string {
	address, err := mail.ParseAddress(from)
	if err != nil {
		return ""
	}
	at := strings.LastIndexByte(address.Address, '@')
	if at < 1 {
		return ""
	}
	return strings.ToLower(address.Address[at+1:])
}

func idempotencyKeyHeader(r *http.Request) (string, error) {
	values := r.Header.Values("Idempotency-Key")
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 256 || !utf8.ValidString(values[0]) || strings.ContainsRune(values[0], 0) {
		return "", errors.New("invalid idempotency key")
	}
	return values[0], nil
}

func credential(r *http.Request, provider string) (string, bool) {
	if provider == "postmark" {
		v := r.Header.Values("X-Postmark-Server-Token")
		return oneKey(v)
	}
	if provider == "mailgun" {
		if len(r.Header.Values("Authorization")) != 1 {
			return "", false
		}
		user, key, ok := r.BasicAuth()
		if !ok || user != "api" {
			return "", false
		}
		return oneKey([]string{key})
	}
	v := r.Header.Values("Authorization")
	if len(v) != 1 {
		return "", false
	}
	parts := strings.Fields(v[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	return oneKey([]string{parts[1]})
}
func oneKey(values []string) (string, bool) {
	if len(values) != 1 || values[0] == "" || len(values[0]) > 512 || strings.TrimSpace(values[0]) != values[0] {
		return "", false
	}
	return values[0], true
}
func tlsFlag(values []string) (bool, error) {
	if len(values) == 0 {
		return false, nil
	}
	if len(values) != 1 {
		return false, fmt.Errorf("duplicate flag")
	}
	switch strings.ToLower(values[0]) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("invalid flag")
	}
}

// protectionFlag reads X-Thinpost-Enhanced-Protection, which may appear at
// most once and must be "true" or "false". The removed pre-release name
// X-Thinpost-Require-TLS is rejected rather than ignored, so a client still
// sending it fails instead of silently losing Enhanced Protection.
func protectionFlag(h http.Header) (bool, error) {
	if len(h.Values("X-Thinpost-Require-TLS")) != 0 {
		return false, fmt.Errorf("unsupported header")
	}
	return tlsFlag(h.Values("X-Thinpost-Enhanced-Protection"))
}

// holdHeader is the Thinpost extension that accepts a message on hold. A
// successful response to a request that set it carries holdHeader: heldValue,
// so a client can tell that the server honored it: a server that predates
// holds ignores the request header and sends at once.
const (
	holdHeader = "X-Thinpost-Hold"
	heldValue  = "held"
)

// holdFlag reads X-Thinpost-Hold, which may appear at most once and must be
// "true" or "false" (case-insensitive). Absent is false.
func holdFlag(h http.Header) (bool, error) {
	return tlsFlag(h.Values(holdHeader))
}

// markHeld sets the response confirmation for a hold request. It must run
// before the status line is written.
func markHeld(w http.ResponseWriter, hold bool) {
	if hold {
		w.Header().Set(holdHeader, heldValue)
	}
}

// Resend does not expose these per-message options in the supported request
// subset. Thinpost headers apply to one message or every item in a batch. A
// header that is absent leaves the kind to the app default (nil).
func resendTrackingHeaders(r *http.Request) (opens, clicks *bool, err error) {
	if opens, err = optionalFlag(r.Header.Values("X-Thinpost-Track-Opens")); err != nil {
		return nil, nil, err
	}
	if clicks, err = optionalFlag(r.Header.Values("X-Thinpost-Track-Clicks")); err != nil {
		return nil, nil, err
	}
	return opens, clicks, nil
}

// optionalFlag is tlsFlag for options whose absence is meaningful: nil when the
// header is absent, otherwise a pointer to the parsed true or false.
func optionalFlag(values []string) (*bool, error) {
	if len(values) == 0 {
		return nil, nil
	}
	value, err := tlsFlag(values)
	if err != nil {
		return nil, err
	}
	return &value, nil
}
func decodeJSON(r *http.Request, dest any, lim limits) error {
	data, err := readBody(r, lim)
	if err != nil {
		return err
	}
	return decodeJSONBytes(data, dest)
}

// readBody reads the whole request body, at most lim.body bytes of valid UTF-8.
func readBody(r *http.Request, lim limits) ([]byte, error) {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, int64(lim.body)+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errBodyRead, err)
	}
	if len(data) > lim.body || !utf8.Valid(data) {
		return nil, fmt.Errorf("invalid body")
	}
	return data, nil
}

// decodeJSONBytes strictly decodes one JSON value: no duplicate or unknown
// fields and no trailing data.
func decodeJSONBytes(data []byte, dest any) error {
	if err := rejectAmbiguousJSONFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing data")
	}
	return nil
}

type postmarkInput struct {
	From        string          `json:"From"`
	To          string          `json:"To"`
	Cc          string          `json:"Cc"`
	Bcc         string          `json:"Bcc"`
	Subject     string          `json:"Subject"`
	Text        string          `json:"TextBody"`
	HTML        string          `json:"HtmlBody"`
	ReplyTo     string          `json:"ReplyTo"`
	TrackOpens  json.RawMessage `json:"TrackOpens"`
	TrackLinks  json.RawMessage `json:"TrackLinks"`
	Attachments json.RawMessage `json:"Attachments"`
	Headers     json.RawMessage `json:"Headers"`
}

func decodePostmark(r *http.Request, lim limits) (submission.Message, string, error) {
	var input postmarkInput
	if err := decodeJSON(r, &input, lim); err != nil {
		return submission.Message{}, "", err
	}
	if strings.ContainsAny(input.To, "\r\n") {
		return submission.Message{}, "", submission.ErrInvalidMessage
	}
	addresses, err := mail.ParseAddressList(input.To)
	if err != nil {
		return submission.Message{}, "", err
	}
	to := make([]string, len(addresses))
	for i, a := range addresses {
		to[i] = a.String()
	}
	replies, err := parseAddressList(input.ReplyTo)
	if err != nil {
		return submission.Message{}, "", err
	}
	cc, err := parseAddressList(input.Cc)
	if err != nil {
		return submission.Message{}, "", err
	}
	bcc, err := parseAddressList(input.Bcc)
	if err != nil {
		return submission.Message{}, "", err
	}
	var opens, clicks *bool
	if len(input.TrackOpens) > 0 {
		var value bool
		if bytes.Equal(bytes.TrimSpace(input.TrackOpens), []byte("null")) || json.Unmarshal(input.TrackOpens, &value) != nil {
			return submission.Message{}, "", submission.ErrInvalidMessage
		}
		opens = &value
	}
	if len(input.TrackLinks) > 0 {
		var mode string
		if bytes.Equal(bytes.TrimSpace(input.TrackLinks), []byte("null")) || json.Unmarshal(input.TrackLinks, &mode) != nil {
			return submission.Message{}, "", submission.ErrInvalidMessage
		}
		switch mode {
		case "None":
			value := false
			clicks = &value
		case "HtmlOnly":
			value := true
			clicks = &value
		default:
			return submission.Message{}, "", submission.ErrInvalidMessage
		}
	}
	message := submission.Message{From: input.From, To: to, Cc: cc, Bcc: bcc, Subject: input.Subject, Text: input.Text, HTML: input.HTML, ReplyTo: replies}
	if len(input.Attachments) > 0 {
		message.Attachments, err = decodeAttachments(input.Attachments, "postmark", lim.attachment)
		if err != nil {
			return submission.Message{}, "", err
		}
	}
	if len(input.Headers) > 0 {
		pairs, headerErr := postmarkHeaders(input.Headers)
		if headerErr == nil {
			headerErr = applyPassthroughHeaders(&message, pairs)
		}
		if headerErr != nil {
			return submission.Message{}, "", headerErr
		}
	}
	message.RequestTracking(opens, clicks)
	return message, input.To, nil
}

func parseAddressList(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	if strings.ContainsAny(value, "\r\n") {
		return nil, submission.ErrInvalidMessage
	}
	addresses, err := mail.ParseAddressList(value)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		result = append(result, address.String())
	}
	return result, nil
}

// nilIfEmpty turns an empty list into nil so stored sources and fingerprints
// never carry an empty Cc or Bcc.
func nilIfEmpty(list stringList) []string {
	if len(list) == 0 {
		return nil
	}
	return list
}

// stringList supports Resend's single-address and array recipient forms.
type stringList []string

func (s *stringList) UnmarshalJSON(data []byte) error {
	var single string
	if json.Unmarshal(data, &single) == nil {
		*s = []string{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*s = many
	return nil
}
func decodeResend(r *http.Request, lim limits) (submission.Message, error) {
	var input struct {
		From        string          `json:"from"`
		To          stringList      `json:"to"`
		Cc          stringList      `json:"cc"`
		Bcc         stringList      `json:"bcc"`
		Subject     string          `json:"subject"`
		Text        string          `json:"text"`
		HTML        string          `json:"html"`
		ReplyTo     stringList      `json:"reply_to"`
		Attachments json.RawMessage `json:"attachments"`
		Headers     json.RawMessage `json:"headers"`
	}
	if err := decodeJSON(r, &input, lim); err != nil {
		return submission.Message{}, err
	}
	message := submission.Message{From: input.From, To: input.To, Cc: nilIfEmpty(input.Cc), Bcc: nilIfEmpty(input.Bcc), Subject: input.Subject, Text: input.Text, HTML: input.HTML, ReplyTo: input.ReplyTo}
	if len(input.Headers) > 0 {
		pairs, err := objectHeaders(input.Headers)
		if err == nil {
			err = applyPassthroughHeaders(&message, pairs)
		}
		if err != nil {
			return submission.Message{}, err
		}
	}
	if len(input.Attachments) > 0 {
		var err error
		message.Attachments, err = decodeAttachments(input.Attachments, "resend", lim.attachment)
		if err != nil {
			return submission.Message{}, err
		}
	}
	return message, nil
}
func fail(w http.ResponseWriter, provider string, code int, msg string) {
	if provider == "thinpost" {
		nativeFail(w, code, http.StatusText(code))
		return
	}
	if provider == "postmark" {
		writeJSON(w, code, map[string]any{"ErrorCode": code, "Message": msg})
		return
	}
	if provider == "sendgrid" {
		writeJSON(w, code, map[string]any{"errors": []map[string]string{{"message": msg}}})
		return
	}
	if provider == "mailgun" {
		writeJSON(w, code, map[string]any{"message": msg})
		return
	}
	writeJSON(w, code, map[string]any{"statusCode": code, "message": msg, "name": http.StatusText(code)})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
