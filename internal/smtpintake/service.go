// Package smtpintake is the control-plane intake of SMTP submissions. It is
// protocol independent: it knows nothing of the control stream, only of a
// credential, a raw message and the session fence it arrived under. Its
// callers map AuthResult and Outcome onto the wire.
package smtpintake

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/postgres"
	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/submission"
)

// AuthResult is the answer to a credential check. It reports the credential
// only: whether the app is paused is not part of authentication, so a paused
// app's clients can still log in and have a retry of an accepted message
// answered.
type AuthResult uint8

const (
	// AuthUnspecified is the zero value. It is never a success: callers treat
	// it as a temporary failure.
	AuthUnspecified AuthResult = iota
	AuthOK
	AuthInvalid
	AuthTempfail
	AuthUnavailable
)

// Outcome is the result of one submission.
type Outcome uint8

const (
	// OutcomeUnspecified is the zero value. It is never a success: callers
	// treat it as a temporary failure.
	OutcomeUnspecified Outcome = iota
	Accepted
	Tempfail
	RateLimited
	QuotaExceeded
	Backlog
	Unavailable
	CredentialInvalid
	AppPaused
	FromNotPermitted
	MessageInvalid
	TooLarge
	RecipientNotPermitted
	EncryptionRequired
	Misconfigured
	ContentNotSupported
)

var outcomeNames = [...]string{
	OutcomeUnspecified: "unspecified",
	Accepted:           "accepted", Tempfail: "tempfail", RateLimited: "rate_limited", QuotaExceeded: "quota_exceeded",
	Backlog: "backlog", Unavailable: "unavailable", CredentialInvalid: "credential_invalid", AppPaused: "app_paused",
	FromNotPermitted: "from_not_permitted", MessageInvalid: "message_invalid", TooLarge: "too_large",
	RecipientNotPermitted: "recipient_not_permitted", EncryptionRequired: "encryption_required",
	Misconfigured: "misconfigured", ContentNotSupported: "content_not_supported",
}

// String is the stable name used in logs.
func (o Outcome) String() string {
	if int(o) < len(outcomeNames) {
		return outcomeNames[o]
	}
	return "other"
}

// Result is the answer to Submit. MessageID is set only for Accepted. Held
// reports an accepted message stored on hold, a replay of one included.
type Result struct {
	Outcome   Outcome
	MessageID string
	Held      bool
}

// Message is one submission as the edge received it. It carries the secret and
// the body, so every formatting verb prints a placeholder.
type Message struct {
	TransferID   string
	CredentialID string
	Secret       string
	MailFrom     string
	Rcpts        []string
	ClientIP     netip.Addr
	TLS          bool
	Received     time.Time
	Body         []byte
}

func (Message) String() string   { return "<submission message redacted>" }
func (Message) GoString() string { return "<submission message redacted>" }

// Format redacts under every verb, including %+v, which would otherwise
// bypass String.
func (m Message) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(m.String())) }

// Repository is the database side of intake. postgres.Repository implements it.
type Repository interface {
	// VerifySMTPCredential returns the app and whether it is active; an
	// unknown, revoked or mismatching credential is submission.ErrInvalidKey.
	VerifySMTPCredential(ctx context.Context, id string, digest [32]byte) (appID string, active bool, err error)
	TouchSMTPCredential(ctx context.Context, id string) error
	// SMTPPolicy returns the app's From policy and which of the candidate
	// domains (plus the policy's own target) are active sender domains.
	SMTPPolicy(ctx context.Context, appID string, candidates []string) (rawmime.Policy, []string, error)
	LookupSMTPReplay(ctx context.Context, appID, key string) (submission.Receipt, bool, error)
}

type Service struct {
	Repository Repository
	Acceptance *acceptance.Service
	Signing    interface {
		Ready(ctx context.Context, appID, domain string) error
	}
	Replay ReplayKey
}

