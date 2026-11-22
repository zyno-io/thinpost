package unsubforward

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/webhook"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

// Retry policy: eight attempts over about 22 hours (the waits after the first
// seven are below), then the forward is marked failed. The recipient's own
// unsubscribe is already recorded and is never undone by a failed forward.
const MaxAttempts = 8

var retryDelays = [MaxAttempts - 1]time.Duration{
	time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour,
}

// RetryDelay is the wait before the next attempt after the given (1-based)
// attempt failed. Past the last attempt it is zero.
func RetryDelay(attempt int) time.Duration {
	if attempt < 1 || attempt > len(retryDelays) {
		return 0
	}
	return retryDelays[attempt-1]
}

// Failure values stored with a failed attempt (a subset of the table's CHECK).
const (
	FailureHTTPStatus = "http_status" // 5xx, 408 or 429: retried
	FailureRejected   = "rejected"    // another status, or a redirect: permanent
	FailureInvalid    = "invalid_link"
	// FailureTargetChanged: the stored message no longer yields a forwardable
	// link on the host recorded at preparation. FailureSourceUnavailable: the
	// stored message could not be read (retried).
	FailureTargetChanged     = "target_changed"
	FailureSourceUnavailable = "source_unavailable"
)

// ErrNoWork is webhook.ErrNoWork so webhook.Run can drive the dispatcher.
var ErrNoWork = webhook.ErrNoWork

// Claim is one leased forward. The sender's link is not stored: the worker
// re-reads it from the delivery's stored source (SourceKey) and requires its
// host to equal Host, recorded at preparation.
type Claim struct {
	DeliveryID, SourceKey, Host, Token string
	EnhancedProtection                 bool
	// Attempt is this attempt's 1-based number.
	Attempt int
}

// Outcome is the result of one attempt.
type Outcome struct {
	// Done: the sender's endpoint answered 2xx.
	Done bool
	// Retry: a failure worth trying again (unless attempts are used up).
	Retry bool
	// Status is the HTTP status when a response arrived, else 0.
	Status int
	// Failure is one of the webhook.Failure* values or the constants above;
	// empty when Done.
	Failure string
}

// Repository leases and records forwards. ClaimForward returns ErrNoWork when
// nothing is due. CommitForward applies the outcome only to the live lease.
type Repository interface {
	ClaimForward(ctx context.Context, token string, ttl time.Duration) (Claim, error)
	CommitForward(ctx context.Context, claim Claim, outcome Outcome) error
}

// Deliverer is the webhook sender's Send.
type Deliverer interface {
	Send(context.Context, string, format.Request, *admin.WebhookAuth) (webhook.Result, error)
}

// Objects reads and decrypts a stored source object.
type Objects interface {
	Get(ctx context.Context, key string, max int64) ([]byte, error)
}

// maxSource mirrors the object store's plaintext limit.
const maxSource = 16 << 20

// Dispatcher sends queued forwards. Sender must be a webhook.Sender with no
// operator destination list: an unsubscribe link is never an internal endpoint.
type Dispatcher struct {
	Repository Repository
	Sender     Deliverer
	Objects    Objects
}

// NewSender returns the sender a Dispatcher should use: the webhook sender's
// public-destination rules (public addresses on port 443 only, resolved and
// vetted at dial time, no redirects, no proxy, short timeouts, capped
// responses) and no internal-destination exceptions.
func NewSender() *webhook.Sender { return &webhook.Sender{Timeout: 15 * time.Second} }

