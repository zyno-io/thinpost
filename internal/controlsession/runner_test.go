package controlsession

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/postgres"
)

type fakeStream struct {
	ctx      context.Context
	outgoing chan *controlv1.ControlFrame
	incoming chan *controlv1.EdgeFrame
}

func TestRouteRejectionMapsWithoutLosingReply(t *testing.T) {
	result := &controlv1.DeliveryResult{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 3, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ROUTE_REJECTED, SmtpCode: 554}
	mapped, err := mapResult("worker", "0123456789abcdef0123456789abcdef", result)
	if err != nil || mapped.Outcome != "route_rejected" || mapped.SMTPCode != 554 {
		t.Fatal("route rejection lost its scope or reply", mapped, err)
	}
}

func (s *fakeStream) Send(frame *controlv1.ControlFrame) error {
	select {
	case s.outgoing <- frame:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}
func (s *fakeStream) Recv() (*controlv1.EdgeFrame, error) {
	select {
	case frame, ok := <-s.incoming:
		if !ok {
			return nil, io.EOF
		}
		return frame, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

type fakeIssuer struct {
	mu          sync.Mutex
	calls       int
	expiresSoon bool
}

func (i *fakeIssuer) Acquire(_ context.Context, worker, controller string, ttl time.Duration) (postgres.ControllerLease, []byte, []byte, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	expiry := time.Now().Add(ttl)
	if i.expiresSoon {
		expiry = time.Now().Add(time.Millisecond)
	}
	return postgres.ControllerLease{WorkerID: worker, ControllerID: controller, Epoch: 1, ExpiresAt: expiry}, []byte{byte(i.calls)}, []byte("signature"), nil
}
func (i *fakeIssuer) Renew(ctx context.Context, current postgres.ControllerLease, ttl time.Duration) (postgres.ControllerLease, []byte, []byte, error) {
	return i.Acquire(ctx, current.WorkerID, current.ControllerID, ttl)
}
func (i *fakeIssuer) Release(context.Context, postgres.ControllerLease) error { return nil }

type cleanupIssuer struct {
	fakeIssuer
	streamCtx             context.Context
	released              postgres.ControllerLease
	cleanupErr, streamErr error
}

func (i *cleanupIssuer) Release(ctx context.Context, lease postgres.ControllerLease) error {
	i.released = lease
	i.cleanupErr = ctx.Err()
	i.streamErr = i.streamCtx.Err()
	return nil
}

func TestRunCancelsBeforeReleasingExactLease(t *testing.T) {
	base := &fakeIssuer{}
	runner, _, _ := setupRunner(base, &fakeResults{})
	issuer := &cleanupIssuer{}
	runner.Issuer = issuer
	runner.Connect = func(ctx context.Context) (Stream, error) {
		issuer.streamCtx = ctx
		return nil, errors.New("connection failed")
	}
	if err := runner.Run(context.Background()); err == nil {
		t.Fatal("connection failure ignored")
	}
	if issuer.released.WorkerID != "worker" || issuer.released.ControllerID != "controller" || issuer.released.Epoch != 1 {
		t.Fatal("wrong lease released", issuer.released)
	}
	if issuer.cleanupErr != nil || !errors.Is(issuer.streamErr, context.Canceled) {
		t.Fatal("lease cleanup did not use a live detached context after cancellation")
	}
}

type fakeResults struct {
	mu          sync.Mutex
	committed   bool
	err         error
	startErr    error
	authorized  bool
	startBegan  chan struct{}
	startGate   chan error
	commitBegan chan struct{}
	commitGate  chan struct{}
	replayed    bool
}

func (r *fakeResults) AuthorizeDeliveryStart(ctx context.Context, _ postgres.ControllerLease, _, _, _, _ string, _ [32]byte, _ int64) error {
	if r.startBegan != nil {
		close(r.startBegan)
	}
	if r.startGate != nil {
		select {
		case err := <-r.startGate:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startErr != nil {
		return r.startErr
	}
	r.authorized = true
	return nil
}
func (r *fakeResults) wasAuthorized() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.authorized }
func (r *fakeResults) ReconcileOrphanedAttempts(context.Context, postgres.ControllerLease) (bool, error) {
	return true, nil
}

type gatedReconciler struct {
	started chan struct{}
	release chan error
}

func (r *gatedReconciler) ReconcileOrphanedAttempts(ctx context.Context, _ postgres.ControllerLease) (bool, error) {
	close(r.started)
	select {
	case err := <-r.release:
		return err == nil, err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func TestReconciliationGatesWorkAdmission(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(map[bool]string{true: "success", false: "failure"}[success], func(t *testing.T) {
			runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
			gate := &gatedReconciler{started: make(chan struct{}), release: make(chan error, 1)}
			runner.Reconciler = gate
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
				t.Fatal("reconciliation did not start")
			}
			select {
			case <-out:
				t.Fatal("work admitted before reconciliation")
			default:
			}
			if success {
				gate.release <- nil
				select {
				case frame := <-out:
					assertReconciled(t, frame, open)
					select {
					case wake := <-out:
						if wake.GetWake() == nil {
							t.Fatal("work was not woken after reconciliation")
						}
					case <-ctx.Done():
						t.Fatal("wake did not follow reconciliation acknowledgement")
					}
				case <-ctx.Done():
					t.Fatal("reconciliation did not open admission")
				}
				cancel()
				<-done
			} else {
				gate.release <- errors.New("database failure")
				if err := <-done; err == nil {
					t.Fatal("failed reconciliation accepted")
				}
				select {
				case frame := <-out:
					if frame.GetWake() != nil {
						t.Fatal("work admitted after failed reconciliation")
					}
				default:
				}
			}
		})
	}
}

func TestStartPermitWaitsForDurableCommit(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(map[bool]string{true: "success", false: "failure"}[success], func(t *testing.T) {
			results := &fakeResults{startBegan: make(chan struct{}), startGate: make(chan error, 1)}
			runner, out, in := setupRunner(&fakeIssuer{}, results)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()
			open := (<-out).GetOpen()
			in <- readyFrame(open)
			in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
			assertReconciled(t, <-out, open)
			<-out
			in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Pull{Pull: &controlv1.Pull{AvailableSlots: 1}}}
			start := (<-out).GetDelivery()
			<-out
			digest, err := edgeprotocol.DeliveryDigest(start)
			if err != nil {
				t.Fatal(err)
			}
			request := &controlv1.StartRequest{WorkerId: "worker", JournalId: open.JournalId, SessionId: open.SessionId, AttemptId: start.AttemptId, DeliveryId: start.DeliveryId, Epoch: 1, DescriptorSha256: digest[:]}
			in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_StartRequest{StartRequest: request}}
			select {
			case <-results.startBegan:
			case <-ctx.Done():
				t.Fatal("authorization did not start")
			}
			select {
			case <-out:
				t.Fatal("permit sent before commit")
			default:
			}
			if success {
				results.startGate <- nil
			} else {
				results.startGate <- errors.New("database failure")
			}
			if success {
				select {
				case frame := <-out:
					if frame.GetStartPermit() == nil || !results.wasAuthorized() {
						t.Fatal("permit missing")
					}
				case <-ctx.Done():
					t.Fatal("permit not sent")
				}
				cancel()
				<-done
			} else {
				if err := <-done; err == nil {
					t.Fatal("failed authorization accepted")
				}
				select {
				case frame := <-out:
					if frame.GetStartPermit() != nil {
						t.Fatal("permit sent after failed commit")
					}
				default:
				}
			}
		})
	}
}

func (r *fakeResults) CommitDeliveryResultIdentified(ctx context.Context, result delivery.Result) (delivery.Committed, error) {
	return delivery.Committed{AppID: "0123456789abcdef0123456789abcdef", MessageID: "fedcba9876543210fedcba9876543210", Replayed: r.replayed}, r.commitDeliveryResult(ctx, result)
}

func (r *fakeResults) commitDeliveryResult(ctx context.Context, result delivery.Result) error {
	if r.commitBegan != nil {
		close(r.commitBegan)
	}
	if r.commitGate != nil {
		select {
		case <-r.commitGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.committed = result.AttemptID == "attempt"
	return nil
}

func TestReplayMustCommitBeforeReconciliation(t *testing.T) {
	results := &fakeResults{commitBegan: make(chan struct{}), commitGate: make(chan struct{})}
	runner, out, in := setupRunner(&fakeIssuer{}, results)
	gate := &gatedReconciler{started: make(chan struct{}), release: make(chan error, 1)}
	runner.Reconciler = gate
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	open := (<-out).GetOpen()
	in <- readyFrame(open)
	result := &controlv1.DeliveryResult{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED, SmtpCode: 250}
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Result{Result: result}}
	select {
	case <-results.commitBegan:
	case <-ctx.Done():
		t.Fatal("replay commit did not start")
	}
	select {
	case <-gate.started:
		t.Fatal("reconciliation began before replay commit")
	default:
	}
	close(results.commitGate)
	select {
	case frame := <-out:
		if frame.GetResultAck() == nil {
			t.Fatal("replay was not acknowledged")
		}
	case <-ctx.Done():
		t.Fatal("replay acknowledgement missing")
	}
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	select {
	case <-gate.started:
	case <-ctx.Done():
		t.Fatal("reconciliation did not follow replay")
	}
	cancel()
	<-done
}
func (r *fakeResults) wasCommitted() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.committed }

type fakeWork struct {
	mu   sync.Mutex
	used bool
}

func (w *fakeWork) Next(_ context.Context, lease postgres.ControllerLease, _ delivery.EdgeCapabilities) (*controlv1.DeliveryStart, []byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.used {
		return nil, nil, delivery.ErrNoWork
	}
	w.used = true
	mime := []byte("signed MIME")
	digest := sha256.Sum256(mime)
	return &controlv1.DeliveryStart{AttemptId: "attempt", DeliveryId: "delivery", Epoch: lease.Epoch, ExpectedSize: uint64(len(mime)), Sha256: digest[:], EnhancedProtection: true}, mime, nil
}
func setupRunner(issuer *fakeIssuer, results *fakeResults) (*Runner, chan *controlv1.ControlFrame, chan *controlv1.EdgeFrame) {
	out := make(chan *controlv1.ControlFrame, 16)
	in := make(chan *controlv1.EdgeFrame, 16)
	runner := &Runner{WorkerID: "worker", ControllerID: "controller", JournalID: "0123456789abcdef0123456789abcdef", Issuer: issuer, Results: results, Starts: results, Reconciler: results, Work: &fakeWork{}, LeaseTTL: 30 * time.Second, ClockMargin: time.Second, PollInterval: time.Second, Connect: func(ctx context.Context) (Stream, error) {
		return &fakeStream{ctx: ctx, outgoing: out, incoming: in}, nil
	}}
	return runner, out, in
}
func readyFrame(open *controlv1.Open) *controlv1.EdgeFrame {
	return &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Ready{Ready: &controlv1.Ready{WorkerId: open.WorkerId, JournalId: open.JournalId, Epoch: open.Epoch, SessionId: open.SessionId, ProtocolVersion: edgeprotocol.Version}}}
}

