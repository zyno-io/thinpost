package controlsession

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/postgres"
	"github.com/zyno-io/thinpost/internal/smtpintake"
	"github.com/zyno-io/thinpost/internal/submission"
)

const (
	subA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	subB      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	subCred   = "cccccccccccccccccccccccccccccccc"
	subSecret = "tps_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
)

type submitCall struct {
	fence submission.Fence
	msg   smtpintake.Message
}

type fakeSubmission struct {
	mu          sync.Mutex
	verify      smtpintake.AuthResult
	verifyBlock chan struct{} // when set, Verify waits for it to close
	calls       []submitCall
	started     chan submitCall
	release     chan smtpintake.Result
	// unwind, when set, is waited on after the release even if ctx is done, to
	// model a worker that is slow to exit.
	unwind chan struct{}
}

func newFakeSubmission() *fakeSubmission {
	return &fakeSubmission{verify: smtpintake.AuthOK, started: make(chan submitCall, 64), release: make(chan smtpintake.Result, 64)}
}

func (f *fakeSubmission) Verify(ctx context.Context, id, secret string, _ netip.Addr) smtpintake.AuthResult {
	f.mu.Lock()
	block, result := f.verifyBlock, f.verify
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
		}
	}
	return result
}

func (f *fakeSubmission) Submit(ctx context.Context, fence submission.Fence, m smtpintake.Message) smtpintake.Result {
	call := submitCall{fence, m}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	unwind := f.unwind
	f.mu.Unlock()
	f.started <- call
	var result smtpintake.Result
	select {
	case result = <-f.release:
	case <-ctx.Done():
		result = smtpintake.Result{Outcome: smtpintake.Tempfail}
	}
	if unwind != nil {
		<-unwind
	}
	return result
}

type subHarness struct {
	runner *Runner
	out    chan *controlv1.ControlFrame
	in     chan *controlv1.EdgeFrame
	open   *controlv1.Open
	done   chan error
	cancel context.CancelFunc
}

func startSubmissionRunner(t *testing.T, runner *Runner, out chan *controlv1.ControlFrame, in chan *controlv1.EdgeFrame, edgeCap bool, drain <-chan struct{}) *subHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	h := &subHarness{runner: runner, out: out, in: in, done: make(chan error, 1), cancel: cancel}
	go func() { h.done <- runner.RunDraining(ctx, drain) }()
	// A reconnecting Runner may leave frames of the earlier session in out.
	for h.open == nil {
		select {
		case frame := <-out:
			h.open = frame.GetOpen()
		case <-time.After(5 * time.Second):
			t.Fatal("missing open")
		}
	}
	ready := readyFrame(h.open)
	if edgeCap {
		ready.GetReady().Capabilities = []string{edgeprotocol.CapSubmission}
		ready.GetReady().SubmissionPorts = []uint32{587}
	}
	in <- ready
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	assertReconciled(t, <-out, h.open)
	return h
}

func startSubmissionSession(t *testing.T, sub Submission, edgeCap bool, tune func(*Runner)) *subHarness {
	t.Helper()
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	runner.Submission = sub
	if tune != nil {
		tune(runner)
	}
	return startSubmissionRunner(t, runner, out, in, edgeCap, nil)
}

func (h *subHarness) next(t *testing.T) *controlv1.ControlFrame {
	t.Helper()
	for {
		select {
		case frame := <-h.out:
			if frame.GetWake() != nil {
				continue
			}
			return frame
		case <-time.After(5 * time.Second):
			t.Fatal("no frame")
		}
	}
}

func (h *subHarness) nextAck(t *testing.T) *controlv1.SubmissionAcknowledgement {
	t.Helper()
	for {
		if ack := h.next(t).GetSubmissionAck(); ack != nil {
			return ack
		}
	}
}

func (h *subHarness) nextAuth(t *testing.T) *controlv1.SubmissionAuthResult {
	t.Helper()
	for {
		if r := h.next(t).GetAuthResult(); r != nil {
			return r
		}
	}
}

func (h *subHarness) start(id string, body []byte) {
	digest := sha256.Sum256(body)
	h.in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Submission{Submission: &controlv1.SubmissionStart{SubmissionId: id, ExpectedSize: uint64(len(body)), Sha256: digest[:], CredentialId: subCred, Secret: subSecret, MailFrom: "a@example.org", RcptTo: []string{"u@example.com"}, ClientIp: "192.0.2.1", TlsVersion: "TLS 1.3", ReceivedUnixMs: 1700000000000, Port: 587}}}
}