// Verify checks a credential. It touches the credential's last-used time on
// success, best effort.
func (s *Service) Verify(ctx context.Context, id, secret string, clientIP netip.Addr) AuthResult {
	_ = clientIP
	if s.Repository == nil {
		return AuthTempfail
	}
	_, _, err := s.Repository.VerifySMTPCredential(ctx, id, sha256.Sum256([]byte(secret)))
	switch {
	case err == nil:
		_ = s.Repository.TouchSMTPCredential(ctx, id)
		return AuthOK
	case errors.Is(err, submission.ErrInvalidKey):
		return AuthInvalid
	case errors.Is(err, postgres.ErrRuntimeClosed):
		return AuthUnavailable
	}
	return AuthTempfail
}

// Submit decides and accepts one message. The order is fixed: credential, scan,
// replay lookup (before the pause, rate, policy, signing and size checks, so a
// retry of an accepted message is answered whatever has changed since), pause,
// Enhanced Protection, From policy and recipient kinds, signing readiness,
// acceptance.
func (s *Service) Submit(ctx context.Context, fence submission.Fence, m Message) Result {
	appID := ""
	result := s.submit(ctx, fence, m, &appID)
	if result.Outcome == OutcomeUnspecified {
		result = Result{Outcome: Tempfail}
	}
	oplog.Submission(result.Outcome.String(), appID, m.TransferID)
	return result
}

func (s *Service) submit(ctx context.Context, fence submission.Fence, m Message, appID *string) Result {
	if s.Repository == nil || s.Acceptance == nil || s.Signing == nil || s.Replay == nil {
		return Result{Outcome: Tempfail}
	}
	// 1. Credential.
	digest := sha256.Sum256([]byte(m.Secret))
	id, active, err := s.Repository.VerifySMTPCredential(ctx, m.CredentialID, digest)
	if err != nil {
		return Result{Outcome: outcomeOf(err)}
	}
	*appID = id
	// 2. Scan.
	scanned, err := rawmime.Scan(m.Body)
	if err != nil {
		switch {
		case errors.Is(err, rawmime.ErrContentNotSupported):
			return Result{Outcome: ContentNotSupported}
		case errors.Is(err, rawmime.ErrTooLarge):
			return Result{Outcome: TooLarge}
		}
		return Result{Outcome: MessageInvalid}
	}
	keys, ok := RecipientKeys(m.Rcpts)
	if !ok || len(keys) == 0 || len(keys) > submission.MaxEnvelopeRecipients {
		return Result{Outcome: MessageInvalid}
	}
	// 3. Replay.
	kind := ReplayKindOf(scanned)
	var replayDigest [32]byte
	if kind != acceptance.ReplayNone {
		secret, err := s.Replay.Secret(ctx)
		if err != nil {
			return Result{Outcome: Tempfail}
		}
		replayDigest = Digest(secret, kind, id, m.MailFrom, keys, scanned.MessageID, m.Body)
		receipt, found, err := s.Repository.LookupSMTPReplay(ctx, id, replayKey(kind, replayDigest))
		if err != nil {
			return Result{Outcome: outcomeOf(err)}
		}
		if found {
			// The digest covers the raw bytes, hold header included, so the
			// original was held exactly when this copy asks for a hold.
			return Result{Outcome: Accepted, MessageID: receipt.ID, Held: scanned.Hold}
		}
	}
	// 4. Pause (fresh submissions only).
	if !active {
		return Result{Outcome: AppPaused}
	}
	// The per-message tracking choice is judged for fresh submissions only: a
	// retry of a message an older pod accepted must still get its original
	// answer. It is judged here, not in Scan, because Scan runs again on stored
	// sources.
	trackOpens, trackClicks, err := scanned.TrackingChoice()
	if err != nil {
		return Result{Outcome: MessageInvalid}
	}
	// 5. Enhanced Protection. AUTH requires TLS, so the second check is a guard.
	if scanned.RequireTLS {
		return Result{Outcome: MessageInvalid}
	}
	if scanned.Enhanced && !m.TLS {
		return Result{Outcome: EncryptionRequired}
	}
	// 6. Policy, From and recipient kinds.
	policy, verifiedList, err := s.Repository.SMTPPolicy(ctx, id, candidateDomains(scanned, m.MailFrom))
	if err != nil {
		return Result{Outcome: outcomeOf(err)}
	}
	verified := make(map[string]struct{}, len(verifiedList))
	for _, domain := range verifiedList {
		verified[domain] = struct{}{}
	}
	decision, err := rawmime.DecideFrom(scanned, m.MailFrom, policy, func(domain string) bool { _, ok := verified[domain]; return ok })
	if err != nil {
		switch {
		case errors.Is(err, rawmime.ErrFromNotPermitted):
			return Result{Outcome: FromNotPermitted}
		case errors.Is(err, rawmime.ErrMisconfigured):
			return Result{Outcome: Misconfigured}
		}
		return Result{Outcome: MessageInvalid}
	}
	recipients, kinds, err := rawmime.Kinds(m.Rcpts, scanned)
	if err != nil || len(recipients) == 0 {
		return Result{Outcome: MessageInvalid}
	}
	// 7. Signing readiness: mail that could never be signed is not accepted.
	if err = s.Signing.Ready(ctx, id, decision.Domain); err != nil {
		return Result{Outcome: Misconfigured}
	}
	// 8. Accept.
	req := acceptance.RawRequest{
		Raw: m.Body, Scan: scanned, MailFrom: m.MailFrom, Recipients: recipients, Kinds: make([]string, len(kinds)),
		From: decision, EnhancedProtection: scanned.Enhanced, Hold: scanned.Hold, Replay: kind, ReplayDigest: replayDigest,
		TrackOpens: trackOpens, TrackClicks: trackClicks,
	}
	for i, k := range kinds {
		req.Kinds[i] = string(k)
	}
	app := submission.App{
		ID: id, SMTPCredentialID: m.CredentialID, SMTPCredentialDigest: digest,
		DefaultTrackOpens: policy.DefaultTrackOpens, DefaultTrackClicks: policy.DefaultTrackClicks,
	}
	receipt, err := s.Acceptance.AcceptRaw(ctx, app, fence, req)
	if err != nil {
		return Result{Outcome: outcomeOf(err)}
	}
	return Result{Outcome: Accepted, MessageID: receipt.ID, Held: receipt.Held}
}

