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
	AttemptID          string
	DeliveryID         string
	MessageID          string
	AppID              string
	WorkerID           string
	Epoch              uint64
	SourceKey          string
	FinalKey           string
	RecipientIndex     int
	RouteAttempt       int
	EnhancedProtection bool
	AcceptedAt         time.Time
	TrackingSendCutoff time.Time
	// RecipientPolicyOverride is set when the message was submitted through the
	// console test send with the recipient allowlist skipped. The dispatch
	// recheck then does not apply the allowlist; the egress policy still runs.
	RecipientPolicyOverride bool
	// The sending IP chosen at claim time. SourceAddress and HeloName are set
	// only when the worker reported per-delivery sources and the app resolves to
	// a named IP; both are empty for an app on the worker's primary address with
	// no named record, and always for a legacy worker. SourceName is the log-safe
	// name of the app's named IP even then. BounceDomain is the return-path
	// domain; empty means the global one.
	SourceAddress, HeloName, SourceName, BounceDomain string
	// ResolvedAddress is always the address of the sending IP row the claim
	// joined (the app's pinned IP, or the primary's named record), whatever the
	// worker's mode; empty when no row joined. The provider checks it against
	// the worker's reported addresses independently of the claim query.
	ResolvedAddress string
}

// EdgeCapabilities is what a worker reported in Ready, or the legacy
// equivalent for a worker that reported nothing. Explicit is true only when
// the worker reported source addresses: then Addresses lists them with the
// primary first and control may name one in a DeliveryStart. Control never
// sets the DeliveryStart source fields for a legacy worker, which sends only
// from its primary address and its own hostname. A legacy worker's Addresses
// and BounceDomains still hold the one address and domain it can serve; they
// gate the claim.
type EdgeCapabilities struct {
	Explicit      bool
	Addresses     []netip.Addr
	BounceDomains []string
	// Hostnames is the SMTP name each address greets with, parallel to
	// Addresses. It is empty for a worker that does not report names.
	Hostnames []string
}

// HostnameFor returns the name the worker reported for address, if any. Control
// sends it as the HELO name so the HELO, the banner and the PTR agree.
func (c EdgeCapabilities) HostnameFor(address string) (string, bool) {
	if len(c.Hostnames) != len(c.Addresses) {
		return "", false
	}
	for i, candidate := range c.Addresses {
		if candidate.String() == address {
			return c.Hostnames[i], true
		}
	}
	return "", false
}

// Primary is the address that apps with no assigned sending IP send from.
func (c EdgeCapabilities) Primary() (netip.Addr, bool) {
	if len(c.Addresses) == 0 {
		return netip.Addr{}, false
	}
	return c.Addresses[0], true
}

// Has reports whether the worker can send from address.
func (c EdgeCapabilities) Has(address string) bool {
	for _, candidate := range c.Addresses {
		if candidate.String() == address {
			return true
		}
	}
	return false
}

// Committed identifies the parent records of a committed delivery result so
// callers can emit lifecycle logs without a second lookup.
type Committed struct {
	AppID, MessageID string
	// SourceName is the log-safe name of the sending IP the attempt used, or
	// "primary" when it used the worker's primary address.
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
