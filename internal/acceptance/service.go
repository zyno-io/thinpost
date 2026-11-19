// Package acceptance coordinates object storage and transactional acceptance.
// It is not wired into a runnable server yet.
package acceptance

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/submission"
)

// Objects must store the bytes durably before returning nil. Objects may be
// unreferenced if the following database transaction fails or deduplicates;
// a collector must coordinate with in-flight uploads and commits before removing
// them. An age threshold and reference check alone cannot exclude a delayed commit.
type Objects interface {
	Put(ctx context.Context, key string, data []byte) error
}

type Record struct {
	AppID string
	// KeyID or Operator identifies the principal; exactly one is set. Operator
	// is the administrator subject of a console submission.
	KeyID              string
	Operator           string
	ObjectKey          string
	RequestHash        [32]byte
	PreviousHashes     [][32]byte
	IdempotencyKey     string
	Adapter            string
	SendingDomain      string
	From               string
	EnhancedProtection bool
	// Hold commits the message (every item of a batch) on hold: stored and
	// counted, but not prepared until the app releases it.
	Hold           bool
	RecipientCount int
	// Subject, Recipients and SenderDomain are operator-visible metadata
	// recorded in the submission transaction. Recipients holds the bare
	// envelope addresses in submission order (To, then Cc, then Bcc;
	// RecipientCount entries) and RecipientKinds the kind of each;
	// SenderDomain is the lowercased domain of From. Never log or wrap them.
	Subject        string
	Recipients     []string
	RecipientKinds []string
	SenderDomain   string
	// SenderAddress and SenderName are the bare From address and decoded
	// display name (empty when none). Never log or wrap them.
	SenderAddress string
	SenderName    string
}

// Repository owns app and sender authorization and transactional idempotency.
// Commit must recheck application and sender/domain authorization, atomically
// persist the reference and queue record, and return only after commit. A
// duplicate key is scoped by app and adapter: identical request hashes return
// the original receipt; different hashes return ErrIdempotencyConflict.
// For Resend, keys must be retained for the documented 24-hour window.
type Repository interface {
	LookupKey(ctx context.Context, digest [32]byte) (submission.App, error)
	// ReserveSubmission charges the app before uploads; committed reservations
	// are not refunded on upload failures, retries or subsequent rejection.
	ReserveSubmission(ctx context.Context, appID, keyID string, messages int) error
	Commit(ctx context.Context, record Record) (submission.Receipt, error)
}

// SuppressionChecker is an optional Repository capability for the Postmark
// all-suppressed rejection. It returns how many of the distinct lowercase
// addresses are on the app's list.
type SuppressionChecker interface {
	SuppressedRecipients(ctx context.Context, appID string, lowers []string) (int, error)
}

// OperatorRepository is an optional Repository capability for operator
// submissions (see submission.OperatorService). LookupOperatorApp reads the
// app as LookupKey does, without a key, returning submission.ErrAppPaused for
// an inactive app. ReserveOperatorSubmission is ReserveSubmission without the
// key check. Commit must accept a record with Operator set and no KeyID.
type OperatorRepository interface {
	LookupOperatorApp(ctx context.Context, appID string) (submission.App, error)
	ReserveOperatorSubmission(ctx context.Context, appID string, messages int) error
}

type Service struct {
	repository      Repository
	objects         Objects
	fingerprintKeys [][]byte
	recipients      RecipientPolicy
	// ApplyTrackingDefaults enables the apps' tracking defaults. Set it exactly
	// when the tracking runtime is configured (the same condition that lets
	// preparation rewrite links); when false, defaults are ignored, so a missed
	// wiring leaves them inert instead of failing every HTML message.
	ApplyTrackingDefaults bool
}

type RecipientPolicy interface{ Allows(string) bool }

// New requires a secret shared by all replicas and retained throughout the
// idempotency window. This reduces message-guessing and cross-app correlation
// if database fingerprints are exposed. Coordinate rotation across replicas.
func New(repository Repository, objects Objects, fingerprintKey []byte, recipients RecipientPolicy) (*Service, error) {
	return NewWithFingerprintKeys(repository, objects, [][]byte{fingerprintKey}, recipients)
}

