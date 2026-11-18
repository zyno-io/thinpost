package controlsession

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/postgres"
)

type drainingIssuer struct {
	fakeIssuer
	released chan struct{}
	once     sync.Once
}

func TestDrainWaitsForBounceCommit(t *testing.T) {
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	issuer := &drainingIssuer{released: make(chan struct{})}
	runner.Issuer = issuer
	store := &bounceStore{started: make(chan struct{}), release: make(chan error, 1)}
	runner.Bounces = store
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	drain := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- runner.RunDraining(ctx, drain) }()
	open := (<-out).GetOpen()
	in <- readyFrame(open)
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	assertReconciled(t, <-out, open)
	<-out
	body := []byte("untrusted bounce")
	digest := sha256.Sum256(body)
	id := "abcdef0123456789abcdef0123456789"
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Bounce{Bounce: &controlv1.BounceStart{BounceId: id, RecipientToken: "0123456789abcdef0123456789abcdef", ExpectedSize: uint64(len(body)), Sha256: digest[:]}}}
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_BounceChunk{BounceChunk: &controlv1.DataChunk{Id: id, Data: body, Last: true}}}
	select {
	case <-store.started:
	case <-ctx.Done():
		t.Fatal("bounce commit did not start")
	}
	close(drain)
	select {
	case <-issuer.released:
		t.Fatal("released lease before bounce commit")
	default:
	}
	store.release <- nil
	if ack := (<-out).GetBounceAck(); ack == nil || !ack.DurablyAccepted {
		t.Fatal("committed bounce was not acknowledged")
	}
	if err := <-done; !errors.Is(err, ErrDraining) {
		t.Fatal("session did not drain", err)
	}
}

func (i *drainingIssuer) Release(context.Context, postgres.ControllerLease) error {
	i.once.Do(func() { close(i.released) })
	return nil
}

type gatedWork struct {
	started chan struct{}
	release chan struct{}
	work    fakeWork
}

func (w *gatedWork) Next(ctx context.Context, lease postgres.ControllerLease, caps delivery.EdgeCapabilities) (*controlv1.DeliveryStart, []byte, error) {
	close(w.started)
	select {
	case <-w.release:
		return w.work.Next(ctx, lease, caps)
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func TestDrainBeforeAcquisitionDoesNotClaimLease(t *testing.T) {
	issuer := &fakeIssuer{}
	runner, _, _ := setupRunner(issuer, &fakeResults{})
	drain := make(chan struct{})
	close(drain)
	if err := runner.RunDraining(context.Background(), drain); !errors.Is(err, ErrDraining) || issuer.calls != 0 {
		t.Fatal("draining process acquired a lease", err, issuer.calls)
	}
}

func TestDrainRejectsPullRacingWithShutdown(t *testing.T) {
	for i := 0; i < 20; i++ {
		runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
		work := &fakeWork{}
		runner.Work = work
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		drain := make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- runner.RunDraining(ctx, drain) }()
		open := (<-out).GetOpen()
		in <- readyFrame(open)
		in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
		assertReconciled(t, <-out, open)
		<-out
		close(drain)
		in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Pull{Pull: &controlv1.Pull{AvailableSlots: 1}}}
		if err := <-done; !errors.Is(err, ErrDraining) {
			cancel()
			t.Fatal("session did not drain", err)
		}
		cancel()
		work.mu.Lock()
		claimed := work.used
		work.mu.Unlock()
		if claimed {
			t.Fatal("claimed new work after shutdown")
		}
	}
}

