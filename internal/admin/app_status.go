package admin

import "context"

// AppStatusStore applies an audited app status transition. Repeating the same
// state is a no-op; it never revokes or creates application keys.
type AppStatusStore interface {
	SetAppActive(context.Context, string, string, bool) error
}
