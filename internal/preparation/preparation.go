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
	"sync"
	"sync/atomic"
	"time"

	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/message"
	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/tracking"
)

var ErrNoWork = errors.New("no submission to prepare")
var ErrLostLease = errors.New("preparation lease is no longer valid")

const maxSourceSize = 16 << 20

type Claim struct {
	MessageID, AppID, SourceKey, Token string
	SigningDomain                      string
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
	Signer           crypto.Signer
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
	Origin     string
	Links      *tracking.LinkKeys
	EventLimit int
	Objects    TrackingObjects
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
	key, err := s.Keys.ForSender(work, claim.AppID, payload.Message.From)
	if err != nil {
		return fmt.Errorf("resolve signing key: %w", err)
	}
	if claim.SenderDomain == "" || key.Domain != claim.SenderDomain {
		return errors.New("signing domain does not match the recorded sender domain")
	}
	claim.SigningDomain = key.Domain
	if payload.Message.Tracking != nil {
		reason = "internal"
		if s.Tracking == nil || s.Tracking.Objects == nil || s.Tracking.Links == nil || s.Tracking.EventLimit < 1 || s.Tracking.EventLimit > 1000 {
			return errors.New("tracking preparation is not configured")
		}
	}
	// Thinpost-managed one-click unsubscribe applies only when the source does
	// not carry the app's own headers. A flag with no tracking runtime fails
	// visibly (a hold) rather than silently dropping the header.
	oneClick := claim.ListUnsubscribe && payload.Message.ListUnsubscribe == "" && !payload.Message.ListUnsubscribePost
	if oneClick {
		reason = "internal"
		if s.Tracking == nil || s.Tracking.Links == nil || s.Tracking.Origin == "" {
			return errors.New("one-click unsubscribe is not configured")
		}
		if tracking.ValidateUnsubscribeOrigin(s.Tracking.Origin) != nil {
			return errors.New("one-click unsubscribe origin is invalid")
		}
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
			html, mappings, rewriteErr := tracking.Rewrite(payload.Message.HTML, tracking.Options{Origin: s.Tracking.Origin, Links: s.Tracking.Links, Opens: payload.Message.Tracking.Opens, Clicks: payload.Message.Tracking.Clicks})
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
		final, err := message.Build(preparedMessage, message.Options{Recipient: recipient, ID: claim.MessageID, Date: claim.date(), SigningDomain: key.Domain, Selector: key.Selector, Signer: key.Signer, ListRecipients: payload.ListsRecipients()})
		if err != nil {
			// message.Build errors are constant; never wrap anything that could quote input.
			return errors.New("build signed MIME failed")
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
		deliveries = append(deliveries, Delivery{ID: id, RecipientIndex: index, ObjectKey: objectKey, Tracking: preparedTracking})
	}
	reason = "queue_commit"
	hb.stop()
	if hb.lost.Load() {
		return ErrLostLease
	}
	if err = s.Repository.CommitPreparation(ctx, claim, deliveries); err != nil {
		return err
	}
	oplog.Message(claim.AppID, claim.MessageID, oplog.StatusPrepared)
	return nil
}
func randomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}
