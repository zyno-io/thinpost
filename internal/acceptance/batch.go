package acceptance

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/submission"
)

const maxBatchMessages = 100
const maxBatchSourceBytes = 2 << 20

type BatchItem struct {
	ObjectKey          string
	From               string
	EnhancedProtection bool
	RecipientCount     int
	Subject            string
	Recipients         []string
	RecipientKinds     []string
	SenderDomain       string
	SenderAddress      string
	SenderName         string
}

// BatchRecord carries bounded object references, a keyed fingerprint and per
// item operator metadata (subject, envelope recipients, sender domain).
// Message bodies and attachments remain in encrypted object storage.
type BatchRecord struct {
	Record
	Items []BatchItem
}

// BatchRepository commits the entire ordered batch and its optional retry
// marker in one transaction. Errors must leave no visible queue records.
type BatchRepository interface {
	CommitBatch(context.Context, BatchRecord) (submission.BatchReceipt, error)
}

func (s *Service) AcceptBatch(ctx context.Context, app submission.App, req submission.BatchRequest) (submission.BatchReceipt, error) {
	repository, ok := s.repository.(BatchRepository)
	if !ok || s.objects == nil {
		return submission.BatchReceipt{}, submission.ErrUnavailable
	}
	if app.ID == "" || len(req.Messages) == 0 || len(req.Messages) > maxBatchMessages || (req.Adapter != "resend" && req.Adapter != "sendgrid") || (req.Adapter == "sendgrid" && req.IdempotencyKey != "") || len(req.IdempotencyKey) > 256 || !utf8.ValidString(req.IdempotencyKey) || strings.ContainsRune(req.IdempotencyKey, 0) {
		return submission.BatchReceipt{}, submission.ErrInvalidMessage
	}
	// Validate every item before any upload. The repository repeats all
	// authorization and quota checks under the app lock before publication.
	bodies := make([][]byte, len(req.Messages))
	total := 0
	recipients := 0
	for i, message := range req.Messages {
		if message.Validate() != nil || (req.Adapter == "resend" && len(message.Envelope()) > 50) {
			return submission.BatchReceipt{}, submission.ErrInvalidMessage
		}
		if app.ListUnsubscribe && (message.ListUnsubscribe != "" || message.ListUnsubscribePost) {
			return submission.BatchReceipt{}, submission.ErrInvalidMessage
		}
		recipients += len(message.Envelope())
		if req.Adapter == "sendgrid" && recipients > 1000 {
			return submission.BatchReceipt{}, submission.ErrInvalidMessage
		}
		for _, recipient := range message.Envelope() {
			if !s.recipients.Allows(recipient) {
				return submission.BatchReceipt{}, submission.ErrInvalidMessage
			}
		}
		body, err := submission.EncodeSource(message)
		if err != nil {
			return submission.BatchReceipt{}, errors.New("cannot encode batch submission")
		}
		// The cap is measured on the request as sent. Tracking defaults add at
		// most about 40 bytes per item after resolution; that is deliberately
		// not re-checked, so a default can never reject a batch.
		total += len(body)
		if total > maxBatchSourceBytes {
			return submission.BatchReceipt{}, submission.ErrInvalidMessage
		}
		bodies[i] = body
	}
	// Hold is omitted when false so earlier fingerprints are byte-identical.
	fingerprint, err := json.Marshal(struct {
		AppID    string               `json:"app_id"`
		Adapter  string               `json:"adapter"`
		Messages []submission.Message `json:"messages"`
		Hold     bool                 `json:"hold,omitempty"`
	}{app.ID, req.Adapter, req.Messages, req.Hold})
	if err != nil {
		return submission.BatchReceipt{}, errors.New("cannot fingerprint batch submission")
	}
	if err := s.repository.ReserveSubmission(ctx, app.ID, app.KeyID, len(req.Messages)); err != nil {
		return submission.BatchReceipt{}, err
	}
	metadata := make([]recordedMetadata, len(req.Messages))
	for i, message := range req.Messages {
		resolved, err := s.resolve(message, app)
		if err != nil {
			return submission.BatchReceipt{}, err
		}
		// Store the resolved source (effective Tracking, never TrackingOff).
		body, err := submission.EncodeSource(resolved)
		if err != nil {
			return submission.BatchReceipt{}, errors.New("cannot encode batch submission")
		}
		bodies[i] = body
		if metadata[i], err = newRecordedMetadata(message); err != nil {
			return submission.BatchReceipt{}, err
		}
	}
	hashes := make([][32]byte, len(s.fingerprintKeys))
	for i, key := range s.fingerprintKeys {
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write(fingerprint)
		copy(hashes[i][:], mac.Sum(nil))
	}
	record := BatchRecord{Record: Record{AppID: app.ID, KeyID: app.KeyID, Adapter: req.Adapter, IdempotencyKey: req.IdempotencyKey, RequestHash: hashes[0], PreviousHashes: hashes[1:], Hold: req.Hold}, Items: make([]BatchItem, 0, len(req.Messages))}
	for i, message := range req.Messages {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return submission.BatchReceipt{}, errors.New("cannot generate batch object ID")
		}
		key, err := objectkey.New("submissions", hex.EncodeToString(id[:]), time.Now())
		if err != nil {
			return submission.BatchReceipt{}, err
		}
		if err = s.objects.Put(ctx, key, bodies[i]); err != nil {
			return submission.BatchReceipt{}, errors.New("cannot store batch submission")
		}
		record.Items = append(record.Items, BatchItem{ObjectKey: key, From: message.From, EnhancedProtection: message.EnhancedProtection, RecipientCount: len(message.Envelope()), Subject: metadata[i].Subject, Recipients: metadata[i].Recipients, RecipientKinds: metadata[i].RecipientKinds, SenderDomain: metadata[i].SenderDomain, SenderAddress: metadata[i].SenderAddress, SenderName: metadata[i].SenderName})
	}
	receipt, err := repository.CommitBatch(ctx, record)
	if err == nil && !receipt.Replayed {
		for _, id := range receipt.IDs {
			oplog.Message(app.ID, id, oplog.StatusAccepted)
			if req.Hold {
				oplog.Message(app.ID, id, oplog.StatusHeld)
			}
		}
	}
	return receipt, err
}
