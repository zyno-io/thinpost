package submission

import (
	"context"
	"errors"
	"time"
)

// ErrAllRecipientsSuppressed is returned by Accept only when the request set
// RejectAllSuppressed and every distinct recipient is on the app's list. It is
// returned before anything is stored, so it never follows a durable accept.
var ErrAllRecipientsSuppressed = errors.New("every recipient is suppressed")

// ProviderSuppression is one list entry as the provider-compatible APIs see
// it. Address is secret input data: never log it or put it in a URL.
type ProviderSuppression struct {
	ID              string
	Address         string
	Reason          string
	Origin          string
	SourceMessageID string
	CreatedAt       time.Time
}

// ProviderSuppressionFilter selects entries, newest first. Empty Reasons or
// Origins match everything. From is inclusive, To exclusive; zero means none.
// After and Before are suppression IDs of entries in the same app: After
// returns entries older than that one, Before newer ones (still newest first).
type ProviderSuppressionFilter struct {
	Reasons []string
	Origins []string
	From    time.Time
	To      time.Time
	After   string
	Before  string
	Limit   int
}

// SuppressionStore backs the provider suppression APIs. Every call is scoped to
// the authenticated app and needs the open gate. Addresses are passed already
// normalized and are never placed in URLs, logs or audit rows.
type SuppressionStore interface {
	// ListProviderSuppressions returns up to Limit entries and whether more
	// exist. An After or Before ID unknown in the app is ErrInvalidMessage.
	ListProviderSuppressions(ctx context.Context, appID string, filter ProviderSuppressionFilter) ([]ProviderSuppression, bool, error)
	// AddProviderSuppressions adds manual entries with origin api and is
	// idempotent for addresses already on the list. Like a send, the write
	// transaction requires the app and the key (app.KeyID) to still be active;
	// otherwise it returns ErrInvalidKey and writes nothing.
	AddProviderSuppressions(ctx context.Context, app App, addresses []string) error
	// DeleteProviderSuppressions removes entries a provider API may remove
	// (origin other than recipient, reason other than spam_complaint). The
	// result reports, per input address, false when the entry exists but is
	// protected; an absent address is true.
	DeleteProviderSuppressions(ctx context.Context, app App, addresses []string) ([]bool, error)
}