// NewWithFingerprintKeys uses the first key for new records and all supplied
// keys to recognize retries during a coordinated rotation. Deploy every key to
// all replicas before changing the active key, and retain old keys until no
// unexpired records can refer to them.
func NewWithFingerprintKeys(repository Repository, objects Objects, keys [][]byte, recipients RecipientPolicy) (*Service, error) {
	if len(keys) == 0 || len(keys) > 8 || recipients == nil {
		return nil, fmt.Errorf("fingerprint key and recipient policy are required")
	}
	copied := make([][]byte, 0, len(keys))
	for _, key := range keys {
		if len(key) < 32 || len(key) > 1024 {
			return nil, fmt.Errorf("invalid fingerprint key")
		}
		for _, other := range copied {
			if hmac.Equal(key, other) {
				return nil, fmt.Errorf("duplicate fingerprint key")
			}
		}
		copied = append(copied, append([]byte(nil), key...))
	}
	return &Service{repository: repository, objects: objects, fingerprintKeys: copied, recipients: recipients}, nil
}

// MatchesRequestHash compares an existing fingerprint against every key held
// by this replica without revealing which key matched.
func (r Record) MatchesRequestHash(existing []byte) bool {
	matched := hmac.Equal(existing, r.RequestHash[:])
	for _, hash := range r.PreviousHashes {
		matched = hmac.Equal(existing, hash[:]) || matched
	}
	return matched
}

func (s *Service) Authenticate(ctx context.Context, key string) (submission.App, error) {
	if s.repository == nil || s.objects == nil {
		return submission.App{}, submission.ErrUnavailable
	}
	if key == "" {
		return submission.App{}, submission.ErrInvalidKey
	}
	digest := sha256.Sum256([]byte(key))
	return s.repository.LookupKey(ctx, digest)
}

// maxOperatorBytes bounds an operator subject, like other administrator text.
const maxOperatorBytes = 256

// AuthorizeOperator returns the app as an operator principal for the
// administrator subject actor. The caller must have authenticated actor as an
// administrator; the subject is recorded in the audit event, never trusted
// from a request body. Every acceptance policy still applies in Accept.
func (s *Service) AuthorizeOperator(ctx context.Context, appID, actor string) (submission.App, error) {
	if s.repository == nil || s.objects == nil {
		return submission.App{}, submission.ErrUnavailable
	}
	repository, ok := s.repository.(OperatorRepository)
	if !ok {
		return submission.App{}, submission.ErrUnavailable
	}
	if appID == "" || !validOperator(actor) {
		return submission.App{}, submission.ErrInvalidRequest
	}
	app, err := repository.LookupOperatorApp(ctx, appID)
	if err != nil {
		return submission.App{}, err
	}
	if app.ID != appID {
		return submission.App{}, submission.ErrUnavailable
	}
	app.KeyID = ""
	app.Operator = actor
	return app, nil
}

