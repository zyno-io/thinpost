// Package preparation turns accepted source submissions into immutable,
// per-recipient signed artifacts before any delivery is queued.
package preparation

import (
	"context"
	"crypto"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/message"
	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/receivedtrace"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/tracking"
	"github.com/zyno-io/thinpost/internal/unsubforward"
)

var ErrNoWork = errors.New("no submission to prepare")
var ErrLostLease = errors.New("preparation lease is no longer valid")

const maxSourceSize = 16 << 20

type Claim struct {
	MessageID, AppID, SourceKey, Token string
	SigningDomain                      string
	// SigningKeyID is the DKIM key that signed the message (empty when the key
	// source has no ids). The commit requires it to still be the domain's
	// active key, so a key switched out mid-preparation cannot sign a message
	// that then becomes visible: the commit fails and the message is prepared
	// again with the new key.
	SigningKeyID string
	// SenderDomain is the From domain recorded at acceptance (lowercase). The
	// shared Message-ID is built on it, so preparation requires the signing key
	// to be for exactly this domain.
	SenderDomain string
	AcceptedAt   time.Time
	// Date is the Date header: the release time for a message submitted on
	// hold, otherwise AcceptedAt. Zero means AcceptedAt.
	Date               time.Time
	EnhancedProtection bool
	// ListUnsubscribe is the app's one-click opt-in as read by the claim. A
	// toggle that races preparation is harmless: either state is a valid
	// outcome for a message accepted around the change.
	ListUnsubscribe bool
	// TrackingDomain is the app's own tracking domain for this message's sender
	// domain, when one is active and was checked recently (the most specific
	// match wins); empty otherwise. It is used only when the deployment has
	// custom domains enabled (TrackingConfig.CustomDomains).
	TrackingDomain string
}

func (c Claim) date() time.Time {
	if c.Date.IsZero() {
		return c.AcceptedAt
	}
	return c.Date
}

type Delivery struct {
	ID             string
	RecipientIndex int
	ObjectKey      string
	Tracking       *Tracking
	// Forward is set when the message had its own List-Unsubscribe headers that
	// Thinpost's one-click headers replaced (see Forward).
	Forward *Forward
}

// Forward records what to do with the sender's own unsubscribe link when the
// recipient unsubscribes through Thinpost's link. Host is set when the original
// headers allow an RFC 8058 POST (the link itself is not copied: the worker
// reads it from the stored source and checks its host against this one);
// otherwise Reason says why not (an unsubforward.Reason* value).
type Forward struct {
	Host, Reason string
}

// forwardFor decides the Forward of a message whose own List-Unsubscribe headers
// are being replaced. It is nil for a message without them. It cannot fail: any
// original it cannot use is recorded as unusable, and preparation goes on.
func forwardFor(original unsubforward.Original, err error) *Forward {
	if err != nil {
		return &Forward{Reason: unsubforward.ReasonUnusable}
	}
	if !original.Present() {
		return nil
	}
	target, reason := unsubforward.Decide(original)
	if reason != "" {
		return &Forward{Reason: reason}
	}
	return &Forward{Host: target.Host}
}

// Tracking references a pre-uploaded encrypted mapping object. New signed links
// are indefinite. Expiration fields support previously prepared finite mappings;
// the per-link event budget is snapshotted when the delivery is committed.
type Tracking struct {
	ObjectKey               string
	Indefinite              bool
	ExpiresAt               time.Time
	PostSendValiditySeconds int
	EventLimit              int
	Links                   []tracking.Reference
}
type Repository interface {
	ClaimPreparation(context.Context, string, time.Duration) (Claim, error)
	// RenewPreparation extends the exact, still-live claim by ttl from now. It
	// returns ErrLostLease when the claim expired, was reassigned or is
	// finished; an expired claim is never revived.
	RenewPreparation(context.Context, Claim, time.Duration) error
	CommitPreparation(context.Context, Claim, []Delivery) error
	// FailPreparation reports whether the failure was terminal (not requeued).
	FailPreparation(context.Context, Claim, string) (bool, error)
}
type Objects interface {
	Get(context.Context, string, int64) ([]byte, error)
	PutMIME(context.Context, string, []byte) error
}
type SigningKey struct {
	Domain, Selector string
	// KeyID identifies the stored key behind Signer.
	KeyID  string
	Signer crypto.Signer
}

// Keys must resolve only keys authorized for the authenticated application's
// verified sender domain. Private key material remains in the control plane.
type Keys interface {
	ForSender(context.Context, string, string) (SigningKey, error)
}

