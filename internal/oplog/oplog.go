// Package oplog writes one stdout line per message lifecycle event. It accepts
// only 32-character lowercase hex IDs, fixed enum statuses and numeric SMTP
// codes, so no subject, address or other free text can reach the log. The one
// exception is a delivery's sending IP, logged by its operator-owned HELO name
// (see Delivery); no recipient, sender or customer domain is ever logged.
package oplog

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Status is a message lifecycle status. Values outside the constants below are
// logged as StatusOther.
type Status string

const (
	StatusAccepted          Status = "accepted"
	StatusPrepared          Status = "prepared"
	StatusPreparationFailed Status = "preparation_failed"
	// Messages submitted on hold: accepted on hold, then released, cancelled
	// by the app, or expired without release.
	StatusHeld        Status = "held"
	StatusReleased    Status = "released"
	StatusCancelled   Status = "cancelled"
	StatusHoldExpired Status = "hold_expired"
	StatusOther       Status = "other"
)

// Delivery outcomes recorded by the delivery result path.
const (
	OutcomeAccepted              Status = "accepted"
	OutcomeRejected              Status = "rejected"
	OutcomeRouteRejected         Status = "route_rejected"
	OutcomeUncertain             Status = "uncertain"
	OutcomeRecipientPolicyDenied Status = "recipient_policy_denied"
	OutcomeNullMX                Status = "null_mx"
	OutcomeDomainNotFound        Status = "domain_not_found"
	OutcomeRetryExpired          Status = "retry_expired"
	OutcomeTrackingUnavailable   Status = "tracking_unavailable"
	OutcomeSuppressed            Status = "suppressed"
)

// invalidID replaces any ID that is not 32 lowercase hex characters.
const invalidID = "invalid"

var messageStatuses = map[Status]bool{StatusAccepted: true, StatusPrepared: true, StatusPreparationFailed: true,
	StatusHeld: true, StatusReleased: true, StatusCancelled: true, StatusHoldExpired: true}

var deliveryOutcomes = map[Status]bool{
	OutcomeAccepted: true, OutcomeRejected: true, OutcomeRouteRejected: true, OutcomeUncertain: true,
	OutcomeRecipientPolicyDenied: true, OutcomeNullMX: true, OutcomeDomainNotFound: true,
	OutcomeRetryExpired: true, OutcomeTrackingUnavailable: true, OutcomeSuppressed: true,
}

// preparationReasons mirrors the failure reasons persisted by the repository.
var preparationReasons = map[string]bool{
	"source_read": true, "source_invalid": true, "signing_key": true, "mime_build": true,
	"object_write": true, "queue_commit": true, "internal": true, "lease_expired": true,
}

// stdout is a dedicated logger so lifecycle lines go to stdout, not stderr,
// with the standard log timestamp format.
var stdout = log.New(os.Stdout, "", log.LstdFlags)

var (
	mu     sync.Mutex
	output = stdout.Printf
)

// SetOutput replaces the writer, which defaults to a logger on os.Stdout, and returns a
// function that restores the previous one. It exists for tests.
func SetOutput(f func(format string, args ...any)) (restore func()) {
	mu.Lock()
	defer mu.Unlock()
	previous := output
	output = f
	return func() {
		mu.Lock()
		defer mu.Unlock()
		output = previous
	}
}

func emit(format string, args ...any) {
	mu.Lock()
	f := output
	mu.Unlock()
	f(format, args...)
}

// ID returns id if it is 32 lowercase hex characters, otherwise "invalid".
func ID(id string) string {
	if len(id) != 32 {
		return invalidID
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return invalidID
		}
	}
	return id
}

// Message logs `message app=<id> message=<id> status=<status>`.
func Message(appID, messageID string, status Status) {
	if !messageStatuses[status] || status == StatusPreparationFailed {
		status = StatusOther
	}
	emit("message app=%s message=%s status=%s", ID(appID), ID(messageID), status)
}

// PreparationFailed logs `message app=<id> message=<id> status=preparation_failed reason=<reason>`.
// Reasons outside the persisted set are logged as "other".
func PreparationFailed(appID, messageID, reason string) {
	if !preparationReasons[reason] {
		reason = string(StatusOther)
	}
	emit("message app=%s message=%s status=%s reason=%s", ID(appID), ID(messageID), StatusPreparationFailed, reason)
}

// PreparationLeaseLost logs `message app=<id> message=<id> status=preparation_lease_lost`
// once when a worker's preparation claim could no longer be renewed.
func PreparationLeaseLost(appID, messageID string) {
	emit("message app=%s message=%s status=preparation_lease_lost", ID(appID), ID(messageID))
}

// Delivery logs `delivery app=<id> message=<id> delivery=<id> status=<outcome> smtp=<code> ip=<name>`.
// Unknown outcomes are logged as "other" and codes outside 0-999 as 0. ip is
// the HELO name of the sending IP (an operator-owned name from the Edge's
// configuration, the one deliberate exception to "no domains in logs"); it is
// never an address. Empty means a pre-schema-45 attempt that had no recorded
// source, logged as "primary". Anything that is not a valid name is logged as
// "other".
func Delivery(appID, messageID, deliveryID string, outcome Status, smtpCode int, ip string) {
	if !deliveryOutcomes[outcome] {
		outcome = StatusOther
	}
	if smtpCode < 0 || smtpCode > 999 {
		smtpCode = 0
	}
	emit("delivery app=%s message=%s delivery=%s status=%s smtp=%s ip=%s", ID(appID), ID(messageID), ID(deliveryID), outcome, strconv.Itoa(smtpCode), IPName(ip))
}