// DispatchNext leases and sends one forward. It is at-least-once: a crash after
// the sender's endpoint accepted the POST but before the result was recorded
// repeats it, which RFC 8058 endpoints must tolerate. It returns ErrNoWork when
// nothing is due.
func (d *Dispatcher) DispatchNext(ctx context.Context) error {
	if d.Repository == nil || d.Sender == nil || d.Objects == nil {
		return errors.New("unsubscribe forwarding is not configured")
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(raw[:])
	claim, err := d.Repository.ClaimForward(ctx, token, 45*time.Second)
	if err != nil {
		return err
	}
	if claim.Token != token || claim.DeliveryID == "" || claim.SourceKey == "" || claim.Host == "" {
		return errors.New("invalid unsubscribe forward claim")
	}
	target, outcome := d.target(ctx, claim)
	if outcome == nil {
		result, sendErr := d.Sender.Send(ctx, target.URL, Request(), nil)
		o := Classify(result, sendErr)
		outcome = &o
	}
	// Only the classified outcome is recorded: no body, header or error text.
	return d.Repository.CommitForward(ctx, claim, *outcome)
}

// target re-derives the sender's link from the delivery's stored source, the
// same object preparation read. It returns an outcome instead when the link
// cannot be used: the source is unreadable (retried), or it no longer yields a
// forwardable link on the recorded host.
func (d *Dispatcher) target(ctx context.Context, claim Claim) (Target, *Outcome) {
	data, err := d.Objects.Get(ctx, claim.SourceKey, maxSource)
	if err != nil {
		return Target{}, &Outcome{Retry: true, Failure: FailureSourceUnavailable}
	}
	payload, err := submission.DecodeSource(data, claim.EnhancedProtection)
	if err != nil {
		return Target{}, &Outcome{Failure: FailureTargetChanged}
	}
	original, err := OriginalOf(payload)
	if err != nil || !original.Present() {
		return Target{}, &Outcome{Failure: FailureTargetChanged}
	}
	target, reason := Decide(original)
	if reason != "" || target.Host != claim.Host {
		return Target{}, &Outcome{Failure: FailureTargetChanged}
	}
	return target, nil
}

// OriginalOf reads a stored source's own List-Unsubscribe headers: the scanned
// raw message for SMTP, the stored message for the HTTP APIs. Preparation and
// the forwarding worker both use it, so they agree.
func OriginalOf(payload submission.Source) (Original, error) {
	if payload.Version == submission.RawSourceVersion {
		scanned, err := rawmime.Scan(payload.Raw)
		if err != nil {
			return Original{}, err
		}
		return Original{
			ListFields: scanned.UnsubscribeFields, Value: scanned.UnsubscribeValue,
			PostFields: scanned.UnsubscribePostFields, PostValue: scanned.UnsubscribePostValue,
		}, nil
	}
	var o Original
	if payload.Message.ListUnsubscribe != "" {
		o.ListFields, o.Value = 1, payload.Message.ListUnsubscribe
	}
	if payload.Message.ListUnsubscribePost {
		o.PostFields, o.PostValue = 1, OneClickPost
	}
	return o, nil
}

// Request is the RFC 8058 one-click POST.
func Request() format.Request {
	return format.Request{
		Body:   []byte(OneClickPost),
		Header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}, "User-Agent": {"Thinpost-Unsubscribe/1"}},
	}
}

// Classify turns a send result into an outcome. A destination that is not
// public, a refusal (4xx other than 408 and 429) and a redirect (never
// followed) are permanent; everything else that did not succeed is retried.
func Classify(result webhook.Result, sendErr error) Outcome {
	switch {
	case sendErr != nil:
		return Outcome{Failure: FailureInvalid}
	case result.Success:
		return Outcome{Done: true, Status: result.StatusCode}
	case result.Failure == webhook.FailureBlocked:
		return Outcome{Failure: webhook.FailureBlocked}
	case result.Failure != "":
		return Outcome{Retry: true, Failure: result.Failure}
	case result.StatusCode == http.StatusRequestTimeout, result.StatusCode == http.StatusTooManyRequests, result.StatusCode >= 500:
		return Outcome{Retry: true, Status: result.StatusCode, Failure: FailureHTTPStatus}
	}
	return Outcome{Status: result.StatusCode, Failure: FailureRejected}
}
