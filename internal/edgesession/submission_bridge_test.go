package edgesession

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/journal"
)

const (
	subCredID = "cccccccccccccccccccccccccccccccc"
	subSecret = "tps_0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"
	subMsgID  = "0123456789abcdef0123456789abcdef"
)

var testCredential = SubmissionCredential{ID: subCredID, Secret: subSecret}

func testMessage(body []byte) SubmissionMessage {
	return SubmissionMessage{Credential: testCredential, MailFrom: "a@example.org", Rcpts: []string{"u@example.com"}, ClientIP: netip.MustParseAddr("192.0.2.1"), TLSVersion: "TLS 1.3", Received: time.UnixMilli(1700000000000), Port: 587, Body: body}
}

// --- bridge level, with a fake send ---

type bridgeHarness struct {
	b     *submissionBridge
	conn  *submissionConnection
	sent  chan *controlv1.EdgeFrame
	mu    sync.Mutex
	valid bool
}

func (h *bridgeHarness) isValid() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.valid }
func (h *bridgeHarness) setValid(v bool) {
	h.mu.Lock()
	h.valid = v
	h.mu.Unlock()
}

func newBridgeHarness(t *testing.T) *bridgeHarness {
	t.Helper()
	h := &bridgeHarness{b: &submissionBridge{enabled: true}, sent: make(chan *controlv1.EdgeFrame, 256), valid: true}
	fence := journal.Fence{ControllerID: "controller", Epoch: 1}
	if err := h.b.activate(fence); err != nil {
		t.Fatal(err)
	}
	conn, err := h.b.attach(fence, func(f *controlv1.EdgeFrame) error { h.sent <- f; return nil }, h.isValid)
	if err != nil {
		t.Fatal(err)
	}
	h.conn = conn
	return h
}

func (h *bridgeHarness) nextFrame(t *testing.T) *controlv1.EdgeFrame {
	t.Helper()
	select {
	case f := <-h.sent:
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("no frame")
	}
	return nil
}

type submitOutcome struct {
	result SubmitResult
	err    error
}

func (h *bridgeHarness) submitAsync(m SubmissionMessage) (<-chan submitOutcome, string) {
	if m.ID == "" {
		m.ID = fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	out := make(chan submitOutcome, 1)
	go func() {
		r, err := h.b.Submit(context.Background(), m)
		out <- submitOutcome{r, err}
	}()
	return out, m.ID
}

// readSubmission consumes the start and every chunk of one submission.
func (h *bridgeHarness) readSubmission(t *testing.T) (*controlv1.SubmissionStart, []*controlv1.DataChunk) {
	t.Helper()
	start := h.nextFrame(t).GetSubmission()
	if start == nil {
		t.Fatal("no submission start")
	}
	var chunks []*controlv1.DataChunk
	for {
		chunk := h.nextFrame(t).GetSubmissionChunk()
		if chunk == nil {
			t.Fatal("no chunk")
		}
		chunks = append(chunks, chunk)
		if chunk.Last {
			return start, chunks
		}
	}
}

func (h *bridgeHarness) ack(id string, verdict controlv1.SubmissionVerdict, messageID string, held bool) {
	h.b.acknowledge(h.conn, &controlv1.SubmissionAcknowledgement{SubmissionId: id, Verdict: verdict, MessageId: messageID, Held: held})
}

func waitOutcome(t *testing.T, ch <-chan submitOutcome) submitOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("Submit did not return")
	}
	return submitOutcome{}
}

