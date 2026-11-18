package webhook

import (
	"context"
	"errors"
	"time"
)

type DispatchSource interface{ DispatchNext(context.Context) error }

// Run drains available webhook work, then polls with a bounded interval. The
// reporter receives no endpoint, secret, event body, or underlying error.
func Run(ctx context.Context, dispatcher DispatchSource, interval time.Duration, report func()) error {
	if dispatcher == nil || interval <= 0 || interval > time.Minute {
		return errors.New("invalid webhook scheduler")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := dispatcher.DispatchNext(ctx)
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !errors.Is(err, ErrNoWork) && report != nil {
			report()
		}
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		}
	}
}
