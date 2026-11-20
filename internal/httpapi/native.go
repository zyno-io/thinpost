package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/submission"
)

// The native API is Thinpost's own JSON submission route. It decodes into the
// same submission.Message as the provider adapters and goes through the same
// Service.Accept; it has no policy of its own beyond the shape of its body.
// The operator console's test send decodes the same body (see
// SubmitTestMessage) under a narrower profile.

// nativePath is the native send route. Message reads share its prefix.
const nativePath = "/app/v1/messages"

// maxTestBody bounds the operator test-send body.
const maxTestBody = 64 << 10

// maxTestRecipients is the most To addresses in an operator test send.
const maxTestRecipients = 5

// nativeInput is the native request body. Recipient fields are arrays only.
type nativeInput struct {
	From               string          `json:"from"`
	To                 []string        `json:"to"`
	Cc                 []string        `json:"cc"`
	Bcc                []string        `json:"bcc"`
	ReplyTo            []string        `json:"reply_to"`
	Subject            string          `json:"subject"`
	Text               string          `json:"text"`
	HTML               string          `json:"html"`
	Headers            json.RawMessage `json:"headers"`
	Attachments        json.RawMessage `json:"attachments"`
	EnhancedProtection bool            `json:"enhanced_protection"`
	Tracking           json.RawMessage `json:"tracking"`
	Hold               bool            `json:"hold"`
	// BypassLimits and BypassRecipientPolicy are the operator test-send
	// overrides. They are in testKeys only: every other route rejects them.
	BypassLimits          bool `json:"bypass_limits"`
	BypassRecipientPolicy bool `json:"bypass_recipient_policy"`
}

// nativeOptions carries what a decoded body asked for beyond the message, and
// which optional parts it used, so a narrower profile can refuse them.
type nativeOptions struct {
	Hold bool
	// Overrides are the operator test-send overrides. They are only ever set
	// by decoding with testKeys, so a native request cannot carry them.
	Overrides submission.Overrides
	// Headers, Attachments and Tracking report that the body contained the field.
	Headers, Attachments, Tracking bool
	// Present holds the top-level keys the body contained.
	Present map[string]bool
}

// nativeKeys are the keys the native app-key route accepts, matched exactly.
// The override flags are deliberately absent: the native route rejects them
// with 400 even when false, so a client cannot discover or probe a bypass.
var nativeKeys = map[string]bool{"from": true, "to": true, "cc": true, "bcc": true, "reply_to": true, "subject": true, "text": true, "html": true, "headers": true, "attachments": true, "enhanced_protection": true, "tracking": true, "hold": true}

// testKeys are the keys of the operator test send: the native keys plus the
// two operator-only overrides.
var testKeys = func() map[string]bool {
	keys := map[string]bool{"bypass_limits": true, "bypass_recipient_policy": true}
	for key := range nativeKeys {
		keys[key] = true
	}
	return keys
}()

// testForbiddenKeys may not appear at all in an operator test send, even empty.
var testForbiddenKeys = []string{"cc", "bcc", "reply_to", "headers", "attachments", "tracking", "hold"}

// decodeNative strictly decodes a native request body: no duplicate, unknown
// or null-valued optional objects. Errors never include request content. A body
// that cannot be decoded is submission.ErrInvalidRequest.
func decodeNative(data []byte, lim limits, keys map[string]bool) (submission.Message, nativeOptions, error) {
	var opts nativeOptions
	if len(data) > lim.body || !utf8.Valid(data) {
		return submission.Message{}, opts, submission.ErrInvalidRequest
	}
	// encoding/json matches keys case-insensitively and reads null as absent,
	// so check the top level first: exact known keys, no null values.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return submission.Message{}, opts, submission.ErrInvalidRequest
	}
	opts.Present = make(map[string]bool, len(fields))
	for key, value := range fields {
		if !keys[key] || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return submission.Message{}, opts, submission.ErrInvalidRequest
		}
		opts.Present[key] = true
	}
	var input nativeInput
	if err := decodeJSONBytes(data, &input); err != nil {
		return submission.Message{}, opts, submission.ErrInvalidRequest
	}
	message := submission.Message{From: input.From, To: nilIfEmpty(input.To), Cc: nilIfEmpty(input.Cc), Bcc: nilIfEmpty(input.Bcc), ReplyTo: nilIfEmpty(input.ReplyTo), Subject: input.Subject, Text: input.Text, HTML: input.HTML, EnhancedProtection: input.EnhancedProtection}
	opts.Hold = input.Hold
	opts.Overrides = submission.Overrides{Limits: input.BypassLimits, RecipientPolicy: input.BypassRecipientPolicy}
	if input.Headers != nil {
		opts.Headers = true
		pairs, err := objectHeaders(input.Headers)
		if err != nil {
			return submission.Message{}, opts, submission.ErrInvalidRequest
		}
		for _, pair := range pairs {
			// Reply addresses have their own field here, unlike in Resend.
			if asciiLower(pair.name) == "reply-to" {
				return submission.Message{}, opts, submission.ErrInvalidRequest
			}
		}
		if err := applyPassthroughHeaders(&message, pairs); err != nil {
			return submission.Message{}, opts, submission.ErrInvalidRequest
		}
	}
	if input.Attachments != nil {
		opts.Attachments = true
		attachments, err := decodeAttachments(input.Attachments, "thinpost", lim.attachment)
		if err != nil {
			return submission.Message{}, opts, submission.ErrInvalidRequest
		}
		message.Attachments = attachments
	}
	if input.Tracking != nil {
		opts.Tracking = true
		var choice struct {
			Opens  json.RawMessage `json:"opens"`
			Clicks json.RawMessage `json:"clicks"`
		}
		if err := decodeStrictObject(input.Tracking, &choice); err != nil {
			return submission.Message{}, opts, submission.ErrInvalidRequest
		}
		opens, okOpens := optionalBool(choice.Opens)
		clicks, okClicks := optionalBool(choice.Clicks)
		if !okOpens || !okClicks {
			return submission.Message{}, opts, submission.ErrInvalidRequest
		}
		message.RequestTracking(opens, clicks)
	}
	return message, opts, nil
}