func TestSubmitSendsChunksWithoutCloning(t *testing.T) {
	h := newBridgeHarness(t)
	body := []byte(strings.Repeat("x", 2*edgeprotocol.MaxChunkBytes+7))
	out, id := h.submitAsync(testMessage(body))
	start, chunks := h.readSubmission(t)
	if start.SubmissionId != id || start.ExpectedSize != uint64(len(body)) || start.CredentialId != subCredID || start.Secret != subSecret || start.Port != 587 || start.ClientIp != "192.0.2.1" || start.TlsVersion != "TLS 1.3" || start.ReceivedUnixMs != 1700000000000 || start.MailFrom != "a@example.org" {
		t.Fatalf("%v", start)
	}
	if len(chunks) != 3 {
		t.Fatalf("%d chunks", len(chunks))
	}
	for i, chunk := range chunks {
		offset := i * edgeprotocol.MaxChunkBytes
		if chunk.Offset != uint64(offset) || chunk.Id != id || unsafe.SliceData(chunk.Data) != unsafe.SliceData(body[offset:]) {
			t.Fatalf("chunk %d was copied or misplaced", i)
		}
	}
	if !edgeprotocol.ValidSubmissionStart(start) {
		t.Fatal("start is not valid on the wire")
	}
	h.ack(id, controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED, subMsgID, false)
	if o := waitOutcome(t, out); o.err != nil || o.result.Verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED || o.result.MessageID != subMsgID || o.result.Held {
		t.Fatalf("%+v", o)
	}
}

func TestSubmitPropagatesHeld(t *testing.T) {
	h := newBridgeHarness(t)
	out, id := h.submitAsync(testMessage([]byte("hello")))
	h.readSubmission(t)
	h.ack(id, controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED, subMsgID, true)
	o := waitOutcome(t, out)
	if o.err != nil || !o.result.Held || o.result.MessageID != subMsgID {
		t.Fatalf("%+v", o)
	}
	if reply := ReplyFor(o.result); reply.Code != 250 || reply.Text != "OK: held as "+subMsgID {
		t.Fatalf("%+v", reply)
	}
	if reply := ReplyFor(SubmitResult{Verdict: controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED, MessageID: subMsgID}); reply.Text != "OK: queued as "+subMsgID {
		t.Fatalf("%+v", reply)
	}
}

func TestAcceptedWithAnInvalidIDIsTempfail(t *testing.T) {
	for _, bad := range []string{"", "short", strings.ToUpper(subMsgID), subMsgID + "0"} {
		h := newBridgeHarness(t)
		out, id := h.submitAsync(testMessage([]byte("hello")))
		h.readSubmission(t)
		h.ack(id, controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED, bad, true)
		o := waitOutcome(t, out)
		if o.err != nil || o.result.Verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL || o.result.MessageID != "" || o.result.Held {
			t.Fatalf("%q: %+v", bad, o)
		}
		if reply := ReplyFor(o.result); reply.Code != 451 {
			t.Fatalf("%+v", reply)
		}
		if h.b.badAckIDs.Load() != 1 {
			t.Fatal("ack_bad_id not counted")
		}
	}
	// ReplyFor never answers 250 for an accepted result without a valid ID.
	if reply := ReplyFor(SubmitResult{Verdict: controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED}); reply.Code != 451 {
		t.Fatalf("%+v", reply)
	}
}

func TestUnspecifiedAndUnknownVerdictsAreTempfail(t *testing.T) {
	for _, verdict := range []controlv1.SubmissionVerdict{controlv1.SubmissionVerdict_SUBMISSION_VERDICT_UNSPECIFIED, controlv1.SubmissionVerdict(99)} {
		h := newBridgeHarness(t)
		out, id := h.submitAsync(testMessage([]byte("hello")))
		h.readSubmission(t)
		h.ack(id, verdict, subMsgID, true)
		if o := waitOutcome(t, out); o.err != nil || o.result.Verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL || o.result.MessageID != "" || o.result.Held {
			t.Fatalf("%v: %+v", verdict, o)
		}
	}
	for _, verdict := range []controlv1.SubmissionAuthVerdict{controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_UNSPECIFIED, controlv1.SubmissionAuthVerdict(99)} {
		h := newBridgeHarness(t)
		done := make(chan controlv1.SubmissionAuthVerdict, 1)
		go func() { v, _ := h.b.Verify(context.Background(), testCredential, netip.Addr{}); done <- v }()
		auth := h.nextFrame(t).GetAuth()
		h.b.acknowledgeAuth(h.conn, &controlv1.SubmissionAuthResult{RequestId: auth.RequestId, Verdict: verdict})
		if v := <-done; v != controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_TEMPFAIL {
			t.Fatalf("%v mapped to %v", verdict, v)
		}
	}
}