func TestDrainFinishesClaimedDeliveryBeforeReleasingLease(t *testing.T) {
	results := &fakeResults{commitBegan: make(chan struct{}), commitGate: make(chan struct{})}
	runner, out, in := setupRunner(&fakeIssuer{}, results)
	issuer := &drainingIssuer{released: make(chan struct{})}
	runner.Issuer = issuer
	work := &gatedWork{started: make(chan struct{}), release: make(chan struct{})}
	runner.Work = work
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	drain := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- runner.RunDraining(ctx, drain) }()
	open := (<-out).GetOpen()
	in <- readyFrame(open)
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	assertReconciled(t, <-out, open)
	<-out // wake
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Pull{Pull: &controlv1.Pull{AvailableSlots: 1}}}
	select {
	case <-work.started:
	case <-ctx.Done():
		t.Fatal("work did not start")
	}
	close(drain)
	select {
	case <-issuer.released:
		t.Fatal("released lease while a claim was in progress")
	default:
	}
	close(work.release)
	start := (<-out).GetDelivery()
	if start == nil || (<-out).GetMimeChunk() == nil {
		t.Fatal("claimed work was abandoned")
	}
	digest, err := edgeprotocol.DeliveryDigest(start)
	if err != nil {
		t.Fatal(err)
	}
	request := &controlv1.StartRequest{WorkerId: "worker", JournalId: open.JournalId, SessionId: open.SessionId, AttemptId: start.AttemptId, DeliveryId: start.DeliveryId, Epoch: start.Epoch, DescriptorSha256: digest[:]}
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_StartRequest{StartRequest: request}}
	if (<-out).GetStartPermit() == nil {
		t.Fatal("start was not permitted")
	}
	result := &controlv1.DeliveryResult{AttemptId: start.AttemptId, DeliveryId: start.DeliveryId, Epoch: start.Epoch, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED, SmtpCode: 250}
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Result{Result: result}}
	select {
	case <-results.commitBegan:
	case <-ctx.Done():
		t.Fatal("result commit did not start")
	}
	select {
	case <-issuer.released:
		t.Fatal("released lease before result commit")
	default:
	}
	close(results.commitGate)
	if ack := (<-out).GetResultAck(); ack == nil || !results.wasCommitted() {
		t.Fatal("result was not committed before acknowledgement")
	}
	if err = <-done; !errors.Is(err, ErrDraining) {
		t.Fatal("session did not drain", err)
	}
	select {
	case <-issuer.released:
	default:
		t.Fatal("drained lease was not released")
	}
	select {
	case frame := <-out:
		if frame.GetWake() != nil || frame.GetDelivery() != nil {
			t.Fatal("drained session requested more work")
		}
	default:
	}
}

func TestDrainDeadlineCancelsStalledClaim(t *testing.T) {
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	work := &gatedWork{started: make(chan struct{}), release: make(chan struct{})}
	runner.Work = work
	ctx, cancel := context.WithCancel(context.Background())
	drain := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- runner.RunDraining(ctx, drain) }()
	open := (<-out).GetOpen()
	in <- readyFrame(open)
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	assertReconciled(t, <-out, open)
	<-out
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Pull{Pull: &controlv1.Pull{AvailableSlots: 1}}}
	<-work.started
	close(drain)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("forced deadline did not cancel session", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forced cancellation stalled")
	}
}

func TestDrainKeepsRenewingLeaseForActiveWork(t *testing.T) {
	issuer := &fakeIssuer{}
	runner, out, in := setupRunner(issuer, &fakeResults{})
	runner.LeaseTTL = 5 * time.Second
	runner.ClockMargin = time.Second
	work := &gatedWork{started: make(chan struct{}), release: make(chan struct{})}
	runner.Work = work
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drain := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- runner.RunDraining(ctx, drain) }()
	open := (<-out).GetOpen()
	in <- readyFrame(open)
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	assertReconciled(t, <-out, open)
	<-out
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Pull{Pull: &controlv1.Pull{AvailableSlots: 1}}}
	<-work.started
	close(drain)
	select {
	case frame := <-out:
		renewal := frame.GetLeaseRenewal()
		if renewal == nil {
			t.Fatal("active drain did not renew its lease")
		}
		digest := sha256.Sum256(renewal.LeasePayload)
		in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_LeaseAccepted{LeaseAccepted: &controlv1.LeaseAccepted{Epoch: open.Epoch, PayloadSha256: digest[:]}}}
	case <-ctx.Done():
		t.Fatal("lease renewal did not arrive")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("forced cancellation failed", err)
	}
}
