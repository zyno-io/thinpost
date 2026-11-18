// Package dispatch prepares a committed delivery attempt for an edge worker.
// DNS policy, bounce addressing and runtime scheduling are injected and must
// be wired before this provider is used in production.
package dispatch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/mail"
	"net/netip"
	"slices"
	"strings"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/postgres"
	"github.com/zyno-io/thinpost/internal/submission"
)

type Repository interface {
	ClaimDelivery(context.Context, postgres.ControllerLease, string, delivery.EdgeCapabilities) (delivery.Claim, error)
	CommitDeliveryResultIdentified(context.Context, delivery.Result) (delivery.Committed, error)
}
type Objects interface {
	Get(context.Context, string, int64) ([]byte, error)
}
type Route struct {
	MXName  string
	Address netip.Addr
}
type Routes interface {
	ForRecipient(context.Context, string, string, int) (Route, error)
}
type RecipientPolicy interface{ Allows(string) bool }

// Envelopes must produce a stable opaque bounce address and persist its
// correlation before returning. It must not use a public message ID as a token.
type Envelopes interface {
	ForDelivery(context.Context, delivery.Claim) (string, error)
}
type Provider struct {
	Repository Repository
	Objects    Objects
	Routes     Routes
	Envelopes  Envelopes
	Recipients RecipientPolicy
}

// Next claims one delivery the connected worker can serve. caps is what that
// worker reported (or the legacy equivalent): the DeliveryStart source fields
// are filled only for a worker that reported sources and only with an address it
// reported, so a legacy worker never receives them and never sends from an
// address control did not report for it.
func (p *Provider) Next(ctx context.Context, lease postgres.ControllerLease, caps delivery.EdgeCapabilities) (*controlv1.DeliveryStart, []byte, error) {
	if p.Repository == nil || p.Objects == nil || p.Routes == nil || p.Envelopes == nil || p.Recipients == nil {
		return nil, nil, errors.New("delivery provider is not configured")
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, nil, err
	}
	id := hex.EncodeToString(raw[:])
	claim, err := p.Repository.ClaimDelivery(ctx, lease, id, caps)
	if err != nil {
		return nil, nil, err
	}
	if claim.AttemptID != id || claim.WorkerID != lease.WorkerID || claim.Epoch != lease.Epoch {
		return nil, nil, errors.New("delivery claim does not match controller lease")
	}
	start, mime, err := p.prepare(ctx, claim, caps)
	if err != nil {
		// No SMTP attempt was sent. Persist a local result before
		// allowing retry; if that commit fails, retain the in-flight attempt for
		// reconciliation instead of falsely claiming it is safe to retry.
		result := delivery.Result{AttemptID: claim.AttemptID, DeliveryID: claim.DeliveryID, WorkerID: claim.WorkerID, Epoch: claim.Epoch, Outcome: "rejected"}
		if errors.Is(err, delivery.ErrNullMX) {
			result.Outcome = delivery.OutcomeNullMX
		} else if errors.Is(err, delivery.ErrDomainNotFound) {
			result.Outcome = delivery.OutcomeDomainNotFound
		} else if errors.Is(err, delivery.ErrRecipientPolicyDenied) {
			result.Outcome = delivery.OutcomeRecipientPolicyDenied
		}
		if _, commitErr := p.Repository.CommitDeliveryResultIdentified(ctx, result); commitErr != nil {
			return nil, nil, errors.New("delivery preparation result could not be committed")
		}
		oplog.Delivery(claim.AppID, claim.MessageID, claim.DeliveryID, oplog.Status(result.Outcome), 0, claim.SourceName)
		return nil, nil, delivery.ErrNoWork
	}
	return start, mime, nil
}
func (p *Provider) prepare(ctx context.Context, claim delivery.Claim, caps delivery.EdgeCapabilities) (*controlv1.DeliveryStart, []byte, error) {
	if claim.AttemptID == "" || claim.DeliveryID == "" || claim.Epoch == 0 || claim.RecipientIndex < 0 || claim.RouteAttempt < 0 {
		return nil, nil, errors.New("invalid delivery claim")
	}
	// Defense in depth behind the claim query: never hand a worker a delivery
	// whose source or bounce domain it did not report. The attempt is committed
	// as a local rejection (retried later), and nothing is sent.
	if claim.ResolvedAddress != "" && !caps.Has(claim.ResolvedAddress) {
		return nil, nil, errors.New("delivery source is not available on this worker")
	}
	if claim.SourceAddress != "" || claim.HeloName != "" {
		if !caps.Explicit || claim.SourceAddress == "" || claim.HeloName == "" || !caps.Has(claim.SourceAddress) {
			return nil, nil, errors.New("delivery source is not available on this worker")
		}
	}
	if claim.BounceDomain != "" && !slices.Contains(caps.BounceDomains, claim.BounceDomain) {
		return nil, nil, errors.New("delivery bounce domain is not available on this worker")
	}
	source, err := p.Objects.Get(ctx, claim.SourceKey, 16<<20)
	if err != nil {
		return nil, nil, err
	}
	object, err := submission.DecodeSource(source, claim.EnhancedProtection)
	if err != nil || claim.RecipientIndex >= len(object.Message.Envelope()) {
		return nil, nil, errors.New("invalid source message")
	}
	recipient, err := mail.ParseAddress(object.Message.Envelope()[claim.RecipientIndex])
	if err != nil {
		return nil, nil, err
	}
	if !p.Recipients.Allows(recipient.Address) {
		return nil, nil, delivery.ErrRecipientPolicyDenied
	}
	route, err := p.Routes.ForRecipient(ctx, recipient.Address, claim.DeliveryID, claim.RouteAttempt)
	if errors.Is(err, delivery.ErrNullMX) || errors.Is(err, delivery.ErrDomainNotFound) {
		return nil, nil, err
	}
	if err != nil || route.MXName == "" || strings.ContainsAny(route.MXName, "\r\n ") || !route.Address.IsValid() || route.Address.Zone() != "" {
		return nil, nil, errors.New("recipient route unavailable")
	}
	from, err := p.Envelopes.ForDelivery(ctx, claim)
	if err != nil || from == "" {
		return nil, nil, errors.New("bounce address unavailable")
	}
	mime, err := p.Objects.Get(ctx, claim.FinalKey, edgeprotocol.MaxMIMEBytes)
	if err != nil || len(mime) == 0 {
		return nil, nil, errors.New("signed MIME unavailable")
	}
	digest := sha256.Sum256(mime)
	var trackingCutoff int64
	if !claim.TrackingSendCutoff.IsZero() {
		trackingCutoff = claim.TrackingSendCutoff.UnixMilli()
	}
	return &controlv1.DeliveryStart{AttemptId: claim.AttemptID, DeliveryId: claim.DeliveryID, Epoch: claim.Epoch, MxName: route.MXName, DestinationIp: route.Address.String(), EnvelopeFrom: from, Recipient: recipient.Address, EnhancedProtection: claim.EnhancedProtection, ExpectedSize: uint64(len(mime)), Sha256: digest[:], TrackingSendCutoffUnixMs: trackingCutoff, SourceAddress: claim.SourceAddress, HeloName: claim.HeloName}, mime, nil
}