func (h *subHarness) chunk(id string, offset int, data []byte, last bool) {
	h.in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_SubmissionChunk{SubmissionChunk: &controlv1.DataChunk{Id: id, Offset: uint64(offset), Data: data, Last: last}}}
}

func (h *subHarness) auth(id string) {
	h.in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Auth{Auth: &controlv1.SubmissionAuth{RequestId: id, CredentialId: subCred, Secret: subSecret, ClientIp: "192.0.2.1"}}}
}

func (h *subHarness) send(t *testing.T, id string, body []byte) {
	t.Helper()
	h.start(id, body)
	h.chunk(id, 0, body, true)
}

func (h *subHarness) ended(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end")
	}
	return nil
}

func (h *subHarness) alive(t *testing.T) {
	t.Helper()
	select {
	case err := <-h.done:
		t.Fatalf("the session ended: %v", err)
	default:
	}
}

func reqID(n int) string {
	const hex = "0123456789abcdef"
	b := []byte(strings.Repeat("0", 32))
	for i := 31; n > 0 && i >= 0; i-- {
		b[i] = hex[n%16]
		n /= 16
	}
	return string(b)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- negotiation and compatibility ---

func TestSubmissionCapabilityAdvertisedOnlyWithService(t *testing.T) {
	runner, out, _ := setupRunner(&fakeIssuer{}, &fakeResults{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	if open := (<-out).GetOpen(); open == nil || len(open.Capabilities) != 0 {
		t.Fatalf("a control plane without submission offered %v", open.GetCapabilities())
	}
	h := startSubmissionSession(t, newFakeSubmission(), true, nil)
	if !edgeprotocol.HasCapability(h.open.Capabilities, edgeprotocol.CapSubmission) {
		t.Fatal("the capability was not advertised")
	}
	// With inbound too, both are listed.
	runner2, out2, _ := setupRunner(&fakeIssuer{}, &fakeResults{})
	runner2.Submission, runner2.Inbound = newFakeSubmission(), newFakeInbound()
	go func() { _ = runner2.Run(ctx) }()
	open := (<-out2).GetOpen()
	if !edgeprotocol.HasCapability(open.Capabilities, edgeprotocol.CapInbound) || !edgeprotocol.HasCapability(open.Capabilities, edgeprotocol.CapSubmission) {
		t.Fatalf("%v", open.Capabilities)
	}
}

func TestSubmissionFramesWithoutNegotiationEndTheSession(t *testing.T) {
	frames := map[string]func(h *subHarness){
		"auth":  func(h *subHarness) { h.auth(subA) },
		"start": func(h *subHarness) { h.start(subA, []byte("x")) },
		"chunk": func(h *subHarness) { h.chunk(subA, 0, []byte("x"), true) },
	}
	for name, send := range frames {
		t.Run("edge without capability/"+name, func(t *testing.T) {
			h := startSubmissionSession(t, newFakeSubmission(), false, nil)
			send(h)
			if err := h.ended(t); err == nil {
				t.Fatal("no error")
			}
		})
		t.Run("control without service/"+name, func(t *testing.T) {
			h := startSubmissionSession(t, nil, true, nil)
			send(h)
			if err := h.ended(t); err == nil {
				t.Fatal("no error")
			}
		})
	}
}

func TestSubmissionFramesBeforeReconciliationEndTheSession(t *testing.T) {
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	runner.Submission = newFakeSubmission()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	open := (<-out).GetOpen()
	ready := readyFrame(open)
	ready.GetReady().Capabilities = []string{edgeprotocol.CapSubmission}
	in <- ready
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Auth{Auth: &controlv1.SubmissionAuth{RequestId: subA, CredentialId: subCred, Secret: subSecret}}}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end")
	}
}

func TestNoSubmissionFramesAreSentWithoutNegotiation(t *testing.T) {
	// An edge that never advertised the capability gets none of the lane's
	// frames, and the lane's tickers do not disturb a session.
	h := startSubmissionSession(t, newFakeSubmission(), false, func(r *Runner) { r.SubmissionPartialTTL = 40 * time.Millisecond })
	select {
	case frame := <-h.out:
		if frame.GetWake() == nil {
			t.Fatalf("unexpected frame %v", frame)
		}
	case <-time.After(300 * time.Millisecond):
	}
	h.alive(t)
}

func TestReadySubmissionPortsAreValidated(t *testing.T) {
	for name, ports := range map[string][]uint32{"zero": {0}, "big": {65536}, "dup": {587, 587}, "many": {1, 2, 3, 4, 5, 6, 7, 8, 9}} {
		t.Run(name, func(t *testing.T) {
			runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
			runner.Submission = newFakeSubmission()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()
			open := (<-out).GetOpen()
			ready := readyFrame(open)
			ready.GetReady().Capabilities = []string{edgeprotocol.CapSubmission}
			ready.GetReady().SubmissionPorts = ports
			in <- ready
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("no error")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("a bad Ready was accepted")
			}
		})
	}
}