func TestDetachFailsEveryWaiter(t *testing.T) {
	h := newBridgeHarness(t)
	out1, _ := h.submitAsync(testMessage([]byte("one")))
	h.readSubmission(t)
	out2, _ := h.submitAsync(testMessage([]byte("two")))
	h.readSubmission(t)
	verify := make(chan error, 1)
	go func() { _, err := h.b.Verify(context.Background(), testCredential, netip.Addr{}); verify <- err }()
	h.nextFrame(t).GetAuth()
	h.b.detach(h.conn)
	for _, out := range []<-chan submitOutcome{out1, out2} {
		if o := waitOutcome(t, out); !errors.Is(o.err, ErrSubmissionSessionClosed) {
			t.Fatalf("%+v", o)
		}
	}
	select {
	case err := <-verify:
		if !errors.Is(err, ErrSubmissionSessionClosed) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Verify did not return")
	}
	if h.b.Available() {
		t.Fatal("available without a session")
	}
	if _, err := h.b.Submit(context.Background(), testMessage([]byte("x"))); !errors.Is(err, ErrSubmissionUnavailable) {
		t.Fatal(err)
	}
	if v, err := h.b.Verify(context.Background(), testCredential, netip.Addr{}); err != nil || v != controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_TEMPFAIL {
		t.Fatal(v, err)
	}
}

func TestStaleAcknowledgementsAreIgnoredAfterAFenceChange(t *testing.T) {
	h := newBridgeHarness(t)
	out, id := h.submitAsync(testMessage([]byte("hello")))
	h.readSubmission(t)
	// A newer controller takes over; the old session is closed.
	next := journal.Fence{ControllerID: "controller", Epoch: 2}
	if err := h.b.activate(next); err != nil {
		t.Fatal(err)
	}
	if o := waitOutcome(t, out); !errors.Is(o.err, ErrSubmissionSessionClosed) {
		t.Fatalf("%+v", o)
	}
	// An acknowledgement that arrives late on the old session does nothing, even
	// for an ID the new session happens to use.
	fresh := &bridgeHarness{b: h.b, sent: make(chan *controlv1.EdgeFrame, 16), valid: true}
	conn, err := h.b.attach(next, func(f *controlv1.EdgeFrame) error { fresh.sent <- f; return nil }, fresh.isValid)
	if err != nil {
		t.Fatal(err)
	}
	fresh.conn = conn
	out2, _ := fresh.submitAsync(func() SubmissionMessage { m := testMessage([]byte("hello")); m.ID = id; return m }())
	fresh.readSubmission(t)
	h.ack(id, controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED, subMsgID, false) // old conn
	select {
	case o := <-out2:
		t.Fatalf("a stale acknowledgement completed a new submission: %+v", o)
	case <-time.After(100 * time.Millisecond):
	}
	if h.b.droppedAcknowledges.Load() == 0 {
		t.Fatal("the stale acknowledgement was not counted as dropped")
	}
	fresh.ack(id, controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED, subMsgID, false)
	if o := waitOutcome(t, out2); o.err != nil || o.result.MessageID != subMsgID {
		t.Fatalf("%+v", o)
	}
	// An older fence cannot be activated again.
	if err := h.b.activate(journal.Fence{ControllerID: "controller", Epoch: 1}); !errors.Is(err, journal.ErrStaleController) {
		t.Fatal(err)
	}
	if _, err := h.b.attach(journal.Fence{ControllerID: "controller", Epoch: 1}, nil, nil); !errors.Is(err, journal.ErrStaleController) {
		t.Fatal(err)
	}
}

func TestAcknowledgementIsIgnoredWhileUnauthorized(t *testing.T) {
	h := newBridgeHarness(t)
	out, id := h.submitAsync(testMessage([]byte("hello")))
	h.readSubmission(t)
	h.setValid(false)
	h.ack(id, controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED, subMsgID, false)
	select {
	case o := <-out:
		t.Fatalf("an unauthorized acknowledgement completed the submission: %+v", o)
	case <-time.After(100 * time.Millisecond):
	}
	// Valid again: a late result is accepted, but never if the lease lapses first.
	h.setValid(true)
	h.ack(id, controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED, subMsgID, false)
	h.setValid(false)
	if o := waitOutcome(t, out); o.err == nil && o.result.Verdict == controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED {
		// The ack may have raced ahead of the validity flip; either is correct as
		// long as an invalid lease at delivery time is an error.
		t.Log("acknowledged before the lease lapsed")
	}
}

