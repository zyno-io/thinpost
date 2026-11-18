package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/zyno-io/thinpost/internal/postgres"
)

// startIdempotencyCleanup bounds each sweep and uses row locking to cooperate
// across replicas. Monitoring for a persistent cleanup backlog remains needed.
func startIdempotencyCleanup(parent context.Context, repository *postgres.Repository) func() {
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
					count, err := repository.CleanupExpiredIdempotency(ctx, 1000)
					if err != nil {
						if ctx.Err() == nil {
							log.Print("idempotency cleanup unavailable")
						}
						break
					}
					if count < 1000 {
						break
					}
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { cancel(); <-done }) }
}
