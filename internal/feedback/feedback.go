// Package feedback turns ARF spam-complaint reports (RFC 5965) received at the
// global feedback mailbox into recipient complaints. It runs only in the trusted
// plane.
//
// A report is untrusted inbound mail. It takes effect only when it carries a
// valid DKIM signature from an operator-allowlisted reporter domain and its
// embedded original correlates to a Thinpost delivery that was accepted.
// Otherwise a daily counter changes and the report is acknowledged and
// discarded. Report bodies are parsed in memory and never stored or logged; the
// suppressed address always comes from the delivery's recipient row, never from
// the report.
package feedback

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"

	"github.com/zyno-io/thinpost/internal/oplog"
)

// Channel is the recipient_feedback and counter channel of this package.
const Channel = "arf"

// DKIMBudget bounds all DNS work of one report. Edge allows a 20 s forward.
const DKIMBudget = 5 * time.Second

// Result is the outcome counted in inbound_feedback_daily.
type Result string

const (
	Accepted        Result = "accepted"
	Duplicate       Result = "duplicate"
	Ignored         Result = "ignored"
	Unauthenticated Result = "unauthenticated"
	Unparsable      Result = "unparsable"
	Uncorrelated    Result = "uncorrelated"
	NotDelivered    Result = "not_delivered"
)

// Delivery is the correlation view of a delivery.
type Delivery struct {
	ID, MessageID, AppID string
	// SenderDomain is the message's sender (and signing) domain; empty for a
	// message from before schema 28.
	SenderDomain string
	State        string
	// BounceTokenHash is nil when the delivery has no bounce address yet.
	BounceTokenHash []byte
}

// Repository is the durable side. Every method runs under the open runtime
// gate and returns an error while it is closed, so the caller answers 451.
type Repository interface {
	// IsMailbox reports whether the token hash is the feedback mailbox.
	IsMailbox(ctx context.Context, tokenHash [32]byte) (bool, error)
	// Reporters lists the allowlisted reporter DKIM domains.
	Reporters(ctx context.Context) ([]string, error)
	// Candidates returns, from one snapshot, the deliveries a Message-ID local
	// part can refer to: the delivery with that ID when its message used
	// per-delivery Message-IDs (before schema 38), or every delivery (at most
	// 100) of the message with that ID when it used the shared form. Both forms
	// are accepted permanently. It returns no rows for an unknown ID.
	Candidates(ctx context.Context, id string) ([]Delivery, error)
	// Record commits, in one transaction, the complaint for the delivery and the
	// day's counter. It returns Accepted (new), Duplicate (already recorded),
	// NotDelivered or Uncorrelated (the delivery changed since Delivery).
	Record(ctx context.Context, deliveryID, reporterDomain string) (Result, error)
	// Count increments the day's counter for a result that records nothing else.
	Count(ctx context.Context, result Result) error
}

type Service struct {
	Repository Repository
	// LookupTXT resolves DKIM key records; nil uses the system resolver.
	LookupTXT func(ctx context.Context, name string) ([]string, error)
	// Budget overrides DKIMBudget (tests).
	Budget time.Duration
}

// Mailbox reports whether tokenHash addresses the feedback mailbox.
func (s *Service) Mailbox(ctx context.Context, tokenHash [32]byte) (bool, error) {
	return s.Repository.IsMailbox(ctx, tokenHash)
}