func assertReconciled(t *testing.T, frame *controlv1.ControlFrame, open *controlv1.Open) {
	t.Helper()
	ack := frame.GetReconciliationComplete()
	if ack == nil || ack.WorkerId != open.WorkerId || ack.JournalId != open.JournalId || ack.SessionId != open.SessionId || ack.Epoch != open.Epoch {
		t.Fatal("invalid reconciliation acknowledgement")
	}
}

func TestDifferentJournalCannotReplayOrReceiveWork(t *testing.T) {
	results := &fakeResults{}
	runner, out, in := setupRunner(&fakeIssuer{}, results)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	open := (<-out).GetOpen()
	ready := readyFrame(open)
	ready.GetReady().JournalId = "ffffffffffffffffffffffffffffffff"
	in <- ready
	if err := <-done; err == nil {
		t.Fatal("accepted unexpected journal identity")
	}
	if results.wasCommitted() || results.wasAuthorized() {
		t.Fatal("journal mismatch reached the database")
	}
	select {
	case frame := <-out:
		if frame.GetWake() != nil || frame.GetResultAck() != nil {
			t.Fatal("journal mismatch received work or acknowledgement")
		}
	default:
	}
}

func TestStartPermitRejectsDifferentJournal(t *testing.T) {
	results := &fakeResults{}
	runner, out, in := setupRunner(&fakeIssuer{}, results)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	open := (<-out).GetOpen()
	in <- readyFrame(open)
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	assertReconciled(t, <-out, open)
	<-out
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Pull{Pull: &controlv1.Pull{AvailableSlots: 1}}}
	start := (<-out).GetDelivery()
	<-out
	digest, err := edgeprotocol.DeliveryDigest(start)
	if err != nil {
		t.Fatal(err)
	}
	request := &controlv1.StartRequest{WorkerId: "worker", JournalId: "ffffffffffffffffffffffffffffffff", SessionId: open.SessionId, AttemptId: start.AttemptId, DeliveryId: start.DeliveryId, Epoch: 1, DescriptorSha256: digest[:]}
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_StartRequest{StartRequest: request}}
	if err = <-done; err == nil || results.wasAuthorized() {
		t.Fatal("different journal received a start authorization", err)
	}
}
func TestSessionCommitsBeforeAcknowledging(t *testing.T) {
	results := &fakeResults{}
	runner, out, in := setupRunner(&fakeIssuer{}, results)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	open := (<-out).GetOpen()
	if open == nil {
		t.Fatal("missing open")
	}
	in <- readyFrame(open)
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	assertReconciled(t, <-out, open)
	if (<-out).GetWake() == nil {
		t.Fatal("missing wake")
	}
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Pull{Pull: &controlv1.Pull{AvailableSlots: 1}}}
	start := (<-out).GetDelivery()
	if start == nil || !start.EnhancedProtection {
		t.Fatal("missing TLS requirement")
	}
	chunk := (<-out).GetMimeChunk()
	if chunk == nil || !chunk.Last || string(chunk.Data) != "signed MIME" {
		t.Fatal("bad MIME chunk")
	}
	digest, err := edgeprotocol.DeliveryDigest(start)
	if err != nil {
		t.Fatal(err)
	}
	request := &controlv1.StartRequest{WorkerId: "worker", JournalId: open.JournalId, SessionId: open.SessionId, AttemptId: start.AttemptId, DeliveryId: start.DeliveryId, Epoch: 1, DescriptorSha256: digest[:]}
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_StartRequest{StartRequest: request}}
	if permit := (<-out).GetStartPermit(); permit == nil || !results.wasAuthorized() {
		t.Fatal("permit preceded durable authorization")
	}
	result := &controlv1.DeliveryResult{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED, SmtpCode: 250}
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Result{Result: result}}
	ack := (<-out).GetResultAck()
	if ack == nil || !results.wasCommitted() {
		t.Fatal("ack preceded durable result")
	}
	if (<-out).GetWake() == nil {
		t.Fatal("missing next wake")
	}
	close(in)
	if err := <-done; !errors.Is(err, io.EOF) {
		t.Fatalf("session close: %v", err)
	}
}
func TestSessionDoesNotAcknowledgeFailedCommit(t *testing.T) {
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{err: errors.New("db down")})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	open := (<-out).GetOpen()
	in <- readyFrame(open)
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Result{Result: &controlv1.DeliveryResult{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED, SmtpCode: 250}}}
	if err := <-done; err == nil {
		t.Fatal("failed commit accepted")
	}
	select {
	case frame := <-out:
		if frame.GetResultAck() != nil {
			t.Fatal("failed commit acknowledged")
		}
	default:
	}
}
func TestSessionRejectsNearlyExpiredLease(t *testing.T) {
	runner, _, _ := setupRunner(&fakeIssuer{expiresSoon: true}, &fakeResults{})
	if err := runner.Run(context.Background()); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expired lease: %v", err)
	}
}
func TestSessionRenewsOnlyAfterMatchingAcknowledgement(t *testing.T) {
	issuer := &fakeIssuer{}
	runner, out, in := setupRunner(issuer, &fakeResults{})
	runner.LeaseTTL = 5 * time.Second
	runner.ClockMargin = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	open := (<-out).GetOpen()
	in <- readyFrame(open)
	renewal := (<-out).GetLeaseRenewal()
	if renewal == nil {
		t.Fatal("missing renewal")
	}
	digest := sha256.Sum256(renewal.LeasePayload)
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_LeaseAccepted{LeaseAccepted: &controlv1.LeaseAccepted{Epoch: 1, PayloadSha256: digest[:]}}}
	close(in)
	if err := <-done; !errors.Is(err, io.EOF) {
		t.Fatalf("session close: %v", err)
	}
}