// TrackingObjects protects capability mappings separately from signed MIME.
type TrackingObjects interface {
	Put(context.Context, string, []tracking.Mapping) ([]tracking.Reference, error)
}

// TrackingConfig must be supplied explicitly. New links have no expiry; only
// bounded analytics records depend on the protected object and database.
type TrackingConfig struct {
	Origin string
	// CustomDomains lets a claim's TrackingDomain replace Origin in body links
	// and the open pixel. It is true exactly when THINPOST_ADMIN_ORIGIN is set.
	// One-click unsubscribe always uses Origin.
	CustomDomains bool
	Links         *tracking.LinkKeys
	EventLimit    int
	Objects       TrackingObjects
}

// originFor is the origin for the message's tracked links and open pixel: the
// app's own tracking domain when the claim names one and custom domains are
// enabled, otherwise the deployment's origin. One-click unsubscribe never calls
// this: the compliance link stays on the deployment's origin so it does not
// depend on a customer's DNS.
func (c *TrackingConfig) originFor(claim Claim) string {
	if c.CustomDomains && claim.TrackingDomain != "" {
		if origin, err := tracking.NormalizeOrigin("https://" + claim.TrackingDomain); err == nil {
			return origin
		}
	}
	return c.Origin
}

// Production lease settings. The claim lease is short so a crashed worker's
// message is retried quickly; a live worker renews it every ttl/4 (three missed
// renewals still leave the claim valid). MaxPreparation caps total renewal: a
// healthy worst case is 100 recipients of about 8 MiB (roughly 800 MiB), which
// takes about 14 minutes at a sustained 1 MiB/s, so 30 minutes tolerates a
// store at roughly 0.5 MiB/s while still bounding a hung-but-alive worker.
const (
	DefaultLeaseTTL       = 2 * time.Minute
	DefaultMaxPreparation = 30 * time.Minute
)

type Service struct {
	Repository Repository
	Objects    Objects
	Keys       Keys
	Tracking   *TrackingConfig
	// Hostname returns the public host name Thinpost puts in the Received trace
	// field: the HELO name of the main sending IP. Nil, an empty name or a name
	// that is not a plain DNS name adds no trace field.
	Hostname func(context.Context) (string, error)
	// LeaseTTL, RenewInterval and MaxPreparation default to DefaultLeaseTTL,
	// LeaseTTL/4 and DefaultMaxPreparation. They are fields for tests.
	LeaseTTL       time.Duration
	RenewInterval  time.Duration
	MaxPreparation time.Duration
}

// heartbeat renews a preparation claim until stopped, lost or capped.
type heartbeat struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	lost   atomic.Bool
	once   sync.Once
}

// startHeartbeat returns a work context that is canceled when the claim is
// lost. A transient renewal error is retried on the next tick; if no renewal
// has succeeded for a full lease the claim is treated as lost. Past the cap the
// renewals stop and the lease expires on its own; the work is not canceled.
func (s *Service) startHeartbeat(parent context.Context, claim Claim, ttl time.Duration) *heartbeat {
	interval, limit := s.RenewInterval, s.MaxPreparation
	if interval <= 0 {
		interval = ttl / 4
	}
	if limit <= 0 {
		limit = DefaultMaxPreparation
	}
	ctx, cancel := context.WithCancel(parent)
	h := &heartbeat{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	started := time.Now()
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		validSince := started
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if time.Since(started) >= limit {
				return
			}
			attempt := time.Now()
			renewCtx, stop := context.WithTimeout(ctx, ttl/2)
			err := s.Repository.RenewPreparation(renewCtx, claim, ttl)
			stop()
			switch {
			case err == nil:
				validSince = attempt
			case ctx.Err() != nil:
				return
			case errors.Is(err, ErrLostLease) || time.Since(validSince) >= ttl:
				// Either the claim is gone or it has lapsed without renewal.
				h.lost.Store(true)
				oplog.PreparationLeaseLost(claim.AppID, claim.MessageID)
				cancel()
				return
			}
		}
	}()
	return h
}

// stop ends the heartbeat and waits for it; it is safe to call repeatedly.
func (h *heartbeat) stop() {
	h.once.Do(func() {
		h.cancel()
		<-h.done
	})
}

