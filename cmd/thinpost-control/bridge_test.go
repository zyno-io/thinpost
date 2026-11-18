package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/controlsession"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestBridgeFailureReasonNeverIncludesUntrustedErrorText(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{errors.New("postgres://user:private-password@database/message-body"), "internal"},
		{status.Error(codes.Unavailable, "private-address and message-body"), "rpc_Unavailable"},
		{status.Error(codes.FailedPrecondition, "journal identity mismatch"), "journal_identity_mismatch"},
		{status.Error(codes.PermissionDenied, "controller lease unavailable"), "lease_unavailable"},
		{status.Error(codes.FailedPrecondition, "controller fence rejected"), "fence_rejected"},
		{status.Error(codes.FailedPrecondition, "unreconciled journal attempt"), "unreconciled_attempt"},
		{context.DeadlineExceeded, "deadline"},
	} {
		if got := bridgeFailureReason(test.err); got != test.want {
			t.Fatalf("safe diagnostic = %q; want %q", got, test.want)
		}
	}
}

type testBridge struct {
	started  chan struct{}
	draining chan struct{}
	finish   chan struct{}
	canceled chan struct{}
	closed   chan struct{}
	calls    atomic.Int32
}

func (b *testBridge) RunDraining(ctx context.Context, drain <-chan struct{}) error {
	b.calls.Add(1)
	close(b.started)
	select {
	case <-drain:
		close(b.draining)
	case <-ctx.Done():
		close(b.canceled)
		return ctx.Err()
	}
	select {
	case <-b.finish:
		return controlsession.ErrDraining
	case <-ctx.Done():
		close(b.canceled)
		return ctx.Err()
	}
}
func (b *testBridge) Close() error { close(b.closed); return nil }

func newTestBridge() *testBridge {
	return &testBridge{started: make(chan struct{}), draining: make(chan struct{}), finish: make(chan struct{}), canceled: make(chan struct{}), closed: make(chan struct{})}
}

func TestBridgeShutdownWaitsForDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge := newTestBridge()
	stop := startBridge(ctx, bridge, time.Second)
	<-bridge.started
	cancel()
	<-bridge.draining
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("bridge closed before in-flight work drained")
	case <-bridge.canceled:
		t.Fatal("bridge canceled before its grace period")
	default:
	}
	close(bridge.finish)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("drained bridge did not stop")
	}
	select {
	case <-bridge.closed:
	default:
		t.Fatal("bridge was not closed")
	}
	if bridge.calls.Load() != 1 {
		t.Fatal("bridge reconnected after shutdown")
	}
}

func TestBridgeShutdownCancelsAfterGrace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge := newTestBridge()
	stop := startBridge(ctx, bridge, 30*time.Millisecond)
	<-bridge.started
	cancel()
	<-bridge.draining
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("grace deadline did not stop bridge")
	}
	select {
	case <-bridge.canceled:
	default:
		t.Fatal("grace deadline did not cancel bridge context")
	}
}

func TestBridgeAbnormalExitCancelsWithoutWaitingForDrain(t *testing.T) {
	bridge := newTestBridge()
	stop := startBridge(context.Background(), bridge, time.Second)
	<-bridge.started
	stop()
	select {
	case <-bridge.canceled:
	default:
		t.Fatal("abnormal exit did not cancel bridge")
	}
}
