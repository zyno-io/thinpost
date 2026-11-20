package acceptance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/message"
	"github.com/zyno-io/thinpost/internal/objectkey"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/submission"
)

// ReplayKind says which replay identity a raw message has (plan section 4.5).
type ReplayKind uint8

const (
	// ReplayNone: no Message-ID and no Date. Nothing is deduplicated.
	ReplayNone ReplayKind = iota
	// ReplayMessageID (kind M): a valid Message-ID. Key "smtp1:", 24 hours.
	ReplayMessageID
	// ReplayContent (kind C): no valid Message-ID but a Date. Key "smtp-c1:",
	// 10 minutes.
	ReplayContent
)

// Replay key prefixes and windows. The two kinds never share a key.
const (
	ReplayMessageIDPrefix = "smtp1:"
	ReplayContentPrefix   = "smtp-c1:"
	ReplayMessageIDWindow = 24 * time.Hour
	ReplayContentWindow   = 10 * time.Minute
)

// RawRequest is one SMTP submission that smtpintake has already scanned and
// decided. AcceptRaw re-checks nothing about the From policy: From is the
// decision rawmime.DecideFrom made for these exact bytes.
type RawRequest struct {
	Raw      []byte
	Scan     *rawmime.Scanned
	MailFrom string
	// Recipients are the de-duplicated envelope recipients and Kinds the kind
	// of each ("to", "cc" or "bcc"), ordered to, then cc, then bcc.
	Recipients, Kinds []string
	// From.Value is the unfolded single-line From; From.Field is the folded
	// wire field, set only when the From is rewritten or inserted.
	From               rawmime.FromDecision
	EnhancedProtection bool
	// Hold stores the message on hold (X-Thinpost-Hold). The replay digests
	// cover the raw bytes, hold header included, so a replay has the same choice.
	Hold         bool
	Replay       ReplayKind
	ReplayDigest [32]byte // meaningful only when Replay != ReplayNone
}

// SMTPRepository is an optional Repository capability for SMTP submissions,
// asserted like SuppressionChecker. ReserveSMTPSubmission is
// ReserveSubmission for an SMTP credential: it rechecks the credential's
// digest and returns submission.ErrAppPaused for a paused app.
type SMTPRepository interface {
	ReserveSMTPSubmission(ctx context.Context, appID, credentialID string, digest [32]byte) error
}

// sizingMessageID and sizingSelector are the longest values preparation can
// use, so the measured size bounds the real one.
var (
	sizingMessageID = strings.Repeat("a", 32)
	sizingSelector  = strings.Repeat("a", 63)
)

