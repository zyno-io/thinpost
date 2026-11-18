package admin

import "context"

// TrackingHoldStore atomically records the operator's decision, terminal
// delivery state, and application notification without resending mail.
type TrackingHoldStore interface {
	AbandonTrackingHold(context.Context, string, string, string, string) error
}
