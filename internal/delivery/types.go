// Package delivery defines durable per-recipient attempt and result contracts.
package delivery

import (
	"errors"
	"net/netip"
	"time"
)

var ErrNoWork = errors.New("no delivery to dispatch")
var ErrLeaseInvalid = errors.New("controller lease is not active")
var ErrResultConflict = errors.New("delivery result conflicts with committed result")
var ErrAttemptSuperseded = errors.New("delivery attempt is not current")
var ErrAppInactive = errors.New("delivery application is inactive")

// These are local DNS conclusions, not replies from an SMTP peer.
var ErrNullMX = errors.New("recipient domain does not accept mail")
var ErrDomainNotFound = errors.New("recipient domain does not exist")

const OutcomeNullMX = "null_mx"
const OutcomeDomainNotFound = "domain_not_found"

// OutcomeRecipientPolicyDenied is a pre-SMTP policy hold, not a mail-server reply.
const OutcomeRecipientPolicyDenied = "recipient_policy_denied"

var ErrRecipientPolicyDenied = errors.New("recipient policy denied delivery")

// An unavailable mapping holds a delivery before SMTP; new signed redirects
// remain usable independently of analytics mapping availability.
const OutcomeTrackingUnavailable = "tracking_unavailable"

// OutcomeRetryExpired is a local timeout, not an SMTP response.
const OutcomeRetryExpired = "retry_expired"

// OutcomeSuppressed is a local outcome: the recipient was on the app's
// suppression list when the attempt was claimed. It is not an SMTP response.
const OutcomeSuppressed = "suppressed"

var ErrTrackingUnavailable = errors.New("delivery tracking mapping unavailable")

// The Edge bounds network execution; the extra margin also covers a delayed
// start permit and a maximum-duration controller lease under normal clocks.
const MaxSMTPNetworkDuration = 2 * time.Hour
const TrackingMinimumRemaining = 2 * MaxSMTPNetworkDuration
const TrackingClockMargin = 5 * time.Minute

type Claim struct {
	AttemptID  string
	DeliveryID string
	MessageID  string
	AppID      string
	WorkerID   string
	Epoch      uint64
	SourceKey  string
	FinalKey   string
	// PreparedFormat is the stored format of the final object (1 full MIME
	// message, 2 recipe), as recorded when the delivery was prepared.
	PreparedFormat     int
	RecipientIndex     int
	RouteAttempt       int
	EnhancedProtection bool
	AcceptedAt         time.Time
	TrackingSendCutoff time.Time
	// RecipientPolicyOverride is set when the message was submitted through the
	// console test send with the recipient allowlist skipped. The dispatch
	// recheck then does not apply the allowlist; the egress policy still runs.
	RecipientPolicyOverride bool
	// The sending IP chosen at claim time, always from the live session's
	// report and always set: the app's pinned IP, else the main IP.
	// SourceAddress and HeloName are what the worker is asked to send from and
	// greet with. SourceName is the log-safe name of that IP (its HELO name).
	// BounceDomain is the return-path domain: the reported name of the address
	// the delivery leaves from, never empty on a claim. It stays separate from
	// HeloName so a later change can give an app its own.
	SourceAddress, HeloName, SourceName, BounceDomain string
	// BounceTarget is set only when BounceDomain is an app's custom bounce
	// domain rather than the HELO name: the HELO name of the source address
	// that domain was verified to point at. The provider accepts the custom
	// domain only if this still equals the reported name of the source.
	BounceTarget string
	// ResolvedAddress is the address of the app's pinned sending IP; empty for
	// an app on the main IP (its SourceAddress is still explicit). The provider
	// checks it against the worker's reported addresses independently of the
	// claim query.
	ResolvedAddress string
}

// Source is one sending address a worker reported and the name it greets, is
// reached by and receives bounces under.
type Source struct {
	Address  netip.Addr
	Hostname string
}

// EdgeCapabilities is what a worker reported in Ready. Sources lists every
// address it can send from with its name, the primary first; control addresses
// each delivery from them and never from its own configuration. A worker that
// reported no sources, or no names for them, has an empty list and is given no
// deliveries.
type EdgeCapabilities struct {
	Sources []Source
	// SubmissionPorts are the SMTP submission ports the worker reported
	// listening on; nil means it reported none. They only drive a console hint.
	SubmissionPorts []int
	// AnyBounceDomain is set when the worker reported bounce-any-domain/1: it
	// accepts bounces addressed to any domain, so a delivery may use a custom
	// bounce domain. Without it every Return-Path is the source's HELO name.
	AnyBounceDomain bool
}

// HasHostname reports whether name is the reported name of one of the
// worker's own addresses. Those names are never a customer's bounce domain.
func (c EdgeCapabilities) HasHostname(name string) bool {
	for _, candidate := range c.Sources {
		if candidate.Hostname == name {
			return true
		}
	}
	return false
}

// Primary is the source that apps with no assigned sending IP send from.
func (c EdgeCapabilities) Primary() (Source, bool) {
	if len(c.Sources) == 0 {
		return Source{}, false
	}
	return c.Sources[0], true
}

// Source returns the reported source for address, if any.
func (c EdgeCapabilities) Source(address string) (Source, bool) {
	for _, candidate := range c.Sources {
		if candidate.Address.String() == address {
			return candidate, true
		}
	}
	return Source{}, false
}

// Has reports whether the worker can send from address.
func (c EdgeCapabilities) Has(address string) bool {
	_, ok := c.Source(address)
	return ok
}

// Committed identifies the parent records of a committed delivery result so
// callers can emit lifecycle logs without a second lookup.
type Committed struct {
	AppID, MessageID string
	// SourceName is the log-safe name of the sending IP the attempt used: its
	// HELO name, "removed" when its row is gone, or "primary" for an attempt
	// recorded before schema 45 without a source.
	SourceName string
	// Replayed is true when the exact result had already been committed.
	Replayed bool
}

type Result struct {
	AttemptID, DeliveryID, WorkerID, JournalID string
	Epoch                                      uint64
	Outcome                                    string
	SMTPCode                                   int
	// SMTPStage (MAIL, RCPT, DATA) and SMTPEnhanced (RFC 3463 digits) are
	// reported by schema-34 Edge workers with 4xx/5xx replies and otherwise
	// empty. Neither is reply text.
	SMTPStage, SMTPEnhanced string
	// SuppressionReason is set only by the claim transaction for
	// OutcomeSuppressed; it is never populated from an Edge result.
	SuppressionReason string
}