// candidateDomains lists the lowercase domains DecideFrom may ask about: the
// From, the MAIL FROM (used when there is no From) and every Sender.
func candidateDomains(s *rawmime.Scanned, mailFrom string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(domain string) {
		domain = strings.ToLower(domain)
		if _, dup := seen[domain]; domain != "" && !dup {
			seen[domain] = struct{}{}
			out = append(out, domain)
		}
	}
	if s.From != nil {
		add(domainOf(s.From.Address))
	}
	if mailFrom != "" && mailFrom != "<>" {
		add(domainOf(mailFrom))
	}
	for _, d := range s.SenderDomains {
		add(d)
	}
	add(s.SenderDomain)
	return out
}

func domainOf(address string) string {
	address = strings.TrimSuffix(strings.TrimPrefix(address, "<"), ">")
	return address[strings.LastIndexByte(address, '@')+1:]
}

// outcomeOf maps an error from the repository or acceptance to an outcome.
// Anything unlisted is a temporary failure: the client retries.
func outcomeOf(err error) Outcome {
	switch {
	case errors.Is(err, submission.ErrInvalidKey):
		return CredentialInvalid
	case errors.Is(err, submission.ErrAppPaused):
		return AppPaused
	case errors.Is(err, submission.ErrRateLimited):
		return RateLimited
	case errors.Is(err, submission.ErrQuotaExceeded):
		return QuotaExceeded
	case errors.Is(err, submission.ErrBacklogExceeded):
		return Backlog
	case errors.Is(err, submission.ErrMessageTooLarge):
		return TooLarge
	case errors.Is(err, submission.ErrRecipientNotPermitted):
		return RecipientNotPermitted
	case errors.Is(err, submission.ErrSenderDomainInactive):
		return Misconfigured
	case errors.Is(err, submission.ErrInvalidMessage), errors.Is(err, submission.ErrInvalidRequest):
		return MessageInvalid
	case errors.Is(err, postgres.ErrRuntimeClosed):
		return Unavailable
	}
	return Tempfail
}