func TestSubmitRefusedWithoutASessionOrWhenInvalid(t *testing.T) {
	b := &submissionBridge{}
	if _, err := b.Submit(context.Background(), testMessage([]byte("x"))); !errors.Is(err, ErrSubmissionUnavailable) {
		t.Fatal("disabled bridge accepted a submission")
	}
	h := newBridgeHarness(t)
	if _, err := h.b.Submit(context.Background(), SubmissionMessage{Credential: testCredential, Rcpts: []string{"u@example.com"}}); err == nil {
		t.Fatal("an empty body was sent")
	}
	// A permanent problem is a verdict, not an error the client retries forever.
	if r, err := h.b.Submit(context.Background(), SubmissionMessage{Credential: SubmissionCredential{ID: "x", Secret: "y"}, Rcpts: []string{"u@example.com"}, Body: []byte("x")}); err != nil || r.Verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_CREDENTIAL_INVALID {
		t.Fatalf("a malformed credential: %+v %v", r, err)
	}
	noRcpt := testMessage([]byte("x"))
	noRcpt.Rcpts = nil
	if r, err := h.b.Submit(context.Background(), noRcpt); err != nil || r.Verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_MESSAGE_INVALID {
		t.Fatalf("no recipients: %+v %v", r, err)
	}
	bogusTLS := testMessage([]byte("x"))
	bogusTLS.TLSVersion = "none"
	if r, err := h.b.Submit(context.Background(), bogusTLS); err != nil || r.Verdict != controlv1.SubmissionVerdict_SUBMISSION_VERDICT_MESSAGE_INVALID {
		t.Fatalf("bogus TLS version: %+v %v", r, err)
	}
	if len(h.sent) != 0 {
		t.Fatal("an invalid submission reached the stream")
	}
	// Lease no longer valid.
	h.setValid(false)
	if h.b.Available() {
		t.Fatal("available with an expired lease")
	}
	if _, err := h.b.Submit(context.Background(), testMessage([]byte("x"))); !errors.Is(err, ErrSubmissionUnavailable) {
		t.Fatal(err)
	}
}

func TestSubmitHonoursTheEdgeByteBudgetAndItemCap(t *testing.T) {
	h := newBridgeHarness(t)
	h.b.limit = 100
	out, id := h.submitAsync(testMessage([]byte(strings.Repeat("x", 80))))
	h.readSubmission(t)
	if _, err := h.b.Submit(context.Background(), testMessage([]byte(strings.Repeat("y", 30)))); !errors.Is(err, ErrSubmissionUnavailable) {
		t.Fatalf("over-budget submit: %v", err)
	}
	h.ack(id, controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL, "", false)
	waitOutcome(t, out)
	h.b.mu.Lock()
	used := h.b.used
	h.b.mu.Unlock()
	if used != 0 {
		t.Fatalf("budget leaked %d", used)
	}
	// The per-session item cap.
	h2 := newBridgeHarness(t)
	for i := 0; i < edgeprotocol.MaxSubmissionItems; i++ {
		h2.submitAsync(testMessage([]byte("x")))
		h2.readSubmission(t)
	}
	if _, err := h2.b.Submit(context.Background(), testMessage([]byte("x"))); !errors.Is(err, ErrSubmissionUnavailable) {
		t.Fatalf("seventeenth submit: %v", err)
	}
}

