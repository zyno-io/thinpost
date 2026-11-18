package format

import "strconv"

// Thinpost-authored sentences for fields that carry remote response text in a
// provider. They are built only from recorded values and never imitate or
// quote a server.
func describe(c class, f Fact) string {
	code := strconv.Itoa(f.SMTPCode)
	switch c {
	case classAccepted:
		return "Accepted by the recipient's mail server (SMTP 250)."
	case classDeferred:
		s := "Temporarily rejected"
		if f.Outcome == "route_rejected" {
			s += " during connection setup"
		}
		if f.SMTPCode == 0 {
			return s + " without an SMTP reply; Thinpost will retry."
		}
		return s + " (SMTP " + code + "); Thinpost will retry."
	case classPermanent:
		switch f.Outcome {
		case "null_mx":
			return "The recipient domain does not accept mail (null MX). No SMTP attempt was made."
		case "domain_not_found":
			return "The recipient domain does not exist. No SMTP attempt was made."
		}
		return "Rejected by the recipient's mail server (SMTP " + code + ")."
	case classSoft:
		if f.Outcome == "retry_expired" {
			return "The retry lifetime expired before another attempt."
		}
		if f.SMTPCode == 0 {
			return "Delivery failed after the retry limit; the last attempt received no SMTP reply."
		}
		return "Delivery failed after the retry limit; the last attempt received SMTP " + code + "."
	case classSuppressed:
		return "The recipient is on the suppression list. No SMTP attempt was made."
	case classTrackingAbandoned:
		return "An operator marked this held delivery failed before any SMTP attempt."
	case classComplained:
		return "The recipient reported this message as spam."
	case classPreparationFailed:
		return "The message could not be prepared for delivery and is held for operator review."
	}
	return ""
}