type subPortReports struct{ caps chan []int }

func (r *subPortReports) RecordEdgeSources(_ context.Context, _ postgres.ControllerLease, c delivery.EdgeCapabilities) error {
	r.caps <- c.SubmissionPorts
	return nil
}

func TestReadyPortsAreRecordedOnlyWhenSubmissionIsServed(t *testing.T) {
	for _, withService := range []bool{true, false} {
		reports := &subPortReports{caps: make(chan []int, 1)}
		var sub Submission
		if withService {
			sub = newFakeSubmission()
		}
		h := startSubmissionSession(t, sub, true, func(r *Runner) { r.Reports = reports })
		_ = h
		select {
		case ports := <-reports.caps:
			if withService && (len(ports) != 1 || ports[0] != 587) {
				t.Fatalf("recorded %v", ports)
			}
			if !withService && ports != nil {
				t.Fatalf("recorded %v without a service", ports)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no report")
		}
	}
}

// --- verify ---

func TestSubmissionVerifyAnswersWithTheMappedVerdict(t *testing.T) {
	for result, want := range map[smtpintake.AuthResult]controlv1.SubmissionAuthVerdict{
		smtpintake.AuthOK:          controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK,
		smtpintake.AuthInvalid:     controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_INVALID,
		smtpintake.AuthTempfail:    controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_TEMPFAIL,
		smtpintake.AuthUnavailable: controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_UNAVAILABLE,
		smtpintake.AuthUnspecified: controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_TEMPFAIL,
		smtpintake.AuthResult(99):  controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_TEMPFAIL,
	} {
		sub := newFakeSubmission()
		sub.verify = result
		h := startSubmissionSession(t, sub, true, nil)
		h.auth(subA)
		got := h.nextAuth(t)
		if got.RequestId != subA || got.Verdict != want {
			t.Fatalf("%v mapped to %v", result, got)
		}
		h.cancel()
	}
}

func TestSubmissionVerifyIsRateLimitedPerSession(t *testing.T) {
	h := startSubmissionSession(t, newFakeSubmission(), true, nil)
	const total = 120
	go func() {
		for i := 1; i <= total; i++ {
			h.auth(reqID(i))
		}
	}()
	ok, throttled := 0, 0
	for i := 0; i < total; i++ {
		switch h.nextAuth(t).Verdict {
		case controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK:
			ok++
		case controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_THROTTLED:
			throttled++
		}
	}
	// Burst of 40 plus whatever 20/s refills while the loop runs.
	if ok < 40 || ok > 60 || throttled == 0 {
		t.Fatalf("ok=%d throttled=%d", ok, throttled)
	}
	h.alive(t)
}

func TestSubmissionVerifyPendingCap(t *testing.T) {
	sub := newFakeSubmission()
	sub.verifyBlock = make(chan struct{})
	h := startSubmissionSession(t, sub, true, nil)
	for i := 1; i <= edgeprotocol.MaxAuthPending; i++ {
		h.auth(reqID(i))
	}
	over := reqID(100)
	h.auth(over)
	got := h.nextAuth(t)
	if got.RequestId != over || got.Verdict != controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_THROTTLED {
		t.Fatalf("%v", got)
	}
	close(sub.verifyBlock)
	for i := 0; i < edgeprotocol.MaxAuthPending; i++ {
		if got := h.nextAuth(t); got.Verdict != controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK {
			t.Fatalf("%v", got)
		}
	}
	// The slots are free again.
	h.auth(reqID(101))
	if got := h.nextAuth(t); got.Verdict != controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK {
		t.Fatalf("%v", got)
	}
}

func TestSubmissionVerifyResultIsDroppedAfterLeaseExpiry(t *testing.T) {
	sub := newFakeSubmission()
	sub.verifyBlock = make(chan struct{})
	// TTL 5s with a 2s margin leaves the lease valid for 3s. No renewal
	// acknowledgement ever arrives, so it is not extended.
	h := startSubmissionSession(t, sub, true, func(r *Runner) { r.LeaseTTL, r.ClockMargin = 5*time.Second, 2*time.Second })
	h.auth(subA)
	time.Sleep(3200 * time.Millisecond)
	close(sub.verifyBlock)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case frame := <-h.out:
			if frame.GetAuthResult() != nil {
				t.Fatal("a result was sent after the lease expired")
			}
		case err := <-h.done:
			if err == nil {
				t.Fatal("no error")
			}
			return
		case <-deadline:
			t.Fatal("session still running")
		}
	}
}

