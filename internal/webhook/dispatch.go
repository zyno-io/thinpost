package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/webhook/format"
)

var ErrNoWork = errors.New("no webhook delivery available")
var ErrLostClaim = errors.New("webhook claim expired or was replaced")

// ErrSkipped reports a claimed delivery that its format can no longer
// represent; it was cancelled without an attempt.
var ErrSkipped = errors.New("webhook delivery skipped")

// Event is one webhook_events row; see package format.
type Event = format.Event

type Claim struct {
	Event                                  Event
	AppID, SubscriptionID, Endpoint, Token string
	Format                                 string
	SealedSecret                           []byte // nil for unsigned formats
	SealedAuth                             []byte // nil when the endpoint has no authentication
	Context                                format.Context
	Attempt                                int
	// Inbound is set only by the inbound lane's claim.
	Inbound *InboundData
}
type Repository interface {
	ClaimWebhook(context.Context, string, time.Duration) (Claim, error)
	CommitWebhook(context.Context, Claim, int) error
}
type Deliverer interface {
	Send(context.Context, string, format.Request, *admin.WebhookAuth) (Result, error)
}

// Keys opens a signing secret sealed by objectstore.Keyring.Seal.
type Keys interface {
	Open(aad, envelope []byte) ([]byte, error)
}

type Dispatcher struct {
	Repository Repository
	Sender     Deliverer
	Keys       Keys
}

// DispatchNext uses at-least-once delivery: if the process dies after a webhook
// is accepted but before its result is committed, a later attempt may repeat
// the event. Consumers must deduplicate by event ID.
func (d *Dispatcher) DispatchNext(ctx context.Context) error {
	if d.Repository == nil || d.Sender == nil || d.Keys == nil {
		return errors.New("webhook dispatcher is not configured")
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(raw[:])
	claim, err := d.Repository.ClaimWebhook(ctx, token, 45*time.Second)
	if errors.Is(err, ErrSkipped) {
		return nil
	}
	if err != nil {
		return err
	}
	if claim.Token != token || claim.Event.ID == "" || claim.AppID == "" || claim.SubscriptionID == "" {
		return errors.New("invalid webhook claim")
	}
	if claim.Format == "" {
		claim.Format = format.Thinpost
	}
	var status int
	if internalBlocked(d.Sender, claim.Endpoint, claim.Format, claim.SealedAuth != nil) {
		log.Printf("webhook endpoint %s is not permitted for an internal destination", claim.SubscriptionID)
	} else if request, auth, ok := d.build(claim); ok {
		if result, sendErr := d.Sender.Send(ctx, claim.Endpoint, request, auth); sendErr == nil {
			status = result.StatusCode
		}
	}
	// Status zero deliberately records no remote body, secret, URL or error.
	return d.Repository.CommitWebhook(ctx, claim, status)
}

// build renders the request and opens the endpoint's sealed secret and
// authentication. Any failure logs only the endpoint ID.
func (d *Dispatcher) build(claim Claim) (format.Request, *admin.WebhookAuth, bool) {
	impl, known := format.Lookup(claim.Format)
	if !known {
		log.Printf("webhook request unavailable for endpoint %s", claim.SubscriptionID)
		return format.Request{}, nil, false
	}
	secret, auth, ok := openCredentials(d.Keys, claim)
	if !ok {
		return format.Request{}, nil, false
	}
	request, err := impl.Build(format.Input{Event: claim.Event, Context: claim.Context, AppID: claim.AppID, Secret: secret, Now: time.Now()})
	if err != nil {
		log.Printf("webhook request unavailable for endpoint %s", claim.SubscriptionID)
		return format.Request{}, nil, false
	}
	return request, auth, true
}

// openCredentials opens the endpoint's sealed signing secret and HTTP
// authentication (each nil when the endpoint has none). Any failure logs only
// the endpoint ID.
func openCredentials(keys Keys, claim Claim) (secret []byte, auth *admin.WebhookAuth, ok bool) {
	if claim.SealedSecret != nil {
		var err error
		if secret, err = keys.Open(SecretAAD(claim.AppID, claim.SubscriptionID), claim.SealedSecret); err != nil {
			log.Printf("webhook secret unavailable for endpoint %s", claim.SubscriptionID)
			return nil, nil, false
		}
	}
	if claim.SealedAuth == nil {
		return secret, nil, true
	}
	plain, err := keys.Open(AuthAAD(claim.AppID, claim.SubscriptionID), claim.SealedAuth)
	if err == nil {
		var opened admin.WebhookAuth
		if opened, err = admin.OpenWebhookAuth(plain); err == nil {
			return secret, &opened, true
		}
	}
	log.Printf("webhook request unavailable for endpoint %s", claim.SubscriptionID)
	return nil, nil, false
}