// PrepareNext stores artifacts before committing delivery queue records. A
// crash, expired lease, or failed commit may leave orphaned objects for the
// collector, which must coordinate with in-flight uploads and commits. Failures
// leave the claim to expire; retry
// backoff and terminal failure handling are persisted by the repository.
func (s *Service) PrepareNext(ctx context.Context) (resultErr error) {
	if s.Repository == nil || s.Objects == nil || s.Keys == nil {
		return errors.New("preparation service is not configured")
	}
	token, err := randomID()
	if err != nil {
		return err
	}
	ttl := s.LeaseTTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	claim, err := s.Repository.ClaimPreparation(ctx, token, ttl)
	if err != nil {
		return err
	}
	if claim.Token != token || claim.MessageID == "" || claim.AppID == "" || claim.SourceKey == "" || claim.AcceptedAt.IsZero() {
		return errors.New("invalid preparation claim")
	}
	reason := "source_read"
	// The heartbeat is stopped before any commit or failure write and on every
	// return (its defer runs before the failure defer below).
	hb := s.startHeartbeat(ctx, claim, ttl)
	work := hb.ctx
	defer func() {
		hb.stop()
		if hb.lost.Load() && ctx.Err() == nil {
			// The claim now belongs to someone else (or is expired): write nothing.
			resultErr = ErrLostLease
			return
		}
		if resultErr == nil || errors.Is(resultErr, ErrLostLease) || ctx.Err() != nil {
			return
		}
		// resultErr wraps storage and signing errors; it is returned to the
		// caller, which logs a constant, and is never logged here.
		terminal, failureErr := s.Repository.FailPreparation(ctx, claim, reason)
		if failureErr == nil {
			if terminal {
				oplog.PreparationFailed(claim.AppID, claim.MessageID, reason)
			}
		} else if !errors.Is(failureErr, ErrLostLease) {
			resultErr = errors.Join(resultErr, errors.New("preparation failure could not be recorded"))
		}
	}()
	source, err := s.Objects.Get(work, claim.SourceKey, maxSourceSize)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	reason = "source_invalid"
	payload, err := submission.DecodeSource(source, claim.EnhancedProtection)
	if err != nil {
		return errors.New("invalid source object")
	}
	reason = "signing_key"
	signFrom := payload.Message.From
	if payload.Version == submission.RawSourceVersion {
		// ForSender reads only the domain; a raw source records no address.
		signFrom = "sender@" + payload.RawHeader.SenderDomain
	}
	key, err := s.Keys.ForSender(work, claim.AppID, signFrom)
	if err != nil {
		return fmt.Errorf("resolve signing key: %w", err)
	}
	if claim.SenderDomain == "" || key.Domain != claim.SenderDomain {
		return errors.New("signing domain does not match the recorded sender domain")
	}
	claim.SigningDomain = key.Domain
	claim.SigningKeyID = key.KeyID
	with := receivedtrace.WithHTTP
	if payload.Version == submission.RawSourceVersion {
		with = receivedtrace.WithSMTP // only SMTP submission stores a raw source
	}
	trace, _ := s.trace(work, claim, with)
	if payload.Version == submission.RawSourceVersion {
		if payload.RawHeader.SenderDomain != claim.SenderDomain {
			return errors.New("signing domain does not match the recorded sender domain")
		}
		deliveries, err := s.prepareRaw(work, claim, payload, key, trace, &reason)
		if err != nil {
			return err
		}
		return s.commit(ctx, hb, claim, deliveries, &reason)
	}
	if payload.Message.Tracking != nil {
		reason = "internal"
		if s.Tracking == nil || s.Tracking.Objects == nil || s.Tracking.Links == nil || s.Tracking.EventLimit < 1 || s.Tracking.EventLimit > 1000 {
			return errors.New("tracking preparation is not configured")
		}
	}
	// Thinpost-managed one-click unsubscribe replaces any List-Unsubscribe
	// headers the source carries. A flag with no tracking runtime fails visibly
	// (a hold) rather than silently dropping the header.
	oneClick := claim.ListUnsubscribe
	if oneClick {
		reason = "internal"
		if err = s.checkOneClick(); err != nil {
			return err
		}
	}
	// The sender's own link, to call when the recipient unsubscribes. Decided
	// once from the stored value; only meaningful when it is being replaced.
	var forward *Forward
	if oneClick {
		forward = forwardFor(unsubforward.OriginalOf(payload))
	}
	trackingOrigin := ""
	if s.Tracking != nil {
		trackingOrigin = s.Tracking.originFor(claim)
	}
	deliveries := make([]Delivery, 0, len(payload.Message.Envelope()))
	for index, recipient := range payload.Message.Envelope() {
		reason = "internal"
		id, err := randomID()
		if err != nil {
			return err
		}
		preparedMessage := payload.Message
		if oneClick {
			link, linkErr := s.Tracking.Links.UnsubscribeLink(s.Tracking.Origin, id)
			if linkErr != nil {
				return errors.New("unsubscribe link could not be signed")
			}
			preparedMessage.ListUnsubscribe, preparedMessage.ListUnsubscribePost = "<"+link+">", true
		}
		var preparedTracking *Tracking
		if payload.Message.Tracking != nil {
			reason = "mime_build"
			html, mappings, rewriteErr := tracking.Rewrite(payload.Message.HTML, tracking.Options{Origin: trackingOrigin, Links: s.Tracking.Links, Opens: payload.Message.Tracking.Opens, Clicks: payload.Message.Tracking.Clicks})
			if rewriteErr != nil {
				return errors.New("tracking HTML preparation failed")
			}
			if len(mappings) > 0 {
				preparedMessage.HTML = html
				mappingKey, keyErr := tracking.NewMappingKey(id, time.Now())
				if keyErr != nil {
					return errors.New("tracking object key could not be created")
				}
				reason = "object_write"
				refs, putErr := s.Tracking.Objects.Put(work, mappingKey, mappings)
				if putErr != nil {
					return errors.New("tracking object could not be stored")
				}
				preparedTracking = &Tracking{ObjectKey: mappingKey, Indefinite: true, EventLimit: s.Tracking.EventLimit, Links: refs}
			}
		}
		reason = "mime_build"
		options := message.Options{Recipient: recipient, ID: claim.MessageID, Date: claim.date(), SigningDomain: key.Domain, Selector: key.Selector, Signer: key.Signer, ListRecipients: payload.ListsRecipients(), Received: trace}
		final, err := message.Build(preparedMessage, options)
		if err != nil {
			// message.Build errors are constant; never wrap anything that could quote input.
			return errors.New("build signed MIME failed")
		}
		if oneClick && len(final) > edgeprotocol.MaxMIMEBytes {
			// A message accepted before one-click was sized in (or whose app
			// turned it on since) can be too big to carry Thinpost's headers.
			// Send it without them rather than fail it: the app's own headers,
			// if any, stay, and nothing is forwarded.
			oneClick, forward = false, nil
			oplog.OneClickSkipped(claim.AppID, claim.MessageID)
			preparedMessage.ListUnsubscribe, preparedMessage.ListUnsubscribePost = payload.Message.ListUnsubscribe, payload.Message.ListUnsubscribePost
			if final, err = message.Build(preparedMessage, options); err != nil {
				return errors.New("build signed MIME failed")
			}
		}
		// Do not commit a delivery that the Edge protocol must reject. This is
		// also a guard for older stored submissions and future transformers.
		if len(final) > edgeprotocol.MaxMIMEBytes {
			return errors.New("signed MIME exceeds delivery limit")
		}
		objectKey, err := objectkey.New("final", id, time.Now())
		if err != nil {
			return err
		}
		reason = "object_write"
		if err = s.Objects.PutMIME(work, objectKey, final); err != nil {
			return fmt.Errorf("store signed MIME: %w", err)
		}
		deliveries = append(deliveries, Delivery{ID: id, RecipientIndex: index, ObjectKey: objectKey, Tracking: preparedTracking, Forward: forward})
	}
	return s.commit(ctx, hb, claim, deliveries, &reason)
}