func TestSubmissionVerifyWhileDrainingIsTempfail(t *testing.T) {
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	runner.Submission = newFakeSubmission()
	drain := make(chan struct{})
	h := startSubmissionRunner(t, runner, out, in, true, drain)
	// A pending item keeps the session open while draining.
	h.start(subB, []byte("ab"))
	h.chunk(subB, 0, []byte("a"), false)
	eventually(t, "the partial to reserve", func() bool { return runner.submissionBudget().inUse() > 0 })
	time.Sleep(50 * time.Millisecond)
	close(drain)
	time.Sleep(50 * time.Millisecond)
	h.auth(subA)
	select {
	case err := <-h.done:
		t.Fatalf("session ended: %v", err)
	default:
	}
	got := h.nextAuth(t)
	if got.Verdict != controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_TEMPFAIL {
		t.Fatalf("%v", got)
	}
}

// --- submit ---

func TestSubmissionAcknowledgedOnlyAfterSubmitReturns(t *testing.T) {
	sub := newFakeSubmission()
	h := startSubmissionSession(t, sub, true, nil)
	body := []byte(strings.Repeat("x", edgeprotocol.MaxChunkBytes+5))
	h.start(subA, body)
	h.chunk(subA, 0, body[:edgeprotocol.MaxChunkBytes], false)
	h.chunk(subA, edgeprotocol.MaxChunkBytes, body[edgeprotocol.MaxChunkBytes:], true)
	call := <-sub.started
	select {
	case frame := <-h.out:
		if frame.GetSubmissionAck() != nil {
			t.Fatal("acknowledged before the commit returned")
		}
	case <-time.After(200 * time.Millisecond):
	}
	m := call.msg
	if call.fence != (submission.Fence{WorkerID: "worker", ControllerID: "controller", Epoch: 1}) {
		t.Fatalf("fence %+v", call.fence)
	}
	if m.TransferID != subA || m.CredentialID != subCred || m.Secret != subSecret || m.MailFrom != "a@example.org" || len(m.Rcpts) != 1 || m.ClientIP.String() != "192.0.2.1" || !m.TLS || string(m.Body) != string(body) || m.Received.UnixMilli() != 1700000000000 {
		t.Fatalf("%+v", m)
	}
	sub.release <- smtpintake.Result{Outcome: smtpintake.Accepted, MessageID: "0123456789abcdef0123456789abcdef", Held: true}
	ack := h.nextAck(t)
	if ack.SubmissionId != subA || ack.Verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED || ack.MessageId != "0123456789abcdef0123456789abcdef" || !ack.Held {
		t.Fatalf("%v", ack)
	}
}

