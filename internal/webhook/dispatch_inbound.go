package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/zyno-io/thinpost/internal/webhook/format"
)

// The inbound lane delivers received mail. Its payload is far larger than an
// outbound event's and its receiver may be slow, so it has its own claim, a
// longer claim TTL, fewer workers, a longer HTTP timeout and a memory budget.
const (
	// InboundClaimTTL covers the budget wait, the object read, the render and
	// the 120 second send with room to spare.
	InboundClaimTTL = 3 * time.Minute
	// InboundWorkers is the number of inbound workers per replica.
	InboundWorkers = 2
	// RenderBudgetBytes bounds the memory of all renders and sends in flight in
	// one replica (control runs under a 460 MiB memory limit).
	RenderBudgetBytes = 48 << 20

	// ClassContentUnavailable is the fixed failure class of a delivery whose
	// message content is gone or past retention. It is permanent: retrying
	// cannot bring the content back.
	ClassContentUnavailable = "content_unavailable"

	// BudgetDeferral is how long a delivery waits when the render budget was busy.
	BudgetDeferral = 15 * time.Second

	budgetWait   = 20 * time.Second
	shutdownWait = 5 * time.Second
	objectWait   = 20 * time.Second
	maxObject    = 16 << 20 // the schema's size bound
)

// ErrContentMissing is returned by Objects when the stored message no longer
// exists; any other error is treated as a temporary storage failure.
var ErrContentMissing = errors.New("inbound content is missing")

// InboundData is what an inbound delivery needs from storage: the recorded
// envelope, results and correlation, and where the stored message is. It holds
// no message content.
type InboundData struct {
	InboundID  string
	Recipient  string
	Recipients []string
	ReceivedAt time.Time
	ObjectKey  string
	Size       int
	// Expired is set by the database clock when content_expires_at has passed.
	Expired        bool
	MailFrom       string
	ClientIP       string
	Helo           string
	TLSVersion     string
	TLSCipher      string
	Receiver       string
	Authentication []byte
	SPF            string
	DKIM           string
	DMARC          string
	MessageID      string
	DeliveryID     string
	Header         string
	FromMatches    *bool
}

// InboundRepository is the inbound lane's durable side. CommitWebhook records
// an HTTP result; FailWebhook ends the delivery permanently with a fixed class.
type InboundRepository interface {
	ClaimWebhookInbound(context.Context, string, time.Duration) (Claim, error)
	CommitWebhook(context.Context, Claim, int) error
	FailWebhook(context.Context, Claim, string) error
	// DeferWebhook puts a claimed delivery back in the queue after the delay
	// without using an attempt (the attempt row is recorded as deferred).
	DeferWebhook(context.Context, Claim, time.Duration) error
}

// Objects reads stored inbound messages, decrypted. It returns ErrContentMissing
// when the object does not exist.
type Objects interface {
	GetMIME(ctx context.Context, key string, maximum int64) ([]byte, error)
}

// RenderBudget is a byte budget shared by the inbound workers of a replica.
// A request that cannot be satisfied within the wait is refused, never queued
// without bound.
type RenderBudget struct {
	mu    sync.Mutex
	total int64
	free  int64
	wake  chan struct{}
}

func NewRenderBudget(total int64) *RenderBudget {
	return &RenderBudget{total: total, free: total, wake: make(chan struct{})}
}

var ErrBudgetBusy = errors.New("render budget busy")

