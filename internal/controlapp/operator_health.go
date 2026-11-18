package controlapp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// OperatorHealth periodically checks the operator database independently of
// incoming HTTP requests. A stale success expires even if a check is stuck.
type OperatorHealth struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
	validTo atomic.Pointer[time.Time]
}

func StartOperatorHealth(parent context.Context, ping func(context.Context) error) *OperatorHealth {
	return startOperatorHealth(parent, ping, 5*time.Second, 2*time.Second, 15*time.Second)
}

func startOperatorHealth(parent context.Context, ping func(context.Context) error, interval, timeout, validity time.Duration) *OperatorHealth {
	ctx, cancel := context.WithCancel(parent)
	health := &OperatorHealth{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(health.done)
		defer health.validTo.Store(nil)
		if ping == nil {
			return
		}
		for ctx.Err() == nil {
			checkCtx, checkCancel := context.WithTimeout(ctx, timeout)
			err := ping(checkCtx)
			checkErr := checkCtx.Err()
			checkCancel()
			if err != nil || checkErr != nil || ctx.Err() != nil {
				health.validTo.Store(nil)
			} else {
				expires := time.Now().Add(validity)
				health.validTo.Store(&expires)
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
			}
		}
	}()
	return health
}

func (h *OperatorHealth) Ready() bool {
	if h == nil || h.ctx.Err() != nil {
		return false
	}
	expires := h.validTo.Load()
	return expires != nil && time.Now().Before(*expires)
}

func (h *OperatorHealth) Close() error {
	if h == nil {
		return errors.New("operator health is not configured")
	}
	h.once.Do(func() {
		h.cancel()
		<-h.done
	})
	return nil
}