func TestCommittedDeliveryResultIsLoggedWithIDsOnly(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	t.Cleanup(oplog.SetOutput(func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}))
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	open := (<-out).GetOpen()
	in <- readyFrame(open)
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Result{Result: &controlv1.DeliveryResult{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ROUTE_REJECTED, SmtpCode: 554}}}
	select {
	case frame := <-out:
		if frame.GetResultAck() == nil {
			t.Fatalf("expected acknowledgement, got %v", frame)
		}
	case <-ctx.Done():
		t.Fatal("no acknowledgement")
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	want := "delivery app=0123456789abcdef0123456789abcdef message=fedcba9876543210fedcba9876543210 delivery=invalid status=route_rejected smtp=554 ip=primary"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("unexpected log lines %q", lines)
	}
}

func TestReplayedDeliveryResultIsNotLoggedAgain(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	t.Cleanup(oplog.SetOutput(func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}))
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{replayed: true})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	open := (<-out).GetOpen()
	in <- readyFrame(open)
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Result{Result: &controlv1.DeliveryResult{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ROUTE_REJECTED, SmtpCode: 554}}}
	select {
	case frame := <-out:
		if frame.GetResultAck() == nil {
			t.Fatalf("expected acknowledgement, got %v", frame)
		}
	case <-ctx.Done():
		t.Fatal("no acknowledgement")
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 0 {
		t.Fatalf("replay was logged: %q", lines)
	}
}