// AcceptRaw stores a raw MIME message received over SMTP and queues it. The
// order follows Accept: authorization, policy, rate reservation, sizing,
// upload, commit. The principal must be an SMTP credential; the commit
// rechecks it, the app, the sender domain and the session fence.
func (s *Service) AcceptRaw(ctx context.Context, app submission.App, fence submission.Fence, req RawRequest) (submission.Receipt, error) {
	if s.repository == nil || s.objects == nil {
		return submission.Receipt{}, submission.ErrUnavailable
	}
	// Step 1: the principal is an SMTP credential and nothing else.
	if app.ID == "" || app.SMTPCredentialID == "" || app.KeyID != "" || app.Operator != "" {
		return submission.Receipt{}, submission.ErrInvalidMessage
	}
	repository, ok := s.repository.(SMTPRepository)
	if !ok {
		return submission.Receipt{}, submission.ErrUnavailable
	}
	if len(req.Raw) > rawmime.MaxRawBytes {
		return submission.Receipt{}, submission.ErrMessageTooLarge
	}
	if req.Scan == nil || len(req.Raw) == 0 || req.Scan.Size != len(req.Raw) || len(req.Recipients) < 1 || len(req.Recipients) > submission.MaxEnvelopeRecipients || len(req.Kinds) != len(req.Recipients) ||
		req.From.Domain == "" || req.Replay > ReplayContent || req.Replay != ReplayNone && req.ReplayDigest == [32]byte{} {
		return submission.Receipt{}, submission.ErrInvalidMessage
	}
	// Step 2: the recipient allowlist is enforced here, as for HTTP, and
	// refuses the whole message.
	for _, recipient := range req.Recipients {
		if !s.recipients.Allows(recipient) {
			return submission.Receipt{}, submission.ErrRecipientNotPermitted
		}
	}
	// Step 3: charge the bucket before any upload.
	if err := repository.ReserveSMTPSubmission(ctx, app.ID, app.SMTPCredentialID, app.SMTPCredentialDigest); err != nil {
		return submission.Receipt{}, err
	}
	// Step 4: the exact prepared size, with the largest signature and
	// identifiers preparation can produce.
	signer, err := rawmime.SizingSigner(message.MaxDKIMRSABits)
	if err != nil {
		return submission.Receipt{}, err
	}
	size, err := rawmime.PreparedSize(req.Raw, rawmime.PrepareInput{
		Scan: req.Scan, From: req.From, MessageID: sizingMessageID, Date: time.Unix(0, 0).UTC(), Selector: sizingSelector, Signer: signer,
	})
	if err != nil {
		if errors.Is(err, rawmime.ErrTooLarge) {
			return submission.Receipt{}, submission.ErrMessageTooLarge
		}
		return submission.Receipt{}, submission.ErrInvalidMessage
	}
	if !size.Fits() {
		return submission.Receipt{}, submission.ErrMessageTooLarge
	}
	// Step 5: the version 3 source. from and from_field are set only when the
	// From is rewritten or inserted; original_from only with them.
	header := submission.RawHeader{
		MailFrom: req.MailFrom, Recipients: req.Recipients, Kinds: req.Kinds,
		StripSender: req.From.StripSender, SenderDomain: req.From.Domain, EnhancedProtection: req.EnhancedProtection,
	}
	if req.From.Rewritten {
		header.From, header.FromField, header.FromRewritten, header.OriginalFrom = req.From.Value, req.From.Field, true, req.From.Original
	}
	body, err := submission.EncodeRawSource(header, req.Raw)
	if err != nil || len(body) > maxSourceBytes {
		return submission.Receipt{}, submission.ErrInvalidMessage
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
	// Step 6: commit. Kind M and kind C rows differ in key prefix and window;
	// a message with neither has no row, so distinct messages never collide.
	record := Record{
		AppID: app.ID, SMTPCredentialID: app.SMTPCredentialID, SMTPCredentialDigest: app.SMTPCredentialDigest, Raw: true, Fence: fence,
		ObjectKey: objectKey, Adapter: submission.AdapterSMTP, From: req.From.Value, EnhancedProtection: req.EnhancedProtection, Hold: req.Hold,
		RecipientCount: len(req.Recipients), Subject: req.Scan.Subject, Recipients: req.Recipients, RecipientKinds: req.Kinds,
		SenderDomain: req.From.Domain, SenderAddress: req.From.Address, SenderName: req.From.Name, OriginalFromAddress: req.From.Original,
	}
	switch req.Replay {
	case ReplayMessageID:
		record.IdempotencyKey, record.IdempotencyTTL, record.RequestHash = ReplayMessageIDPrefix+hex.EncodeToString(req.ReplayDigest[:]), ReplayMessageIDWindow, req.ReplayDigest
	case ReplayContent:
		record.IdempotencyKey, record.IdempotencyTTL, record.RequestHash = ReplayContentPrefix+hex.EncodeToString(req.ReplayDigest[:]), ReplayContentWindow, req.ReplayDigest
	}
	receipt, err := s.repository.Commit(ctx, record)
	if err != nil {
		return submission.Receipt{}, err
	}
	receipt.Held = req.Hold
	// Step 7.
	if !receipt.Replayed {
		oplog.Message(app.ID, receipt.ID, oplog.StatusAccepted)
		if req.Hold {
			oplog.Message(app.ID, receipt.ID, oplog.StatusHeld)
		}
	}
	return receipt, nil
}