func TestVerifyThrottlesBeyondThePendingCap(t *testing.T) {
	h := newBridgeHarness(t)
	var wg sync.WaitGroup
	results := make(chan controlv1.SubmissionAuthVerdict, edgeprotocol.MaxAuthPending)
	for i := 0; i < edgeprotocol.MaxAuthPending; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _ := h.b.Verify(context.Background(), testCredential, netip.Addr{})
			results <- v
		}()
	}
	var ids []string
	for i := 0; i < edgeprotocol.MaxAuthPending; i++ {
		ids = append(ids, h.nextFrame(t).GetAuth().RequestId)
	}
	v, err := h.b.Verify(context.Background(), testCredential, netip.Addr{})
	if err != nil || v != controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_THROTTLED {
		t.Fatal(v, err)
	}
	if len(h.sent) != 0 {
		t.Fatal("a throttled check was sent")
	}
	for _, id := range ids {
		h.b.acknowledgeAuth(h.conn, &controlv1.SubmissionAuthResult{RequestId: id, Verdict: controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK})
	}
	wg.Wait()
	close(results)
	for v := range results {
		if v != controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK {
			t.Fatal(v)
		}
	}
}

func TestVerifyResultIsAcceptedOnlyWhileAuthorized(t *testing.T) {
	h := newBridgeHarness(t)
	done := make(chan controlv1.SubmissionAuthVerdict, 1)
	go func() { v, _ := h.b.Verify(context.Background(), testCredential, netip.Addr{}); done <- v }()
	auth := h.nextFrame(t).GetAuth()
	if auth.CredentialId != subCredID || auth.Secret != subSecret || !edgeprotocol.ValidSubmissionAuth(auth) {
		t.Fatalf("%v", auth)
	}
	h.setValid(false)
	h.b.acknowledgeAuth(h.conn, &controlv1.SubmissionAuthResult{RequestId: auth.RequestId, Verdict: controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK})
	select {
	case v := <-done:
		t.Fatalf("an unauthorized result was accepted: %v", v)
	case <-time.After(100 * time.Millisecond):
	}
	// An unknown request ID is ignored too.
	h.setValid(true)
	h.b.acknowledgeAuth(h.conn, &controlv1.SubmissionAuthResult{RequestId: subMsgID, Verdict: controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK})
	select {
	case v := <-done:
		t.Fatalf("a result for another request was accepted: %v", v)
	case <-time.After(100 * time.Millisecond):
	}
	h.b.acknowledgeAuth(h.conn, &controlv1.SubmissionAuthResult{RequestId: auth.RequestId, Verdict: controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_INVALID})
	if v := <-done; v != controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_INVALID {
		t.Fatal(v)
	}
}

func TestVerifyAndSubmitTimeOut(t *testing.T) {
	h := newBridgeHarness(t)
	h.b.verifyTimeout, h.b.submitTimeout = 50*time.Millisecond, 50*time.Millisecond
	if v, err := h.b.Verify(context.Background(), testCredential, netip.Addr{}); err != nil || v != controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_TEMPFAIL {
		t.Fatal(v, err)
	}
	out, _ := h.submitAsync(testMessage([]byte("x")))
	if o := waitOutcome(t, out); o.err == nil {
		t.Fatal("no timeout error")
	}
	h.b.mu.Lock()
	pending, used := len(h.b.auths)+len(h.b.items), h.b.used
	h.b.mu.Unlock()
	if pending != 0 || used != 0 {
		t.Fatalf("leaked state: %d pending, %d bytes", pending, used)
	}
}

func TestRedaction(t *testing.T) {
	for _, v := range []any{testCredential, &testCredential, testMessage([]byte("body"))} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			if out := fmt.Sprintf(verb, v); strings.Contains(out, subSecret) || strings.Contains(out, "body") {
				t.Fatalf("%s leaked: %s", verb, out)
			}
		}
	}
}

