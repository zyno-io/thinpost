package admin

import "context"

// PolicyHoldStore atomically audits a release scoped to the owning app and
// message. A release does not bypass either recipient policy.
type PolicyHoldStore interface {
	RetryPolicyHold(context.Context, string, string, string, string) error
}