// checkOneClick fails when the deployment cannot sign Thinpost's one-click link.
func (s *Service) checkOneClick() error {
	if s.Tracking == nil || s.Tracking.Links == nil || s.Tracking.Origin == "" {
		return errors.New("one-click unsubscribe is not configured")
	}
	if tracking.ValidateUnsubscribeOrigin(s.Tracking.Origin) != nil {
		return errors.New("one-click unsubscribe origin is invalid")
	}
	return nil
}

// commit stops the heartbeat and queues the prepared deliveries.
func (s *Service) commit(ctx context.Context, hb *heartbeat, claim Claim, deliveries []Delivery, reason *string) error {
	*reason = "queue_commit"
	hb.stop()
	if hb.lost.Load() {
		return ErrLostLease
	}
	if err := s.Repository.CommitPreparation(ctx, claim, deliveries); err != nil {
		return err
	}
	oplog.Message(claim.AppID, claim.MessageID, oplog.StatusPrepared)
	return nil
}

// prepareRaw prepares a version 3 (raw MIME) source. Tracking never applies to a
// raw message yet. Without one-click unsubscribe the message is prepared and
// signed once and every recipient's delivery points at an identical object,
// because the recipients are in the SMTP envelope only. With it, each delivery
// gets its own List-Unsubscribe link (replacing any the client wrote), so each
// is prepared, signed and stored separately.
func (s *Service) prepareRaw(ctx context.Context, claim Claim, payload submission.Source, key SigningKey, trace []byte, reason *string) ([]Delivery, error) {
	h := payload.RawHeader
	*reason = "source_invalid"
	scanned, err := rawmime.Scan(payload.Raw)
	if err != nil {
		return nil, errors.New("invalid source object")
	}
	if h.FromField == "" && (scanned.From == nil || addressDomain(scanned.From.Address) != h.SenderDomain) {
		return nil, errors.New("invalid source object")
	}
	oneClick := claim.ListUnsubscribe
	if oneClick {
		*reason = "internal"
		if err = s.checkOneClick(); err != nil {
			return nil, err
		}
	}
	input := rawmime.PrepareInput{
		Scan: scanned,
		From: rawmime.FromDecision{
			Value: h.From, Field: h.FromField, Domain: h.SenderDomain, Rewritten: h.FromRewritten,
			Original: h.OriginalFrom, StripSender: h.StripSender,
		},
		MessageID: claim.MessageID,
		Date:      claim.date(),
		Selector:  key.Selector,
		Signer:    key.Signer,
		Received:  trace,
	}
	var shared []byte
	var forward *Forward
	if oneClick {
		forward = forwardFor(unsubforward.OriginalOf(payload))
	} else {
		*reason = "mime_build"
		if shared, err = rawmime.Prepare(payload.Raw, input); err != nil {
			// rawmime errors are constant, but never wrap anything built from input.
			return nil, errors.New("build signed MIME failed")
		}
		if len(shared) > edgeprotocol.MaxMIMEBytes {
			return nil, errors.New("signed MIME exceeds delivery limit")
		}
	}
	deliveries := make([]Delivery, 0, len(h.Recipients))
	for index := range h.Recipients {
		*reason = "internal"
		id, err := randomID()
		if err != nil {
			return nil, err
		}
		final := shared
		if oneClick {
			link, linkErr := s.Tracking.Links.UnsubscribeLink(s.Tracking.Origin, id)
			if linkErr != nil {
				return nil, errors.New("unsubscribe link could not be signed")
			}
			value := "<" + link + ">"
			if submission.ValidateListUnsubscribe(value, true) != nil {
				return nil, errors.New("unsubscribe link is not a valid header value")
			}
			*reason = "mime_build"
			perDelivery := input
			perDelivery.ListUnsubscribe = value
			if final, err = rawmime.Prepare(payload.Raw, perDelivery); err != nil {
				return nil, errors.New("build signed MIME failed")
			}
			if len(final) > edgeprotocol.MaxMIMEBytes {
				// Too big to carry Thinpost's headers (a source accepted before
				// they were sized in): send it as the client wrote it, with the
				// client's own headers and no forward, rather than fail it.
				oneClick, forward = false, nil
				oplog.OneClickSkipped(claim.AppID, claim.MessageID)
				if shared, err = rawmime.Prepare(payload.Raw, input); err != nil {
					return nil, errors.New("build signed MIME failed")
				}
				if len(shared) > edgeprotocol.MaxMIMEBytes {
					return nil, errors.New("signed MIME exceeds delivery limit")
				}
				final = shared
			}
		}
		objectKey, err := objectkey.New("final", id, time.Now())
		if err != nil {
			return nil, err
		}
		*reason = "object_write"
		if err = s.Objects.PutMIME(ctx, objectKey, final); err != nil {
			return nil, fmt.Errorf("store signed MIME: %w", err)
		}
		deliveries = append(deliveries, Delivery{ID: id, RecipientIndex: index, ObjectKey: objectKey, Forward: forward})
	}
	return deliveries, nil
}

func addressDomain(address string) string {
	return strings.ToLower(address[strings.LastIndexByte(address, '@')+1:])
}
func randomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// trace renders the message's Received field from stored facts only: the
// message ID, the acceptance time and the deployment's public host name. It
// never reads or records anything about the client. It returns nil when no
// usable host name is known (or the lookup fails), and the message goes without one.
func (s *Service) trace(ctx context.Context, claim Claim, with string) ([]byte, error) {
	if s.Hostname == nil {
		return nil, nil
	}
	host, err := s.Hostname(ctx)
	if err != nil {
		// A cosmetic header never fails preparation. Log a constant only.
		log.Print("received trace host name unavailable; preparing without a Received header")
		return nil, nil
	}
	field, err := receivedtrace.Field(host, with, claim.MessageID, claim.AcceptedAt)
	if err != nil {
		return nil, nil
	}
	return field, nil
}