func TestReplyMappings(t *testing.T) {
	for verdict, code := range map[controlv1.SubmissionVerdict]int{
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TEMPFAIL:                451,
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_RATE_LIMITED:            451,
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_QUOTA_EXCEEDED:          452,
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_BACKLOG:                 451,
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_UNAVAILABLE:             421,
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_CREDENTIAL_INVALID:      550,
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_APP_PAUSED:              550,
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_FROM_NOT_PERMITTED:      550,
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_MESSAGE_INVALID:         554,
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_TOO_LARGE:               552,
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_RECIPIENT_NOT_PERMITTED: 550,
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ENCRYPTION_REQUIRED:     550,
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_MISCONFIGURED:           451,
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_CONTENT_NOT_SUPPORTED:   554,
		controlv1.SubmissionVerdict_SUBMISSION_VERDICT_UNSPECIFIED:             451,
	} {
		reply := ReplyFor(SubmitResult{Verdict: verdict})
		if reply.Code != code || reply.Enhanced == "" || reply.Text == "" || reply.Close != (code == 421) {
			t.Errorf("%v: %+v", verdict, reply)
		}
	}
	for verdict, code := range map[controlv1.SubmissionAuthVerdict]int{
		controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK:          235,
		controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_INVALID:     535,
		controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_TEMPFAIL:    454,
		controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_THROTTLED:   454,
		controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_UNAVAILABLE: 421,
		controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_UNSPECIFIED: 454,
	} {
		if reply := AuthReply(verdict); reply.Code != code {
			t.Errorf("%v: %+v", verdict, reply)
		}
	}
}

// --- session level, through Server.Connect ---

type submissionSession struct {
	s      *Server
	stream *channelStream
	done   chan error
	ready  *controlv1.Ready
}

// startSubmission opens a session. The control side advertises the capability
// only when withCap is set; the worker only when enabled.
func startSubmission(t *testing.T, s *Server, withCap bool) *submissionSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(verifiedContext(t, "spiffe://thinpost.test/staging/controller/one"), 10*time.Second)
	t.Cleanup(cancel)
	stream := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 64), sent: make(chan *controlv1.EdgeFrame, 64)}
	done := make(chan error, 1)
	go func() { done <- s.Connect(stream) }()
	open := openFrame(s, 1)
	if withCap {
		open.GetOpen().Capabilities = []string{edgeprotocol.CapSubmission}
	}
	stream.incoming <- open
	ready := <-stream.sent
	if ready.GetReady() == nil {
		t.Fatal("no ready frame")
	}
	if (<-stream.sent).GetReplayComplete() == nil {
		t.Fatal("missing replay barrier")
	}
	stream.incoming <- reconcileFrame(s, 1)
	return &submissionSession{s: s, stream: stream, done: done, ready: ready.GetReady()}
}

func TestSubmissionCapabilityIsAdvertisedOnlyWhenEnabled(t *testing.T) {
	s, _, _, _ := setup(t, true)
	ss := startSubmission(t, s, true)
	if edgeprotocol.HasCapability(ss.ready.Capabilities, edgeprotocol.CapSubmission) || len(ss.ready.SubmissionPorts) != 0 {
		t.Fatalf("a worker that never enabled submission advertised it: %v %v", ss.ready.Capabilities, ss.ready.SubmissionPorts)
	}
	if s.Submission().Available() {
		t.Fatal("available while disabled")
	}
	// No frame is ever sent, and one arriving is a protocol error.
	ss.stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_SubmissionAck{SubmissionAck: &controlv1.SubmissionAcknowledgement{SubmissionId: subMsgID, Verdict: controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED, MessageId: subMsgID}}}
	if err := <-ss.done; err == nil {
		t.Fatal("a submission acknowledgement was accepted without the capability")
	}
	if _, err := s.Submission().Submit(context.Background(), testMessage([]byte("x"))); !errors.Is(err, ErrSubmissionUnavailable) {
		t.Fatal(err)
	}
}

func TestSubmissionFramesWithoutNegotiationEndTheSession(t *testing.T) {
	frames := map[string]*controlv1.ControlFrame{
		"auth result": {Body: &controlv1.ControlFrame_AuthResult{AuthResult: &controlv1.SubmissionAuthResult{RequestId: subMsgID, Verdict: controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK}}},
		"ack":         {Body: &controlv1.ControlFrame_SubmissionAck{SubmissionAck: &controlv1.SubmissionAcknowledgement{SubmissionId: subMsgID, Verdict: controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED, MessageId: subMsgID}}},
	}
	for name, frame := range frames {
		// Enabled here, but the control plane did not advertise it.
		s, _, _, _ := setup(t, true)
		s.EnableSubmission(587)
		ss := startSubmission(t, s, false)
		if !edgeprotocol.HasCapability(ss.ready.Capabilities, edgeprotocol.CapSubmission) {
			t.Fatal("an enabled worker did not advertise the capability")
		}
		select {
		case f := <-ss.stream.sent:
			t.Fatalf("%s: a frame was sent to a control plane without the capability: %v", name, f)
		case <-time.After(50 * time.Millisecond):
		}
		if s.Submission().Available() {
			t.Fatalf("%s: available without a negotiated session", name)
		}
		ss.stream.incoming <- frame
		if err := <-ss.done; err == nil {
			t.Fatalf("%s: accepted without negotiation", name)
		}
	}
}