// IPName returns name if it is a plausible host name (lowercase letters,
// digits, dots and hyphens; non-empty labels of at most 63 characters that do
// not start or end with a hyphen; at most 253 characters in all), "primary" for
// the empty name, and "other" otherwise. A name whose last label is all digits,
// or that holds four consecutive all-digit labels anywhere, is "other": that is
// how an IPv4 address (or one embedded in a name such as 45.0.1.22.example)
// looks, and dotted digits pass the character set, so an address can never
// reach a log through it. The same goes for free text.
func IPName(name string) string {
	if name == "" {
		return "primary"
	}
	if len(name) > 253 {
		return string(StatusOther)
	}
	last, digitRun := "", 0
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return string(StatusOther)
		}
		for i := 0; i < len(label); i++ {
			if c := label[i]; (c < '0' || c > '9') && (c < 'a' || c > 'z') && c != '-' {
				return string(StatusOther)
			}
		}
		last = label
		if strings.Trim(label, "0123456789") == "" {
			if digitRun++; digitRun == 4 {
				return string(StatusOther)
			}
		} else {
			digitRun = 0
		}
	}
	if strings.Trim(last, "0123456789") == "" {
		return string(StatusOther)
	}
	return name
}

// FeedbackResults are the outcomes of one inbound feedback item, mirroring the
// persisted inbound_feedback_daily results. Anything else is logged as "other".
var feedbackResults = map[string]bool{
	"accepted": true, "duplicate": true, "ignored": true, "unauthenticated": true,
	"unparsable": true, "uncorrelated": true, "not_delivered": true,
}

// Feedback logs `feedback channel=<arf|one_click> result=<enum>` and, when the
// report correlated to a delivery, ` app=<id> message=<id> delivery=<id>`.
// Report content, addresses and reporter names never reach the log.
func Feedback(channel, result, appID, messageID, deliveryID string) {
	if channel != "arf" && channel != "one_click" {
		channel = string(StatusOther)
	}
	if !feedbackResults[result] {
		result = string(StatusOther)
	}
	if appID == "" && messageID == "" && deliveryID == "" {
		emit("feedback channel=%s result=%s", channel, result)
		return
	}
	emit("feedback channel=%s result=%s app=%s message=%s delivery=%s", channel, result, ID(appID), ID(messageID), ID(deliveryID))
}

// inboundResults are the outcomes of one inbound message, mirroring the
// persisted inbound_daily results plus the control-side refusals. Anything else
// is logged as "other".
var inboundResults = map[string]bool{
	"accepted": true, "duplicate": true, "correlated": true, "tempfail_routing": true,
	"tempfail_limit": true, "tempfail_internal": true, "rejected": true,
}

// Inbound logs `inbound result=<enum> app=<id> inbound=<id>`. Addresses,
// domains, headers and content never reach the log; an app unknown at the
// point of refusal and any malformed ID are logged as "invalid".
func Inbound(result, appID, inboundID string) {
	if !inboundResults[result] {
		result = string(StatusOther)
	}
	emit("inbound result=%s app=%s inbound=%s", result, ID(appID), ID(inboundID))
}

// submissionResults are the outcomes of one SMTP submission, mirroring
// smtpintake.Outcome. Anything else is logged as "other".
var submissionResults = map[string]bool{
	"accepted": true, "tempfail": true, "rate_limited": true, "quota_exceeded": true, "backlog": true,
	"unavailable": true, "credential_invalid": true, "app_paused": true, "from_not_permitted": true,
	"message_invalid": true, "too_large": true, "recipient_not_permitted": true, "encryption_required": true,
	"misconfigured": true, "content_not_supported": true,
}

// Submission logs `submission result=<enum> app=<id> transfer=<id>`. Addresses,
// headers, credentials and content never reach the log; an app unknown at the
// point of refusal and any malformed ID are logged as "invalid".
func Submission(result, appID, transferID string) {
	if !submissionResults[result] {
		result = string(StatusOther)
	}
	emit("submission result=%s app=%s transfer=%s", result, ID(appID), ID(transferID))
}

var domainKinds = map[string]bool{"bounce": true, "tracking": true}

var domainStates = map[string]bool{"pending": true, "active": true, "failing": true, "removed": true}

var domainResults = map[string]bool{
	"ok": true, "missing": true, "not_cname": true, "other_ip": true, "wrong_target": true,
	"lookup_failed": true, "no_sending_ip": true, "unreachable": true, "certificate": true,
	"redirected": true, "not_thinpost": true, "not_found": true,
}

// DomainCheck logs `domain_check app=<id> kind=<enum> state=<enum> result=<enum>`
// when a customer domain's state changes. The domain itself, what its DNS
// answered and any error text never reach the log; anything outside the enums
// is logged as "other".
func DomainCheck(appID, kind, state, result string) {
	if !domainKinds[kind] {
		kind = string(StatusOther)
	}
	if !domainStates[state] {
		state = string(StatusOther)
	}
	if !domainResults[result] {
		result = string(StatusOther)
	}
	emit("domain_check app=%s kind=%s state=%s result=%s", ID(appID), kind, state, result)
}