func TestSubmissionOutcomesAreMapped(t *testing.T) {
	want := map[smtpintake.Outcome]controlv1.SubmissionVerdict{
		smtpintake.Accepted:              controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED,
		smtpintake.Tempfail:              controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL,
		smtpintake.RateLimited:           controlv1.SubmissionVerdict_SUBMISSION_VERDICT_RATE_LIMITED,
		smtpintake.QuotaExceeded:         controlv1.SubmissionVerdict_SUBMISSION_VERDICT_QUOTA_EXCEEDED,
		smtpintake.Backlog:               controlv1.SubmissionVerdict_SUBMISSION_VERDICT_BACKLOG,
		smtpintake.Unavailable:           controlv1.SubmissionVerdict_SUBMISSION_VERDICT_UNAVAILABLE,
		smtpintake.CredentialInvalid:     controlv1.SubmissionVerdict_SUBMISSION_VERDICT_CREDENTIAL_INVALID,
		smtpintake.AppPaused:             controlv1.SubmissionVerdict_SUBMISSION_VERDICT_APP_PAUSED,
		smtpintake.FromNotPermitted:      controlv1.SubmissionVerdict_SUBMISSION_VERDICT_FROM_NOT_PERMITTED,
		smtpintake.MessageInvalid:        controlv1.SubmissionVerdict_SUBMISSION_VERDICT_MESSAGE_INVALID,
		smtpintake.TooLarge:              controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TOO_LARGE,
		smtpintake.RecipientNotPermitted: controlv1.SubmissionVerdict_SUBMISSION_VERDICT_RECIPIENT_NOT_PERMITTED,
		smtpintake.EncryptionRequired:    controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ENCRYPTION_REQUIRED,
		smtpintake.Misconfigured:         controlv1.SubmissionVerdict_SUBMISSION_VERDICT_MISCONFIGURED,
		smtpintake.ContentNotSupported:   controlv1.SubmissionVerdict_SUBMISSION_VERDICT_CONTENT_NOT_SUPPORTED,
		smtpintake.OutcomeUnspecified:    controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL,
		smtpintake.Outcome(200):          controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL,
	}
	for outcome, verdict := range want {
		if got := submissionVerdict(outcome); got != verdict {
			t.Errorf("%v mapped to %v", outcome, got)
		}
	}
	// Only ACCEPTED carries an ID and the held flag.
	frame := submissionAck(subA, smtpintake.Result{Outcome: smtpintake.Backlog, MessageID: "x", Held: true})
	if ack := frame.GetSubmissionAck(); ack.MessageId != "" || ack.Held {
		t.Fatalf("%v", ack)
	}
}

func TestSubmissionRefusedStartsDoNotEndTheSession(t *testing.T) {
	sub := newFakeSubmission()
	h := startSubmissionSession(t, sub, true, nil)
	digest := sha256.Sum256([]byte("x"))
	// A malformed credential: CREDENTIAL_INVALID, chunks swallowed.
	h.in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Submission{Submission: &controlv1.SubmissionStart{SubmissionId: subA, ExpectedSize: 1, Sha256: digest[:], CredentialId: "short", Secret: "x", RcptTo: []string{"u@example.com"}}}}
	h.chunk(subA, 0, []byte("x"), true)
	if ack := h.nextAck(t); ack.SubmissionId != subA || ack.Verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_CREDENTIAL_INVALID {
		t.Fatalf("%v", ack)
	}
	// A bad envelope: MESSAGE_INVALID.
	h.in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Submission{Submission: &controlv1.SubmissionStart{SubmissionId: subB, ExpectedSize: 1, Sha256: digest[:], CredentialId: subCred, Secret: subSecret}}}
	h.chunk(subB, 0, []byte("x"), true)
	if ack := h.nextAck(t); ack.SubmissionId != subB || ack.Verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_MESSAGE_INVALID {
		t.Fatalf("%v", ack)
	}
	if len(sub.calls) != 0 || h.runner.submissionBudget().inUse() != 0 {
		t.Fatal("a refused start reached the intake or held bytes")
	}
	h.alive(t)
	// Framing violations do end it.
	h.in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Submission{Submission: &controlv1.SubmissionStart{SubmissionId: "bad", ExpectedSize: 1, Sha256: digest[:]}}}
	if err := h.ended(t); err == nil {
		t.Fatal("no error")
	}
}

