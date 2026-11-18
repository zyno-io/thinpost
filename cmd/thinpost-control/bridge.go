package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/zyno-io/thinpost/internal/controlsession"
	"github.com/zyno-io/thinpost/internal/postgres"
	"google.golang.org/grpc/status"
)

type bridgeSession interface {
	RunDraining(context.Context, <-chan struct{}) error
	Close() error
}

// startBridge drains the current stream on process shutdown and cancels it
// after a bounded grace period. Abnormal exit still cancels immediately.
func startBridge(parent context.Context, bridge bridgeSession, grace time.Duration) func() {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	done := make(chan struct{})
	var closeOnce sync.Once
	closeBridge := func() { closeOnce.Do(func() { _ = bridge.Close() }) }
	go func() {
		defer close(done)
		for ctx.Err() == nil && parent.Err() == nil {
			err := bridge.RunDraining(ctx, parent.Done())
			if ctx.Err() != nil || parent.Err() != nil || errors.Is(err, controlsession.ErrDraining) {
				return
			}
			if !errors.Is(err, postgres.ErrLeaseHeld) {
				log.Printf("edge control session unavailable; retrying reason=%s", bridgeFailureReason(err))
			}
			timer := time.NewTimer(5 * time.Second)
			select {
			case <-timer.C:
			case <-ctx.Done():
			case <-parent.Done():
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}()
	go func() {
		select {
		case <-parent.Done():
			timer := time.NewTimer(grace)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
				cancel()
				closeBridge()
			}
		case <-done:
		}
	}()
	return func() {
		if parent.Err() == nil {
			cancel()
			closeBridge()
		}
		<-done
		cancel()
		closeBridge()
	}
}

// Report only fixed protocol diagnostics. Database and transport errors can
// contain connection details, so their text must never enter runtime logs.
func bridgeFailureReason(err error) string {
	if err == nil {
		return "ended"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(err, controlsession.ErrLeaseExpired) {
		return "lease_expired"
	}
	s, ok := status.FromError(err)
	if !ok {
		return "internal"
	}
	switch s.Message() {
	case "journal identity mismatch":
		return "journal_identity_mismatch"
	case "controller lease unavailable":
		return "lease_unavailable"
	case "controller fence rejected":
		return "fence_rejected"
	case "unreconciled journal attempt":
		return "unreconciled_attempt"
	case "journal unavailable", "invalid journal result":
		return "journal_unavailable"
	case "controller lease expired":
		return "lease_expired"
	default:
		return "rpc_" + s.Code().String()
	}
}
