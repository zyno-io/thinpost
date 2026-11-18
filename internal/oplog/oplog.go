// Package oplog writes one stdout line per message lifecycle event. It accepts
// only 32-character lowercase hex IDs, fixed enum statuses and numeric SMTP
// codes, so no subject, address, domain or other free text can reach the log.
package oplog

import (
	"log"
	"os"
	"strconv"
	"sync"
)

// Status is a message lifecycle status. Values outside the constants below are
// logged as StatusOther.
type Status string

const (
	StatusAccepted          Status = "accepted"
	StatusPrepared          Status = "prepared"
	StatusPreparationFailed Status = "preparation_failed"
	StatusOther             Status = "other"
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

var messageStatuses = map[Status]bool{StatusAccepted: true, StatusPrepared: true, StatusPreparationFailed: true}

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
// Unknown outcomes are logged as "other" and codes outside 0-999 as 0. ip is the
// sending IP's name (never its address); empty means the worker's primary
// address, logged as "primary". Anything that is not a valid name is logged as "other".
func Delivery(appID, messageID, deliveryID string, outcome Status, smtpCode int, ip string) {
	if !deliveryOutcomes[outcome] {
		outcome = StatusOther
	}
	if smtpCode < 0 || smtpCode > 999 {
		smtpCode = 0
	}
	emit("delivery app=%s message=%s delivery=%s status=%s smtp=%s ip=%s", ID(appID), ID(messageID), ID(deliveryID), outcome, strconv.Itoa(smtpCode), IPName(ip))
}

// IPName returns name if it is a valid sending IP name (a lowercase slug of at
// most 32 characters), "primary" for the empty name, and "other" otherwise, so
// an address or free text can never reach a log through it.
func IPName(name string) string {
	if name == "" {
		return "primary"
	}
	if len(name) > 32 || name[0] == '-' || name[len(name)-1] == '-' {
		return string(StatusOther)
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; (c < '0' || c > '9') && (c < 'a' || c > 'z') && c != '-' {
			return string(StatusOther)
		}
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
