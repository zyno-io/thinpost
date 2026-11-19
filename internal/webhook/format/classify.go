package format

// class is the provider-neutral meaning of an event. It is a pure function of
// immutable facts, so queueing and claiming agree.
type class int

const (
	classNone class = iota
	classAccepted
	classDeferred
	classPermanent
	classSoft
	classTrackingAbandoned
	classPreparationFailed
	classOpened
	classSuppressed
	classUnsubscribed
	classComplained
	classHoldExpired
)

// classify never yields a class for bounce_received_unverified: an
// unauthenticated receipt must not drive provider bounce events, which cause
// suppression downstream. Clicks and every other outcome wait for later work.
func classify(f Fact) class {
	rejected := f.Outcome == "rejected" || f.Outcome == "route_rejected"
	final := f.DeliveryState == "failed" || (f.DeliveryState == "" && f.Outcome == "rejected" && f.SMTPCode >= 500)
	switch {
	case f.Outcome == "accepted":
		return classAccepted
	case rejected && !final:
		return classDeferred
	case f.Outcome == "null_mx" || f.Outcome == "domain_not_found" || f.Outcome == "rejected" && final && f.SMTPCode >= 500:
		return classPermanent
	// route_rejected happens at greeting, EHLO or STARTTLS, before any recipient
	// is presented, so it never says the recipient is invalid, at any code.
	case f.Outcome == "retry_expired" || rejected && final:
		return classSoft
	case f.Outcome == "suppressed":
		return classSuppressed
	case f.Outcome == "tracking_abandoned":
		return classTrackingAbandoned
	case f.Outcome == "preparation_failed":
		return classPreparationFailed
	case f.Outcome == "opened":
		return classOpened
	case f.Outcome == "unsubscribed":
		return classUnsubscribed
	case f.Outcome == "complained":
		return classComplained
	// held, released and cancelled are app-driven hold transitions with no
	// provider meaning; an expired hold is a message that was never sent.
	case f.Outcome == "hold_expired":
		return classHoldExpired
	}
	return classNone
}