func TestSubmissionOverBudgetStartIsTempfailed(t *testing.T) {
	sub := newFakeSubmission()
	body := []byte(strings.Repeat("x", 1000))
	weight := edgeprotocol.SubmissionWeight(uint64(len(body)))
	h := startSubmissionSession(t, sub, true, func(r *Runner) { r.SubmissionBudget = weight })
	h.send(t, subA, body)
	<-sub.started
	if got := h.runner.submissionBudget().inUse(); got != weight {
		t.Fatalf("in use %d want %d", got, weight)
	}
	// The budget is full: the next one is refused at once, its chunks swallowed,
	// and it never reaches the intake.
	h.send(t, subB, body)
	if ack := h.nextAck(t); ack.SubmissionId != subB || ack.Verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL {
		t.Fatalf("%v", ack)
	}
	if len(sub.started) != 0 {
		t.Fatal("the over-budget message reached the intake")
	}
	h.alive(t)
	sub.release <- smtpintake.Result{Outcome: smtpintake.Accepted, MessageID: "0123456789abcdef0123456789abcdef"}
	if ack := h.nextAck(t); ack.SubmissionId != subA || ack.Verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED {
		t.Fatalf("%v", ack)
	}
	eventually(t, "the reservation to be released", func() bool { return h.runner.submissionBudget().inUse() == 0 })
	// With the budget free again, it is served.
	h.send(t, subB, body)
	<-sub.started
}

func TestSubmissionStartWithoutChunksReservesButAllocatesNothing(t *testing.T) {
	// The reservation is taken at Start; the buffer is allocated only on the
	// first chunk (asserted on the assembler in edgeprotocol). Here: the
	// reservation is released when the abandoned partial expires.
	sub := newFakeSubmission()
	h := startSubmissionSession(t, sub, true, func(r *Runner) { r.SubmissionPartialTTL = 100 * time.Millisecond })
	h.start(subA, []byte(strings.Repeat("x", 4096)))
	eventually(t, "the reservation", func() bool { return h.runner.submissionBudget().inUse() > 0 })
	eventually(t, "the partial to expire", func() bool { return h.runner.submissionBudget().inUse() == 0 })
	if len(sub.calls) != 0 {
		t.Fatal("reached the intake")
	}
	h.alive(t)
}

func TestSubmissionReservationIsHeldUntilTheWorkerExits(t *testing.T) {
	sub := newFakeSubmission()
	sub.unwind = make(chan struct{})
	body := []byte("hello")
	weight := edgeprotocol.SubmissionWeight(uint64(len(body)))
	h := startSubmissionSession(t, sub, true, nil)
	h.send(t, subA, body)
	<-sub.started
	// The session ends while the worker is still running.
	h.cancel()
	h.ended(t)
	time.Sleep(50 * time.Millisecond)
	if got := h.runner.submissionBudget().inUse(); got != weight {
		t.Fatalf("the reservation was released before the worker exited: %d", got)
	}
	close(sub.unwind)
	eventually(t, "the worker's release", func() bool { return h.runner.submissionBudget().inUse() == 0 })
}

func TestSubmissionReconnectWhileTheOldWorkerUnwinds(t *testing.T) {
	sub := newFakeSubmission()
	sub.unwind = make(chan struct{})
	body := []byte("hello")
	weight := edgeprotocol.SubmissionWeight(uint64(len(body)))
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	runner.Submission, runner.SubmissionBudget = sub, weight
	h := startSubmissionRunner(t, runner, out, in, true, nil)
	h.send(t, subA, body)
	<-sub.started
	h.cancel()
	h.ended(t)
	// The same Runner reconnects. The old worker's bytes are still counted, so
	// the new session's message is a tempfail (not a second full budget).
	h2 := startSubmissionRunner(t, runner, out, in, true, nil)
	h2.send(t, subB, body)
	if ack := h2.nextAck(t); ack.SubmissionId != subB || ack.Verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL {
		t.Fatalf("%v", ack)
	}
	close(sub.unwind)
	eventually(t, "the old worker to exit", func() bool { return runner.submissionBudget().inUse() == 0 })
	h2.send(t, "cccccccccccccccccccccccccccccccc", body)
	<-sub.started
	h2.alive(t)
}