// Process handles one report received at the feedback mailbox. It returns nil
// once the report has been acted on or counted (including every rejection) and
// an error only when nothing was durably recorded, so the sender retries.
func (s *Service) Process(ctx context.Context, body []byte) error {
	parsed, err := parseARF(body)
	if err != nil {
		return s.count(ctx, Unparsable)
	}
	if parsed.feedbackType != "abuse" {
		return s.count(ctx, Ignored)
	}
	reporters, err := s.Repository.Reporters(ctx)
	if err != nil {
		return errors.New("feedback unavailable")
	}
	if len(reporters) == 0 {
		return s.count(ctx, Unauthenticated)
	}
	reporter, err := s.authenticate(ctx, body, reporters)
	if err != nil {
		return err
	}
	if reporter == "" {
		return s.count(ctx, Unauthenticated)
	}
	deliveryID, domain, bounceToken, hasReturn, ok := parsed.originalIDs()
	if !ok {
		return s.count(ctx, Uncorrelated)
	}
	candidates, err := s.Repository.Candidates(ctx, deliveryID)
	if err != nil {
		return errors.New("feedback unavailable")
	}
	matching := candidates[:0:0]
	for _, c := range candidates {
		if c.SenderDomain != "" && strings.EqualFold(c.SenderDomain, domain) {
			matching = append(matching, c)
		}
	}
	if len(matching) == 0 {
		return s.count(ctx, Uncorrelated)
	}
	// A present Return-Path must be one candidate's own bounce address (a
	// self-made message could otherwise borrow another delivery's Message-ID);
	// it also picks the reporting recipient of a shared-ID message. An absent
	// one is accepted only when the ID identifies a single delivery: some
	// reporters redact it, and a multi-recipient message cannot be attributed.
	var d Delivery
	if hasReturn {
		found := false
		if bounceToken != "" {
			hash := sha256.Sum256([]byte(bounceToken))
			for _, c := range matching {
				if len(c.BounceTokenHash) == len(hash) && bytes.Equal(c.BounceTokenHash, hash[:]) {
					d, found = c, true
					break
				}
			}
		}
		if !found {
			if len(matching) > 1 {
				// No sibling matched: attribute to the message, not to a recipient who was not involved.
				return s.countFor(ctx, Uncorrelated, Delivery{MessageID: matching[0].MessageID, AppID: matching[0].AppID})
			}
			return s.countFor(ctx, Uncorrelated, matching[0])
		}
	} else {
		if len(matching) != 1 {
			return s.countFor(ctx, Uncorrelated, Delivery{MessageID: matching[0].MessageID, AppID: matching[0].AppID})
		}
		d = matching[0]
	}
	if d.State != "accepted" {
		return s.countFor(ctx, NotDelivered, d)
	}
	result, err := s.Repository.Record(ctx, d.ID, reporter)
	if err != nil {
		return errors.New("feedback commit failed")
	}
	oplog.Feedback(Channel, string(result), d.AppID, d.MessageID, d.ID)
	return nil
}

func (s *Service) count(ctx context.Context, result Result) error {
	return s.countFor(ctx, result, Delivery{})
}

func (s *Service) countFor(ctx context.Context, result Result, d Delivery) error {
	if err := s.Repository.Count(ctx, result); err != nil {
		return errors.New("feedback unavailable")
	}
	oplog.Feedback(Channel, string(result), d.AppID, d.MessageID, d.ID)
	return nil
}

// authenticate returns the verified signing domain of a passing DKIM signature
// that equals, or is a subdomain of, an allowlisted reporter domain; empty when
// there is none. It errors only on a temporary DNS failure for such a domain, so
// the report is retried rather than discarded.
func (s *Service) authenticate(ctx context.Context, body []byte, reporters []string) (string, error) {
	budget := s.Budget
	if budget <= 0 {
		budget = DKIMBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	lookup := s.LookupTXT
	if lookup == nil {
		lookup = net.DefaultResolver.LookupTXT
	}
	results, verr := dkim.VerifyWithOptions(bytes.NewReader(body), &dkim.VerifyOptions{
		LookupTXT: func(name string) ([]string, error) {
			// No network query for a signer that is not allowlisted.
			lower := strings.ToLower(strings.TrimSuffix(name, "."))
			at := strings.LastIndex(lower, "._domainkey.")
			if at < 0 || !asciiOnly(name) || !allowed(lower[at+len("._domainkey."):], reporters) {
				return nil, errors.New("signer not allowlisted")
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return lookup(ctx, name)
		},
		MaxVerifications: 3,
	})
	if verr != nil && !errors.Is(verr, dkim.ErrTooManySignatures) {
		return "", nil
	}
	retry := false
	for _, v := range results {
		if v == nil {
			continue
		}
		if !asciiOnly(v.Domain) {
			continue
		}
		domain := strings.ToLower(strings.TrimSuffix(v.Domain, "."))
		if len(domain) == 0 || len(domain) > 253 || !allowed(domain, reporters) {
			continue
		}
		if v.Err == nil {
			// The signature must cover the Content-Type, which carries the
			// multipart boundary and report type.
			if !signsHeader(v.HeaderKeys, "content-type") {
				continue
			}
			return domain, nil
		}
		if dkim.IsTempFail(v.Err) {
			retry = true
		}
	}
	if retry {
		return "", errors.New("feedback dkim lookup unavailable")
	}
	return "", nil
}

func allowed(domain string, reporters []string) bool {
	for _, r := range reporters {
		if domain == r || strings.HasSuffix(domain, "."+r) {
			return true
		}
	}
	return false
}

func asciiOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func signsHeader(keys []string, name string) bool {
	for _, k := range keys {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}
