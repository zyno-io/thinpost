package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/zyno-io/thinpost/internal/postgres"
)

// startHoldExpiry ends messages whose hold expired without release, once a
// minute. Sweeps are bounded and skip rows another replica holds, so every
// replica can run it; a release that races the sweep waits for the row lock.
// A release after the expiry time is refused even before the sweep reaches the
// message, so the sweep's delay only postpones the event and the pending slot.
func startHoldExpiry(parent context.Context, repository *postgres.Repository) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for i := 0; i < 8; i++ {
					count, err := repository.ExpireHeldMessages(ctx, 100)
					if err != nil {
						if ctx.Err() == nil {
							log.Print("hold expiry unavailable")
						}
						break
					}
					if count < 100 {
						break
					}
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { cancel(); <-done }) }
}
