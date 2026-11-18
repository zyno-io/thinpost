package controlsession

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
)

type bounceStore struct {
	started chan struct{}
	release chan error
}

func (b *bounceStore) Forward(ctx context.Context, token, id string, body []byte) error {
	if token != "0123456789abcdef0123456789abcdef" || id != "abcdef0123456789abcdef0123456789" || string(body) != "untrusted bounce" {
		return errors.New("wrong bounce")
	}
	close(b.started)
	select {
	case err := <-b.release:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestBounceAcknowledgedOnlyAfterCommit(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(map[bool]string{true: "success", false: "failure"}[success], func(t *testing.T) {
			runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
			store := &bounceStore{started: make(chan struct{}), release: make(chan error, 1)}
			runner.Bounces = store
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()
			open := (<-out).GetOpen()
			in <- readyFrame(open)
			in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
			assertReconciled(t, <-out, open)
			<-out // wake
			body := []byte("untrusted bounce")
			digest := sha256.Sum256(body)
			id := "abcdef0123456789abcdef0123456789"
			in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Bounce{Bounce: &controlv1.BounceStart{BounceId: id, RecipientToken: "0123456789abcdef0123456789abcdef", ExpectedSize: uint64(len(body)), Sha256: digest[:]}}}
			in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_BounceChunk{BounceChunk: &controlv1.DataChunk{Id: id, Data: body, Last: true}}}
			select {
			case <-store.started:
			case <-time.After(time.Second):
				t.Fatal("commit never started")
			}
			select {
			case <-out:
				t.Fatal("ack before commit")
			default:
			}
			if success {
				store.release <- nil
			} else {
				store.release <- errors.New("storage failure")
			}
			select {
			case frame := <-out:
				if ack := frame.GetBounceAck(); ack == nil || ack.BounceId != id || ack.DurablyAccepted != success {
					t.Fatal("incorrect acknowledgement")
				}
			case <-time.After(time.Second):
				t.Fatal("missing ack")
			}
			cancel()
			<-done
		})
	}
}

func TestBounceRejectedBeforeReconciliation(t *testing.T) {
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	gate := &gatedReconciler{started: make(chan struct{}), release: make(chan error, 1)}
	store := &bounceStore{started: make(chan struct{}), release: make(chan error, 1)}
	runner.Reconciler = gate
	runner.Bounces = store
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	open := (<-out).GetOpen()
	in <- readyFrame(open)
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	select {
	case <-gate.started:
	case <-ctx.Done():
		t.Fatal("reconciliation did not begin")
	}
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Bounce{Bounce: &controlv1.BounceStart{BounceId: "abcdef0123456789abcdef0123456789"}}}
	if err := <-done; err == nil {
		t.Fatal("bounce accepted before reconciliation")
	}
	select {
	case <-store.started:
		t.Fatal("bounce reached storage before reconciliation")
	default:
	}
}

func TestBounceAssemblerRejectsCorruption(t *testing.T) {
	digest := sha256.Sum256([]byte("original"))
	start := &controlv1.BounceStart{BounceId: "abcdef0123456789abcdef0123456789", RecipientToken: "0123456789abcdef0123456789abcdef", ExpectedSize: 8, Sha256: digest[:]}
	a, err := newBounceAssembler(start)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.add(&controlv1.DataChunk{Id: start.BounceId, Data: []byte("modified"), Last: true}); err == nil {
		t.Fatal("corrupt bounce accepted")
	}
	start.ExpectedSize = 17 << 20
	if _, err = newBounceAssembler(start); err == nil {
		t.Fatal("oversized bounce accepted")
	}
}
