package edgesession

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/journal"
)

func TestBounceBridgeBoundsAndFencesSessions(t *testing.T) {
	var bridge bounceBridge
	fence := journal.Fence{ControllerID: "one", Epoch: 1}
	if err := bridge.activate(fence); err != nil {
		t.Fatal(err)
	}
	frames := make(chan *controlv1.EdgeFrame, 4)
	conn, err := bridge.attach(fence, func(frame *controlv1.EdgeFrame) error { frames <- frame; return nil }, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	const token = "0123456789abcdef0123456789abcdef"
	const id = "abcdef0123456789abcdef0123456789"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- bridge.Forward(ctx, token, id, []byte("body")) }()
	if (<-frames).GetBounce() == nil || (<-frames).GetBounceChunk() == nil {
		t.Fatal("bounce was not streamed")
	}
	if err = bridge.Forward(ctx, token, id, []byte("second")); err == nil {
		t.Fatal("concurrent bounce accepted")
	}
	bridge.acknowledge(conn, &controlv1.BounceAcknowledgement{BounceId: "00000000000000000000000000000000", DurablyAccepted: true})
	select {
	case <-result:
		t.Fatal("mismatched acknowledgement accepted")
	default:
	}
	bridge.acknowledge(conn, &controlv1.BounceAcknowledgement{BounceId: id, DurablyAccepted: false})
	if err = <-result; err == nil {
		t.Fatal("failed trusted commit accepted")
	}
	if err = bridge.activate(journal.Fence{ControllerID: "two", Epoch: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err = bridge.attach(fence, func(*controlv1.EdgeFrame) error { return nil }, func() bool { return true }); !errors.Is(err, journal.ErrStaleController) {
		t.Fatal("old controller attached")
	}
	if err = bridge.Forward(ctx, token, id, []byte("body")); err == nil {
		t.Fatal("bounce forwarded without active session")
	}
}

func TestBounceRejectsAcknowledgementAfterAuthorizationExpires(t *testing.T) {
	var bridge bounceBridge
	fence := journal.Fence{ControllerID: "one", Epoch: 1}
	if err := bridge.activate(fence); err != nil {
		t.Fatal(err)
	}
	var valid atomic.Bool
	valid.Store(true)
	frames := make(chan *controlv1.EdgeFrame, 2)
	conn, err := bridge.attach(fence, func(frame *controlv1.EdgeFrame) error { frames <- frame; return nil }, valid.Load)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	const id = "abcdef0123456789abcdef0123456789"
	result := make(chan error, 1)
	go func() { result <- bridge.Forward(ctx, "0123456789abcdef0123456789abcdef", id, []byte("body")) }()
	<-frames
	<-frames
	valid.Store(false)
	bridge.acknowledge(conn, &controlv1.BounceAcknowledgement{BounceId: id, DurablyAccepted: true})
	bridge.detach(conn)
	if err = <-result; err == nil {
		t.Fatal("expired controller acknowledgement was accepted")
	}
}