// Acquire reserves n bytes (at most the whole budget) and returns the release
// function, or ErrBudgetBusy once wait has passed.
func (b *RenderBudget) Acquire(ctx context.Context, n int64, wait time.Duration) (func(), error) {
	n = min(max(n, 1), b.total)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		b.mu.Lock()
		if b.free >= n {
			b.free -= n
			b.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					b.mu.Lock()
					b.free += n
					close(b.wake)
					b.wake = make(chan struct{})
					b.mu.Unlock()
				})
			}, nil
		}
		wake := b.wake
		b.mu.Unlock()
		select {
		case <-wake:
		case <-timer.C:
			return nil, ErrBudgetBusy
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// InboundDispatcher delivers inbound mail. Like Dispatcher it is at least
// once: a result lost after the receiver accepted may repeat the event, so
// consumers deduplicate by event ID (or message ID).
type InboundDispatcher struct {
	Repository InboundRepository
	Sender     Deliverer
	Keys       Keys
	Objects    Objects
	Budget     *RenderBudget
	// BudgetWait overrides the 20 second wait for the budget (tests).
	BudgetWait time.Duration
	Now        func() time.Time
}

func (d *InboundDispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *InboundDispatcher) DispatchNext(ctx context.Context) error {
	if d.Repository == nil || d.Sender == nil || d.Keys == nil || d.Objects == nil || d.Budget == nil {
		return errors.New("webhook dispatcher is not configured")
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(raw[:])
	claim, err := d.Repository.ClaimWebhookInbound(ctx, token, InboundClaimTTL)
	if errors.Is(err, ErrSkipped) {
		return nil
	}
	if err != nil {
		return err
	}
	if claim.Token != token || claim.Event.ID == "" || claim.AppID == "" || claim.SubscriptionID == "" || claim.Inbound == nil {
		return errors.New("invalid webhook claim")
	}
	status, class, deferred := d.deliver(ctx, claim)
	// A stopping process must not leave the claim to expire as an unknown
	// attempt: finish on a short context of its own. Nothing was received when
	// the attempt was cut short, so it goes back to the queue unused.
	finish := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		finish, cancel = context.WithTimeout(context.WithoutCancel(ctx), shutdownWait)
		defer cancel()
		if class == "" && status == 0 {
			deferred = true
		}
	}
	switch {
	case deferred:
		return d.Repository.DeferWebhook(finish, claim, BudgetDeferral)
	case class != "":
		return d.Repository.FailWebhook(finish, claim, class)
	}
	// Status zero deliberately records no remote body, secret, URL or error.
	return d.Repository.CommitWebhook(finish, claim, status)
}

// deliver reads the stored message, renders the payload and sends it. It
// returns the HTTP status (zero when none arrived or the attempt failed before
// sending), a permanent failure class, or deferred when the render budget was
// busy: nothing was attempted, so the delivery goes back to the queue without
// using an attempt.
func (d *InboundDispatcher) deliver(ctx context.Context, claim Claim) (status int, class string, deferred bool) {
	data := claim.Inbound
	if data.Expired {
		return 0, ClassContentUnavailable, false
	}
	if !format.InboundFormat(claim.Format) {
		log.Printf("webhook request unavailable for endpoint %s", claim.SubscriptionID)
		return 0, "", false
	}
	secret, auth, ok := openCredentials(d.Keys, claim)
	if !ok {
		return 0, "", false
	}
	wait := budgetWait
	if d.BudgetWait > 0 {
		wait = d.BudgetWait
	}
	release, err := d.Budget.Acquire(ctx, format.RenderPeakBytes(data.Size), wait)
	if errors.Is(err, ErrBudgetBusy) {
		return 0, "", true
	}
	if err != nil {
		return 0, "", false
	}
	defer release()
	getCtx, cancel := context.WithTimeout(ctx, objectWait)
	stored, err := d.Objects.GetMIME(getCtx, data.ObjectKey, maxObject)
	cancel()
	if errors.Is(err, ErrContentMissing) {
		return 0, ClassContentUnavailable, false
	}
	if err != nil || len(stored) == 0 {
		// A storage error says nothing about the content; try again later.
		return 0, "", false
	}
	request, err := d.render(claim, secret, stored)
	if err != nil {
		log.Printf("webhook request unavailable for endpoint %s", claim.SubscriptionID)
		return 0, "", false
	}
	// The stored message is unreachable from here on: only the body is held
	// while the receiver answers.
	if internalBlocked(d.Sender, claim.Endpoint, claim.Format, auth != nil) {
		return 0, "", false
	}
	if result, sendErr := d.Sender.Send(ctx, claim.Endpoint, request, auth); sendErr == nil {
		return result.StatusCode, "", false
	}
	return 0, "", false
}

// render is its own function so the stored message is released before the send.
func (d *InboundDispatcher) render(claim Claim, secret, stored []byte) (format.Request, error) {
	data := claim.Inbound
	impl, ok := format.Lookup(claim.Format)
	if !ok {
		return format.Request{}, errors.New("unknown format")
	}
	input := &format.InboundInput{
		ID: data.InboundID, ReceivedAt: data.ReceivedAt, Recipient: data.Recipient, Recipients: data.Recipients, MailFrom: data.MailFrom,
		ClientIP: data.ClientIP, Helo: data.Helo, TLSVersion: data.TLSVersion, TLSCipher: data.TLSCipher, Receiver: data.Receiver, Raw: stored,
		Authentication: data.Authentication, SPF: data.SPF, DKIM: data.DKIM, DMARC: data.DMARC,
	}
	if data.MessageID != "" {
		input.Correlation = &format.InboundCorrelation{MessageID: data.MessageID, DeliveryID: data.DeliveryID, Header: data.Header, FromMatches: data.FromMatches}
	}
	return impl.Build(format.Input{Event: claim.Event, AppID: claim.AppID, Secret: secret, Now: d.now(), Inbound: input})
}