func validOperator(actor string) bool {
	if actor == "" || len(actor) > maxOperatorBytes || !utf8.ValidString(actor) || strings.TrimSpace(actor) != actor {
		return false
	}
	for _, c := range actor {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

// maxSourceBytes mirrors the object store's plaintext limit (16 MiB).
const maxSourceBytes = 16 << 20

// fingerprintSlack covers what the fingerprint adds to, or the resolved source
// adds beyond, the other encoding: the app ID, adapter and sending domain on one
// side, the version wrapper and resolved tracking flags on the other. Well
// under 1 KiB in practice.
const fingerprintSlack = 4 << 10

func (s *Service) Accept(ctx context.Context, app submission.App, req submission.Request) (submission.Receipt, error) {
	if s.repository == nil || s.objects == nil {
		return submission.Receipt{}, submission.ErrUnavailable
	}
	if app.ID == "" || req.Adapter == "" || req.Message.Validate() != nil {
		return submission.Receipt{}, submission.ErrInvalidMessage
	}
	// The operator principal and the console adapter are bound to each other:
	// an operator never carries a key or an idempotency key, and no key
	// submission may claim the console adapter.
	operator := app.Operator != ""
	if operator && (app.KeyID != "" || !validOperator(app.Operator) || req.Adapter != submission.AdapterConsole || req.IdempotencyKey != "") || !operator && req.Adapter == submission.AdapterConsole {
		return submission.Receipt{}, submission.ErrInvalidMessage
	}
	var operators OperatorRepository
	if operator {
		var ok bool
		if operators, ok = s.repository.(OperatorRepository); !ok {
			return submission.Receipt{}, submission.ErrUnavailable
		}
	}
	// An app with Thinpost-managed one-click unsubscribe cannot also supply
	// its own List-Unsubscribe headers: the two would conflict.
	if app.ListUnsubscribe && (req.Message.ListUnsubscribe != "" || req.Message.ListUnsubscribePost) {
		return submission.Receipt{}, submission.ErrInvalidMessage
	}
	for _, recipient := range req.Message.Envelope() {
		if !s.recipients.Allows(recipient) {
			return submission.Receipt{}, submission.ErrInvalidMessage
		}
	}
	// Hold is omitted when false so fingerprints of requests without it are
	// byte-identical to those recorded before holds existed.
	fingerprint, err := json.Marshal(struct {
		AppID         string             `json:"app_id"`
		Adapter       string             `json:"adapter"`
		Message       submission.Message `json:"message"`
		SendingDomain string             `json:"sending_domain"`
		Hold          bool               `json:"hold,omitempty"`
	}{app.ID, req.Adapter, req.Message, req.SendingDomain, req.Hold})
	if err != nil {
		return submission.Receipt{}, fmt.Errorf("fingerprint submission: %w", err)
	}
	if operator {
		if err := operators.ReserveOperatorSubmission(ctx, app.ID, 1); err != nil {
			return submission.Receipt{}, err
		}
	} else if err := s.repository.ReserveSubmission(ctx, app.ID, app.KeyID, 1); err != nil {
		return submission.Receipt{}, err
	}
	// The fingerprint is the request as sent, so it is at least as large as the
	// source built below, less a few hundred bytes. Past the object limit the
	// source cannot fit: reject before preflight renders MIME and before a second
	// full-size encode. After the reservation, like the guard below.
	if len(fingerprint) > maxSourceBytes+fingerprintSlack {
		return submission.Receipt{}, submission.ErrInvalidMessage
	}
	// Tracking defaults resolve here, once; the fingerprint above covers the
	// request as sent, so a retry after a default changes still replays.
	resolved, err := s.resolve(req.Message, app)
	if err != nil {
		return submission.Receipt{}, err
	}
	// Encode a versioned source object, not final MIME. Signing and delivery
	// must create and persist final MIME separately before any SMTP attempt.
	body, err := submission.EncodeSource(resolved)
	if err != nil {
		return submission.Receipt{}, fmt.Errorf("encode submission: %w", err)
	}
	// The object store refuses larger plaintext. JSON escaping can expand a
	// body, so reject here instead of failing the upload as unavailable.
	if len(body) > maxSourceBytes {
		return submission.Receipt{}, submission.ErrInvalidMessage
	}
	// After the size preflight, like it: both are rejections of otherwise
	// valid messages that must follow the rate reservation.
	metadata, err := newRecordedMetadata(req.Message)
	if err != nil {
		return submission.Receipt{}, err
	}
	if req.RejectAllSuppressed {
		if checker, ok := s.repository.(SuppressionChecker); ok {
			// After the rate reservation and before the upload. Partially
			// suppressed requests are accepted; the claim drops those recipients.
			seen := make(map[string]struct{}, len(metadata.Recipients))
			lowers := make([]string, 0, len(metadata.Recipients))
			for _, recipient := range metadata.Recipients {
				lower := strings.ToLower(recipient)
				if _, dup := seen[lower]; !dup {
					seen[lower] = struct{}{}
					lowers = append(lowers, lower)
				}
			}
			suppressed, err := checker.SuppressedRecipients(ctx, app.ID, lowers)
			if err != nil {
				return submission.Receipt{}, err
			}
			if len(lowers) > 0 && suppressed >= len(lowers) {
				return submission.Receipt{}, submission.ErrAllRecipientsSuppressed
			}
		}
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return submission.Receipt{}, fmt.Errorf("generate object ID: %w", err)
	}
	objectKey, err := objectkey.New("submissions", hex.EncodeToString(id), time.Now())
	if err != nil {
		return submission.Receipt{}, err
	}
	if err := s.objects.Put(ctx, objectKey, body); err != nil {
		return submission.Receipt{}, fmt.Errorf("store submission: %w", err)
	}
	hashes := make([][32]byte, len(s.fingerprintKeys))
	for i, key := range s.fingerprintKeys {
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write(fingerprint)
		copy(hashes[i][:], mac.Sum(nil))
	}
	receipt, err := s.repository.Commit(ctx, Record{AppID: app.ID, KeyID: app.KeyID, Operator: app.Operator, ObjectKey: objectKey, RequestHash: hashes[0], PreviousHashes: hashes[1:], IdempotencyKey: req.IdempotencyKey, Adapter: req.Adapter, SendingDomain: req.SendingDomain, From: req.Message.From, EnhancedProtection: req.Message.EnhancedProtection, Hold: req.Hold, RecipientCount: len(req.Message.Envelope()), Subject: metadata.Subject, Recipients: metadata.Recipients, RecipientKinds: metadata.RecipientKinds, SenderDomain: metadata.SenderDomain, SenderAddress: metadata.SenderAddress, SenderName: metadata.SenderName})
	if err == nil && !receipt.Replayed {
		oplog.Message(app.ID, receipt.ID, oplog.StatusAccepted)
		if req.Hold {
			oplog.Message(app.ID, receipt.ID, oplog.StatusHeld)
		}
	}
	return receipt, err
}