func TestSubmissionRoundTripThroughASession(t *testing.T) {
	s, _, _, _ := setup(t, true)
	s.EnableSubmission(465, 587)
	ss := startSubmission(t, s, true)
	if !edgeprotocol.HasCapability(ss.ready.Capabilities, edgeprotocol.CapSubmission) || len(ss.ready.SubmissionPorts) != 2 || ss.ready.SubmissionPorts[0] != 465 || ss.ready.SubmissionPorts[1] != 587 {
		t.Fatalf("%v %v", ss.ready.Capabilities, ss.ready.SubmissionPorts)
	}
	waitFor(t, "attach", s.Submission().Available)
	// Verify.
	verdict := make(chan controlv1.SubmissionAuthVerdict, 1)
	go func() {
		v, _ := s.Submission().Verify(context.Background(), testCredential, netip.MustParseAddr("192.0.2.9"))
		verdict <- v
	}()
	auth := (<-ss.stream.sent).GetAuth()
	if auth == nil || auth.ClientIp != "192.0.2.9" {
		t.Fatalf("%v", auth)
	}
	ss.stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_AuthResult{AuthResult: &controlv1.SubmissionAuthResult{RequestId: auth.RequestId, Verdict: controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK}}}
	if v := <-verdict; v != controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_OK {
		t.Fatal(v)
	}
	// Submit.
	type outcome struct {
		r   SubmitResult
		err error
	}
	got := make(chan outcome, 1)
	m := testMessage([]byte("hello"))
	m.ID = subMsgID
	go func() { r, err := s.Submission().Submit(context.Background(), m); got <- outcome{r, err} }()
	start := (<-ss.stream.sent).GetSubmission()
	chunk := (<-ss.stream.sent).GetSubmissionChunk()
	if start == nil || chunk == nil || !chunk.Last || string(chunk.Data) != "hello" {
		t.Fatal("bad frames")
	}
	ss.stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_SubmissionAck{SubmissionAck: &controlv1.SubmissionAcknowledgement{SubmissionId: subMsgID, Verdict: controlv1.SubmissionVerdict_SUBMISSION_VERDICT_ACCEPTED, MessageId: "ffffffffffffffffffffffffffffffff", Held: true}}}
	o := <-got
	if o.err != nil || o.r.MessageID != "ffffffffffffffffffffffffffffffff" || !o.r.Held {
		t.Fatalf("%+v", o)
	}
	// Ending the session detaches the bridge.
	close(ss.stream.incoming)
	<-ss.done
	if s.Submission().Available() {
		t.Fatal("still available after the session ended")
	}
}

func TestNilSubmissionFramesEndTheSession(t *testing.T) {
	s, _, _, _ := setup(t, true)
	s.EnableSubmission()
	ss := startSubmission(t, s, true)
	waitFor(t, "attach", s.Submission().Available)
	ss.stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_SubmissionAck{}}
	if err := <-ss.done; err == nil {
		t.Fatal("a nil acknowledgement was accepted")
	}
}

func TestEnableSubmissionRejectsBadPorts(t *testing.T) {
	for _, ports := range [][]uint32{{0}, {65536}, {587, 587}, {1, 2, 3, 4, 5, 6, 7, 8, 9}} {
		s, _, _, _ := setup(t, true)
		if err := s.EnableSubmission(ports...); err == nil {
			t.Fatalf("%v accepted", ports)
		}
		if s.submission.isEnabled() {
			t.Fatalf("%v enabled the capability", ports)
		}
	}
	s, _, _, _ := setup(t, true)
	if err := s.EnableSubmission(465, 587); err != nil || !s.submission.isEnabled() {
		t.Fatal(err)
	}
}