// optionalBool is nil for an absent value, a pointer to a JSON boolean, and
// not ok for anything else, null included.
func optionalBool(raw json.RawMessage) (*bool, bool) {
	if raw == nil {
		return nil, true
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	return &value, true
}

// acceptNative applies the admission checks every route shares and hands the
// message to the one acceptance path. adapter is AdapterNative or
// AdapterConsole; the service enforces the pairing with the principal.
func (s *Server) acceptNative(ctx context.Context, app submission.App, msg submission.Message, opts nativeOptions, idem, adapter string) (submission.Receipt, error) {
	if !s.admits(msg) || msg.Tracking != nil && !s.tracking {
		return submission.Receipt{}, submission.ErrInvalidRequest
	}
	return s.service.Accept(ctx, app, submission.Request{Message: msg, IdempotencyKey: idem, Adapter: adapter, Hold: opts.Hold, Overrides: opts.Overrides})
}

// SubmitTestMessage submits a message as the app on behalf of the
// administrator actor, who the caller must already have authenticated. It is
// the operator console's test send: the body is the native body under a
// constrained profile (1 to 5 To recipients; no Cc, Bcc, reply addresses,
// headers, attachments, tracking choice or hold), and it is accepted through
// the same Service.Accept as every other submission, so quotas, rate limits,
// sender-domain and recipient policy all apply unless the body sets one of the
// two operator-only options: bypass_limits skips, and does not count, the
// sending limits; bypass_recipient_policy skips the recipient allowlist. Nothing
// else is relaxed. It never uses an idempotency key.
func (s *Server) SubmitTestMessage(ctx context.Context, actor, appID string, body []byte) (submission.Receipt, error) {
	if s.service == nil {
		return submission.Receipt{}, submission.ErrUnavailable
	}
	operators, ok := s.service.(submission.OperatorService)
	if !ok {
		return submission.Receipt{}, submission.ErrUnavailable
	}
	app, err := operators.AuthorizeOperator(ctx, appID, actor)
	if err != nil {
		return submission.Receipt{}, err
	}
	msg, opts, err := decodeNative(body, limits{body: maxTestBody, attachment: s.attachmentLimit}, testKeys)
	if err != nil {
		return submission.Receipt{}, err
	}
	for _, key := range testForbiddenKeys {
		if opts.Present[key] {
			return submission.Receipt{}, submission.ErrInvalidRequest
		}
	}
	if len(msg.To) < 1 || len(msg.To) > maxTestRecipients || len(msg.Cc) > 0 || len(msg.Bcc) > 0 || len(msg.ReplyTo) > 0 || opts.Hold || opts.Headers || opts.Attachments || opts.Tracking {
		return submission.Receipt{}, submission.ErrInvalidRequest
	}
	return s.acceptNative(ctx, app, msg, opts, "", submission.AdapterConsole)
}

// nativeHeaders sets the headers of every native response. Like the app read
// API, a response is never cached and carries no sniffable or framable body.
func nativeHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; sandbox")
	header.Set("Referrer-Policy", "no-referrer")
}

// nativeRejectedHeader reports a Thinpost option header, which the native
// route takes in its body instead.
func nativeRejectedHeader(h http.Header) bool {
	for _, name := range []string{holdHeader, "X-Thinpost-Enhanced-Protection", "X-Thinpost-Require-TLS", "X-Thinpost-Track-Opens", "X-Thinpost-Track-Clicks"} {
		if len(h.Values(name)) > 0 {
			return true
		}
	}
	return false
}

// serveNative accepts a decoded native message and writes the response:
// 201 for a new message, 200 for an idempotent replay.
func (s *Server) serveNative(w http.ResponseWriter, r *http.Request, app submission.App, msg submission.Message, opts nativeOptions) {
	idem, err := idempotencyKeyHeader(r)
	if err != nil {
		nativeFail(w, http.StatusBadRequest, "Bad Request")
		return
	}
	receipt, err := s.acceptNative(r.Context(), app, msg, opts, idem, submission.AdapterNative)
	if err != nil {
		status, code := admin.SubmissionFailure(err)
		nativeFail(w, status, code)
		return
	}
	if receipt.ID == "" || receipt.AcceptedAt.IsZero() {
		nativeFail(w, http.StatusServiceUnavailable, "Service Unavailable")
		return
	}
	markHeld(w, opts.Hold)
	w.Header().Set("Location", nativePath+"/"+receipt.ID)
	status := http.StatusCreated
	if receipt.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]string{"id": receipt.ID, "accepted_at": receipt.AcceptedAt.UTC().Format(time.RFC3339)})
}

func nativeFail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