func TestMapResultStageAndEnhancedCode(t *testing.T) {
	base := func() *controlv1.DeliveryResult {
		return &controlv1.DeliveryResult{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED, SmtpCode: 550, SmtpStage: controlv1.SmtpStage_SMTP_STAGE_RCPT, SmtpEnhanced: "5.1.1"}
	}
	for _, tc := range []struct {
		name            string
		change          func(*controlv1.DeliveryResult)
		stage, enhanced string
	}{
		{"valid", func(*controlv1.DeliveryResult) {}, "RCPT", "5.1.1"},
		{"old worker", func(r *controlv1.DeliveryResult) { r.SmtpStage, r.SmtpEnhanced = 0, "" }, "", ""},
		{"mail stage", func(r *controlv1.DeliveryResult) { r.SmtpStage = controlv1.SmtpStage_SMTP_STAGE_MAIL }, "MAIL", "5.1.1"},
		{"unknown stage value", func(r *controlv1.DeliveryResult) { r.SmtpStage = 99 }, "", "5.1.1"},
		{"malformed enhanced", func(r *controlv1.DeliveryResult) { r.SmtpEnhanced = "5.1.1 <a@b.example>" }, "RCPT", ""},
		{"class disagrees with code", func(r *controlv1.DeliveryResult) { r.SmtpEnhanced = "4.1.1" }, "RCPT", ""},
		{"no reply code", func(r *controlv1.DeliveryResult) { r.SmtpCode = 0 }, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.change(r)
			got, err := mapResult("worker", "journal", r)
			if err != nil || got.SMTPStage != tc.stage || got.SMTPEnhanced != tc.enhanced {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}