func TestSubmissionOwnershipTransfersExactlyOnce(t *testing.T) {
	sub := newFakeSubmission()
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	runner.Submission = sub
	h := startSubmissionRunner(t, runner, out, in, true, nil)
	const n = 12
	for i := 0; i < n; i++ {
		go func(i int) { h.send(t, reqID(i+1), []byte("payload")) }(i)
	}
	for i := 0; i < n; i++ {
		<-sub.started
		sub.release <- smtpintake.Result{Outcome: smtpintake.Accepted, MessageID: "0123456789abcdef0123456789abcdef"}
	}
	for i := 0; i < n; i++ {
		h.nextAck(t)
	}
	// Some more are abandoned mid-assembly and expire, and the session ends with
	// others still assembling.
	h.start(subA, []byte("ab"))
	h.start(subB, []byte("ab"))
	budget := runner.submissionBudget()
	eventually(t, "both partials to reserve", func() bool { budget.mu.Lock(); defer budget.mu.Unlock(); return budget.acquires == n+2 })
	h.cancel()
	h.ended(t)
	eventually(t, "every reservation to be released", func() bool { return budget.inUse() == 0 })
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.acquires != n+2 || budget.releases != budget.acquires {
		t.Fatalf("acquires=%d releases=%d", budget.acquires, budget.releases)
	}
}

func TestSubmissionStragglerCap(t *testing.T) {
	sub := newFakeSubmission()
	h := startSubmissionSession(t, sub, true, func(r *Runner) { r.SubmissionPartialTTL = 40 * time.Millisecond })
	// Abandon 70 items in waves of 16 (the in-flight cap), letting each wave
	// expire into the straggler table.
	id := 1
	for wave := 0; wave < 5; wave++ {
		for i := 0; i < 14 && id <= 70; i++ {
			h.start(reqID(id), []byte("ab"))
			id++
		}
		budget := h.runner.submissionBudget()
		want := int64(id - 1)
		eventually(t, "the wave to expire", func() bool {
			budget.mu.Lock()
			defer budget.mu.Unlock()
			return budget.acquires == want && budget.used == 0
		})
		time.Sleep(60 * time.Millisecond)
	}
	// The newest straggler is still remembered and its late chunk is ignored.
	h.chunk(reqID(id-1), 0, []byte("a"), false)
	h.alive(t)
	// Six of the first wave (the oldest) were evicted; a chunk of any of them
	// ends the session, while the others are still ignored.
	for i := 1; i <= 14; i++ {
		h.chunk(reqID(i), 0, []byte("a"), false)
	}
	err := h.ended(t)
	if err == nil || !strings.Contains(err.Error(), "straggler overflow") {
		t.Fatalf("%v", err)
	}
}

func TestSubmissionUnknownChunkEndsTheSession(t *testing.T) {
	h := startSubmissionSession(t, newFakeSubmission(), true, nil)
	h.chunk(subA, 0, []byte("a"), false)
	if err := h.ended(t); err == nil || !strings.Contains(err.Error(), "unexpected submission chunk") {
		t.Fatalf("%v", err)
	}
}

func TestSubmissionMoreThanTheItemCapIsTempfailed(t *testing.T) {
	sub := newFakeSubmission()
	h := startSubmissionSession(t, sub, true, nil)
	for i := 1; i <= edgeprotocol.MaxSubmissionItems; i++ {
		h.send(t, reqID(i), []byte("x"))
	}
	for i := 0; i < edgeprotocol.MaxSubmissionItems; i++ {
		<-sub.started
	}
	extra := reqID(99)
	h.send(t, extra, []byte("x"))
	if ack := h.nextAck(t); ack.SubmissionId != extra || ack.Verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL {
		t.Fatalf("%v", ack)
	}
	h.alive(t)
}

func TestSubmissionDrainWaitsForBothLanes(t *testing.T) {
	sub := newFakeSubmission()
	sub.verifyBlock = make(chan struct{})
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	runner.Submission = sub
	drain := make(chan struct{})
	h := startSubmissionRunner(t, runner, out, in, true, drain)
	// One message committing and one credential check pending.
	h.send(t, subA, []byte("x"))
	<-sub.started
	h.auth(subB)
	time.Sleep(50 * time.Millisecond)
	close(drain)
	select {
	case err := <-h.done:
		t.Fatalf("drained with work in flight: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// Finishing only the commit is not enough.
	sub.release <- smtpintake.Result{Outcome: smtpintake.Accepted, MessageID: "0123456789abcdef0123456789abcdef"}
	if ack := h.nextAck(t); ack.Verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED {
		t.Fatal(ack)
	}
	select {
	case err := <-h.done:
		t.Fatalf("drained with a credential check pending: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(sub.verifyBlock)
	if got := h.nextAuth(t); got.RequestId != subB {
		t.Fatal(got)
	}
	select {
	case err := <-h.done:
		if !errors.Is(err, ErrDraining) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the drain did not finish")
	}
}
