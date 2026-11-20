package edgesession

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/edgeworker"
	"github.com/zyno-io/thinpost/internal/journal"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"
)

type grant bool

func (g grant) Valid() bool         { return bool(g) }
func (g grant) Deadline() time.Time { return time.Now().Add(time.Hour) }

type mutableGrant struct{ valid atomic.Bool }

func (g *mutableGrant) Valid() bool         { return g.valid.Load() }
func (g *mutableGrant) Deadline() time.Time { return time.Now().Add(time.Hour) }

type timedGrant struct{ deadline time.Time }

func (g timedGrant) Valid() bool         { return time.Now().Before(g.deadline) }
func (g timedGrant) Deadline() time.Time { return g.deadline }

type timedAuthority struct{ grant timedGrant }

func (a timedAuthority) Authorize(context.Context, string, *controlv1.Open) (Grant, error) {
	return a.grant, nil
}

type mutableAuthority struct{ grant *mutableGrant }

func (a mutableAuthority) Authorize(context.Context, string, *controlv1.Open) (Grant, error) {
	return a.grant, nil
}

type authority struct{ valid bool }

func (a authority) Authorize(context.Context, string, *controlv1.Open) (Grant, error) {
	return grant(a.valid), nil
}

type executor struct {
	j     *journal.Journal
	calls int
}

type timeoutTestPolicy struct{}

func (timeoutTestPolicy) Allows(string) bool { return true }

type timeoutTestDialer struct{ calls atomic.Int32 }

func (d *timeoutTestDialer) Dial(context.Context, netip.Addr, netip.Addr, string, string) (net.Conn, error) {
	d.calls.Add(1)
	return nil, errors.New("unexpected SMTP dial")
}

func (e *executor) Execute(ctx context.Context, f journal.Fence, start *controlv1.DeliveryStart, _ []byte, _ func() bool, permit func(context.Context, *controlv1.DeliveryStart) error) (*controlv1.DeliveryResult, error) {
	e.calls++
	if err := e.j.Begin(start.AttemptId, start.DeliveryId, f, time.Now()); err != nil {
		return nil, err
	}
	if err := permit(ctx, start); err != nil {
		return nil, err
	}
	if err := e.j.Complete(start.AttemptId, journal.Accepted, 250, time.Now()); err != nil {
		return nil, err
	}
	return &controlv1.DeliveryResult{AttemptId: start.AttemptId, DeliveryId: start.DeliveryId, Epoch: start.Epoch, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED, SmtpCode: 250}, nil
}

type testStream struct {
	ctx      context.Context
	incoming []*controlv1.ControlFrame
	sent     []*controlv1.EdgeFrame
}

type channelStream struct {
	ctx      context.Context
	incoming chan *controlv1.ControlFrame
	sent     chan *controlv1.EdgeFrame
}

type blockedSendStream struct {
	*channelStream
	started chan struct{}
	release chan struct{}
	exited  chan struct{}
}

func (s *blockedSendStream) Send(*controlv1.EdgeFrame) error {
	close(s.started)
	<-s.release
	close(s.exited)
	return io.EOF
}

func (s *channelStream) Context() context.Context { return s.ctx }
func (s *channelStream) Recv() (*controlv1.ControlFrame, error) {
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
func (s *channelStream) Send(frame *controlv1.EdgeFrame) error {
	select {
	case s.sent <- frame:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}
func (*channelStream) SetHeader(metadata.MD) error  { return nil }
func (*channelStream) SendHeader(metadata.MD) error { return nil }
func (*channelStream) SetTrailer(metadata.MD)       {}
func (*channelStream) SendMsg(any) error            { return nil }
func (*channelStream) RecvMsg(any) error            { return nil }

type delayedExecutor struct {
	wrapped          *executor
	started, release chan struct{}
}

type stuckExecutor struct{ started, release chan struct{} }

func (e *stuckExecutor) Execute(context.Context, journal.Fence, *controlv1.DeliveryStart, []byte, func() bool, func(context.Context, *controlv1.DeliveryStart) error) (*controlv1.DeliveryResult, error) {
	close(e.started)
	<-e.release
	return nil, errors.New("execution stopped")
}

func (e *delayedExecutor) Execute(ctx context.Context, f journal.Fence, start *controlv1.DeliveryStart, mime []byte, authorized func() bool, permit func(context.Context, *controlv1.DeliveryStart) error) (*controlv1.DeliveryResult, error) {
	close(e.started)
	select {
	case <-e.release:
		return e.wrapped.Execute(ctx, f, start, mime, authorized, permit)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *testStream) Context() context.Context { return s.ctx }
func (s *testStream) Recv() (*controlv1.ControlFrame, error) {
	if len(s.incoming) == 0 {
		return nil, io.EOF
	}
	frame := s.incoming[0]
	s.incoming = s.incoming[1:]
	return frame, nil
}
func (s *testStream) Send(frame *controlv1.EdgeFrame) error {
	s.sent = append(s.sent, frame)
	return nil
}
func (*testStream) SetHeader(metadata.MD) error  { return nil }
func (*testStream) SendHeader(metadata.MD) error { return nil }
func (*testStream) SetTrailer(metadata.MD)       {}
func (*testStream) SendMsg(any) error            { return nil }
func (*testStream) RecvMsg(any) error            { return nil }
func verifiedContext(t *testing.T, identity string) context.Context {
	t.Helper()
	uri, err := url.Parse(identity)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{URIs: []*url.URL{uri}}
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
}
func openFrame(s *Server, epoch uint64) *controlv1.ControlFrame {
	id, err := s.journal.Identity()
	if err != nil {
		panic(err)
	}
	return &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Open{Open: &controlv1.Open{WorkerId: "worker", ControllerId: "controller", JournalId: id, Epoch: epoch, SessionId: "0123456789abcdef0123456789abcdef", ProtocolVersion: edgeprotocol.Version}}}
}
func reconcileFrame(s *Server, epoch uint64) *controlv1.ControlFrame {
	id, err := s.journal.Identity()
	if err != nil {
		panic(err)
	}
	return &controlv1.ControlFrame{Body: &controlv1.ControlFrame_ReconciliationComplete{ReconciliationComplete: &controlv1.ReconciliationComplete{WorkerId: "worker", JournalId: id, SessionId: "0123456789abcdef0123456789abcdef", Epoch: epoch}}}
}
func ackFrame(result *controlv1.DeliveryResult) *controlv1.ControlFrame {
	return &controlv1.ControlFrame{Body: &controlv1.ControlFrame_ResultAck{ResultAck: &controlv1.ResultAcknowledgement{Result: result}}}
}
func setup(t *testing.T, valid bool) (*Server, *journal.Journal, *executor, context.Context) {
	t.Helper()
	j, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"), 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	e := &executor{j: j}
	identity := "spiffe://thinpost.test/staging/controller/one"
	s, err := New("worker", []string{identity}, j, e, authority{valid}, 2)
	if err != nil {
		t.Fatal(err)
	}
	return s, j, e, verifiedContext(t, identity)
}
func TestSessionPullsExecutesAndAcknowledges(t *testing.T) {
	s, j, e, ctx := setup(t, true)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	delayed := &delayedExecutor{wrapped: e, started: make(chan struct{}), release: make(chan struct{})}
	s.executor = delayed
	mime := []byte("signed MIME")
	digest := sha256.Sum256(mime)
	result := &controlv1.DeliveryResult{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED, SmtpCode: 250}
	stream := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 8), sent: make(chan *controlv1.EdgeFrame, 8)}
	done := make(chan error, 1)
	go func() { done <- s.Connect(stream) }()
	stream.incoming <- openFrame(s, 1)
	if (<-stream.sent).GetReady() == nil || (<-stream.sent).GetReplayComplete() == nil {
		t.Fatal("session not ready")
	}
	stream.incoming <- reconcileFrame(s, 1)
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Wake{Wake: &controlv1.Wake{}}}
	if (<-stream.sent).GetPull() == nil {
		t.Fatal("no work request")
	}
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Delivery{Delivery: &controlv1.DeliveryStart{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, ExpectedSize: uint64(len(mime)), Sha256: digest[:]}}}
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_MimeChunk{MimeChunk: &controlv1.DataChunk{Id: "attempt", Data: mime, Last: true}}}
	select {
	case <-delayed.started:
	case <-ctx.Done():
		t.Fatal("SMTP execution did not start")
	}
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_LeaseRenewal{LeaseRenewal: &controlv1.LeaseRenewal{}}}
	select {
	case frame := <-stream.sent:
		if frame.GetLeaseAccepted() == nil {
			t.Fatal("renewal not acknowledged during SMTP")
		}
	case <-ctx.Done():
		t.Fatal("renewal blocked during SMTP")
	}
	close(delayed.release)
	var requested *controlv1.StartRequest
	select {
	case frame := <-stream.sent:
		requested = frame.GetStartRequest()
		if requested == nil {
			t.Fatal("missing start request")
		}
	case <-ctx.Done():
		t.Fatal("start request missing")
	}
	items, err := j.List()
	if err != nil || len(items) != 1 || items[0].Outcome != journal.InFlight {
		t.Fatal("start requested before journal begin", err)
	}
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_StartPermit{StartPermit: &controlv1.StartPermit{Request: requested}}}
	select {
	case frame := <-stream.sent:
		if frame.GetResult() == nil {
			t.Fatal("missing delivery result")
		}
	case <-ctx.Done():
		t.Fatal("delivery result missing")
	}
	stream.incoming <- ackFrame(result)
	close(stream.incoming)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e.calls != 1 {
		t.Fatal("delivery executed more than once")
	}
	items, err = j.List()
	if err != nil || len(items) != 0 {
		t.Fatal("acknowledged result retained")
	}
}

func TestLeaseExpiryCancelsInFlightExecution(t *testing.T) {
	s, _, e, ctx := setup(t, true)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	s.authority = timedAuthority{grant: timedGrant{deadline: time.Now().Add(400 * time.Millisecond)}}
	delayed := &delayedExecutor{wrapped: e, started: make(chan struct{}), release: make(chan struct{})}
	s.executor = delayed
	stream := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 8), sent: make(chan *controlv1.EdgeFrame, 8)}
	done := make(chan error, 1)
	go func() { done <- s.Connect(stream) }()
	stream.incoming <- openFrame(s, 1)
	if (<-stream.sent).GetReady() == nil || (<-stream.sent).GetReplayComplete() == nil {
		t.Fatal("session did not become ready")
	}
	stream.incoming <- reconcileFrame(s, 1)
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Wake{Wake: &controlv1.Wake{}}}
	if (<-stream.sent).GetPull() == nil {
		t.Fatal("missing pull")
	}
	mime := []byte("signed MIME")
	digest := sha256.Sum256(mime)
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Delivery{Delivery: &controlv1.DeliveryStart{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, ExpectedSize: uint64(len(mime)), Sha256: digest[:]}}}
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_MimeChunk{MimeChunk: &controlv1.DataChunk{Id: "attempt", Data: mime, Last: true}}}
	select {
	case <-delayed.started:
	case <-ctx.Done():
		t.Fatal("execution did not start")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("expired session did not cancel execution", err)
		}
	case <-ctx.Done():
		t.Fatal("expired session did not terminate")
	}
	if e.calls != 0 {
		t.Fatal("execution continued after its grant expired")
	}
}

func TestLeaseGuardExtendsOnlyOnRenewal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := &leaseState{grant: timedGrant{deadline: time.Now().Add(200 * time.Millisecond)}, changed: make(chan struct{}, 1)}
	go state.guard(ctx, cancel)
	if !state.set(timedGrant{deadline: time.Now().Add(600 * time.Millisecond)}) {
		t.Fatal("valid renewal rejected")
	}
	select {
	case <-ctx.Done():
		t.Fatal("renewed grant ended at the old deadline")
	case <-time.After(350 * time.Millisecond):
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("renewed grant did not expire")
	}
}

func TestLeaseCannotBeRevivedAfterExpiry(t *testing.T) {
	state := &leaseState{grant: timedGrant{deadline: time.Now().Add(-time.Second)}, changed: make(chan struct{}, 1)}
	if state.set(timedGrant{deadline: time.Now().Add(time.Hour)}) {
		t.Fatal("late renewal revived an expired session")
	}
	if state.valid() {
		t.Fatal("expired session became authorized")
	}
}

func TestLeaseExpiryInterruptsReplayWithoutDiscardingResult(t *testing.T) {
	s, j, _, ctx := setup(t, true)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	fence := journal.Fence{ControllerID: "controller", Epoch: 1}
	if err := j.Activate(fence); err != nil {
		t.Fatal(err)
	}
	if err := j.Begin("attempt", "delivery", fence, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := j.Complete("attempt", journal.Accepted, 250, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.authority = timedAuthority{grant: timedGrant{deadline: time.Now().Add(300 * time.Millisecond)}}
	stream := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 1), sent: make(chan *controlv1.EdgeFrame, 2)}
	done := make(chan error, 1)
	go func() { done <- s.Connect(stream) }()
	stream.incoming <- openFrame(s, 1)
	if (<-stream.sent).GetReady() == nil || (<-stream.sent).GetResult() == nil {
		t.Fatal("pending result was not replayed")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("expired replay was not canceled", err)
		}
	case <-ctx.Done():
		t.Fatal("expired replay remained blocked")
	}
	items, err := j.List()
	if err != nil || len(items) != 1 || items[0].Outcome != journal.Accepted {
		t.Fatal("unacknowledged result was not retained", err)
	}
}

func TestReplayRejectsAcknowledgementAfterAuthorizationExpires(t *testing.T) {
	s, j, _, ctx := setup(t, true)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	fence := journal.Fence{ControllerID: "controller", Epoch: 1}
	if err := j.Activate(fence); err != nil {
		t.Fatal(err)
	}
	if err := j.Begin("attempt", "delivery", fence, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := j.Complete("attempt", journal.Accepted, 250, time.Now()); err != nil {
		t.Fatal(err)
	}
	g := &mutableGrant{}
	g.valid.Store(true)
	s.authority = mutableAuthority{grant: g}
	stream := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 2), sent: make(chan *controlv1.EdgeFrame, 2)}
	done := make(chan error, 1)
	go func() { done <- s.Connect(stream) }()
	stream.incoming <- openFrame(s, 1)
	if (<-stream.sent).GetReady() == nil {
		t.Fatal("session did not become ready")
	}
	result := (<-stream.sent).GetResult()
	if result == nil {
		t.Fatal("missing replayed result")
	}
	g.valid.Store(false)
	stream.incoming <- ackFrame(result)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expired controller acknowledgement was accepted")
		}
	case <-ctx.Done():
		t.Fatal("expired acknowledgement did not terminate the session")
	}
	items, err := j.List()
	if err != nil || len(items) != 1 || items[0].Outcome != journal.Accepted {
		t.Fatal("unacknowledged result was discarded", err)
	}
}

func TestRejectsOldProtocol(t *testing.T) {
	for _, version := range []uint32{1, 2, 3, 4, 5, 6, 8} {
		s, _, _, ctx := setup(t, true)
		open := openFrame(s, 1)
		open.GetOpen().ProtocolVersion = version
		stream := &testStream{ctx: ctx, incoming: []*controlv1.ControlFrame{open}}
		if err := s.Connect(stream); err == nil {
			t.Fatal("old protocol accepted")
		}
		if len(stream.sent) != 0 {
			t.Fatal("old protocol received a response")
		}
	}
}

func TestRejectsDifferentJournalBeforeFencingOrReplay(t *testing.T) {
	s, j, _, ctx := setup(t, true)
	old := journal.Fence{ControllerID: "controller", Epoch: 1}
	if err := j.Activate(old); err != nil {
		t.Fatal(err)
	}
	open := openFrame(s, 2)
	open.GetOpen().JournalId = "ffffffffffffffffffffffffffffffff"
	stream := &testStream{ctx: ctx, incoming: []*controlv1.ControlFrame{open}}
	if err := s.Connect(stream); err == nil || len(stream.sent) != 0 {
		t.Fatal("different journal was accepted or replayed")
	}
	current, err := j.IsCurrent(old)
	if err != nil || !current {
		t.Fatal("journal mismatch changed the persisted fence", err)
	}
}

func TestReconciliationAcknowledgementGatesBounceAndWake(t *testing.T) {
	s, _, _, ctx := setup(t, true)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stream := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 4), sent: make(chan *controlv1.EdgeFrame, 4)}
	done := make(chan error, 1)
	go func() { done <- s.Connect(stream) }()
	stream.incoming <- openFrame(s, 1)
	if (<-stream.sent).GetReady() == nil || (<-stream.sent).GetReplayComplete() == nil {
		t.Fatal("missing replay barrier")
	}
	if err := s.Forward(ctx, "0123456789abcdef0123456789abcdef", "abcdef0123456789abcdef0123456789", []byte("body")); err == nil {
		t.Fatal("bounce forwarded before reconciliation acknowledgement")
	}
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Wake{Wake: &controlv1.Wake{}}}
	if err := <-done; err == nil {
		t.Fatal("wake accepted before reconciliation acknowledgement")
	}
}

func TestRejectsMismatchedReconciliationAcknowledgement(t *testing.T) {
	for _, change := range []func(*controlv1.ReconciliationComplete){
		func(a *controlv1.ReconciliationComplete) { a.WorkerId = "other" },
		func(a *controlv1.ReconciliationComplete) { a.JournalId = "ffffffffffffffffffffffffffffffff" },
		func(a *controlv1.ReconciliationComplete) { a.SessionId = "ffffffffffffffffffffffffffffffff" },
		func(a *controlv1.ReconciliationComplete) { a.Epoch++ },
	} {
		s, _, _, ctx := setup(t, true)
		ack := reconcileFrame(s, 1)
		change(ack.GetReconciliationComplete())
		stream := &testStream{ctx: ctx, incoming: []*controlv1.ControlFrame{openFrame(s, 1), ack}}
		if err := s.Connect(stream); err == nil {
			t.Fatal("mismatched reconciliation acknowledgement accepted")
		}
		if len(stream.sent) != 2 || stream.sent[0].GetReady() == nil || stream.sent[1].GetReplayComplete() == nil {
			t.Fatal("unexpected edge frames after mismatched acknowledgement")
		}
	}
}

func TestRejectsMismatchedStartPermit(t *testing.T) {
	s, j, _, ctx := setup(t, true)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stream := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 8), sent: make(chan *controlv1.EdgeFrame, 8)}
	done := make(chan error, 1)
	go func() { done <- s.Connect(stream) }()
	stream.incoming <- openFrame(s, 1)
	<-stream.sent
	<-stream.sent
	stream.incoming <- reconcileFrame(s, 1)
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Wake{Wake: &controlv1.Wake{}}}
	<-stream.sent
	mime := []byte("signed MIME")
	digest := sha256.Sum256(mime)
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Delivery{Delivery: &controlv1.DeliveryStart{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, ExpectedSize: uint64(len(mime)), Sha256: digest[:]}}}
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_MimeChunk{MimeChunk: &controlv1.DataChunk{Id: "attempt", Data: mime, Last: true}}}
	var request *controlv1.StartRequest
	select {
	case frame := <-stream.sent:
		request = frame.GetStartRequest()
	case <-ctx.Done():
		t.Fatal("start request missing")
	}
	if request == nil {
		t.Fatal("unexpected frame")
	}
	tampered := proto.Clone(request).(*controlv1.StartRequest)
	tampered.SessionId = "ffffffffffffffffffffffffffffffff"
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_StartPermit{StartPermit: &controlv1.StartPermit{Request: tampered}}}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("mismatched permit accepted")
		}
	case <-ctx.Done():
		t.Fatal("mismatched permit did not terminate the session")
	}
	items, err := j.List()
	if err != nil || len(items) != 1 || items[0].Outcome != journal.InFlight {
		t.Fatal("mismatched permit allowed a delivery result", err)
	}
}

func TestStartPermitTimeoutEndsSessionAndReplaysUnsentResult(t *testing.T) {
	s, j, _, ctx := setup(t, true)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s.permitTimeout = 100 * time.Millisecond
	dialer := &timeoutTestDialer{}
	s.executor = &edgeworker.Worker{Journal: j, Dialer: dialer, HeloName: "smtp.sender.example"}
	stream := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 8), sent: make(chan *controlv1.EdgeFrame, 8)}
	done := make(chan error, 1)
	go func() { done <- s.Connect(stream) }()
	stream.incoming <- openFrame(s, 1)
	if (<-stream.sent).GetReady() == nil || (<-stream.sent).GetReplayComplete() == nil {
		t.Fatal("session not ready")
	}
	stream.incoming <- reconcileFrame(s, 1)
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Wake{Wake: &controlv1.Wake{}}}
	if (<-stream.sent).GetPull() == nil {
		t.Fatal("missing pull")
	}
	mime := []byte("signed MIME")
	digest := sha256.Sum256(mime)
	start := &controlv1.DeliveryStart{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, MxName: "mx.example", DestinationIp: "8.8.8.8", EnvelopeFrom: "bounce@example.com", Recipient: "to@example.net", ExpectedSize: uint64(len(mime)), Sha256: digest[:]}
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Delivery{Delivery: start}}
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_MimeChunk{MimeChunk: &controlv1.DataChunk{Id: "attempt", Data: mime, Last: true}}}
	request := (<-stream.sent).GetStartRequest()
	if request == nil {
		t.Fatal("missing start request")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("timed out permit did not terminate the session")
		}
	case <-ctx.Done():
		t.Fatal("permit wait did not time out")
	}
	if dialer.calls.Load() != 0 {
		t.Fatal("SMTP dialed without a timely permit")
	}
	items, err := j.List()
	if err != nil || len(items) != 1 || items[0].Outcome != journal.Rejected {
		t.Fatal("unsent attempt was not durably journaled", items, err)
	}
	select {
	case frame := <-stream.sent:
		t.Fatal("timed-out session sent another frame", frame)
	default:
	}
	// A permit from the former session may arrive after the deadline. The old
	// stream is gone; the new one must replay the rejected result before work.
	stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_StartPermit{StartPermit: &controlv1.StartPermit{Request: request}}}
	next := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 4), sent: make(chan *controlv1.EdgeFrame, 4)}
	nextDone := make(chan error, 1)
	go func() { nextDone <- s.Connect(next) }()
	next.incoming <- openFrame(s, 2)
	if (<-next.sent).GetReady() == nil {
		t.Fatal("replacement session not ready")
	}
	replayed := (<-next.sent).GetResult()
	if replayed == nil || replayed.AttemptId != start.AttemptId || replayed.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED || replayed.SmtpCode != 0 {
		t.Fatal("unsent result not replayed", replayed)
	}
	next.incoming <- ackFrame(replayed)
	if (<-next.sent).GetReplayComplete() == nil {
		t.Fatal("replay did not complete after acknowledgement")
	}
	close(next.incoming)
	if err = <-nextDone; err != nil {
		t.Fatal(err)
	}
	items, err = j.List()
	if err != nil || len(items) != 0 || dialer.calls.Load() != 0 {
		t.Fatal("replay did not safely settle the attempt", items, err)
	}
}

func TestTakeoverCancelsBlockedStreamWrite(t *testing.T) {
	s, _, _, ctx := setup(t, true)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	old := &blockedSendStream{
		channelStream: &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 1), sent: make(chan *controlv1.EdgeFrame, 1)},
		started:       make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{}),
	}
	oldDone := make(chan error, 1)
	go func() { oldDone <- s.Connect(old) }()
	old.incoming <- openFrame(s, 1)
	select {
	case <-old.started:
	case <-ctx.Done():
		t.Fatal("old stream did not block")
	}
	next := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 1), sent: make(chan *controlv1.EdgeFrame, 2)}
	nextDone := make(chan error, 1)
	go func() { nextDone <- s.Connect(next) }()
	next.incoming <- openFrame(s, 1)
	select {
	case frame := <-next.sent:
		if frame.GetReady() == nil {
			t.Fatal("replacement did not become ready")
		}
	case <-ctx.Done():
		t.Fatal("blocked write prevented takeover")
	}
	select {
	case <-oldDone:
	case <-ctx.Done():
		t.Fatal("old handler did not exit")
	}
	close(next.incoming)
	<-nextDone
	close(old.release)
	select {
	case <-old.exited:
	case <-ctx.Done():
		t.Fatal("transport writer did not exit after release")
	}
}

func TestCancelledStreamStopsQueuedWriter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked := &blockedSendStream{
		channelStream: &channelStream{ctx: ctx},
		started:       make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{}),
	}
	stream := &lockedStream{BidiStreamingServer: blocked, ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1)}
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- stream.Send(&controlv1.EdgeFrame{}) }()
	<-blocked.started
	go func() { second <- stream.Send(&controlv1.EdgeFrame{}) }()
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal("blocked writer did not cancel", err)
	}
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Fatal("queued writer did not cancel", err)
	}
	close(blocked.release)
	<-blocked.exited
}

func TestSameEpochTakeoverWaitsForOldExecution(t *testing.T) {
	s, _, _, base := setup(t, true)
	ctx, cancel := context.WithTimeout(base, 3*time.Second)
	defer cancel()
	stuck := &stuckExecutor{started: make(chan struct{}), release: make(chan struct{})}
	s.executor = stuck
	first := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 8), sent: make(chan *controlv1.EdgeFrame, 8)}
	firstDone := make(chan error, 1)
	go func() { firstDone <- s.Connect(first) }()
	first.incoming <- openFrame(s, 1)
	if (<-first.sent).GetReady() == nil || (<-first.sent).GetReplayComplete() == nil {
		t.Fatal("first session not ready")
	}
	first.incoming <- reconcileFrame(s, 1)
	first.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Wake{Wake: &controlv1.Wake{}}}
	if (<-first.sent).GetPull() == nil {
		t.Fatal("missing pull")
	}
	mime := []byte("signed MIME")
	digest := sha256.Sum256(mime)
	first.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Delivery{Delivery: &controlv1.DeliveryStart{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, ExpectedSize: uint64(len(mime)), Sha256: digest[:]}}}
	first.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_MimeChunk{MimeChunk: &controlv1.DataChunk{Id: "attempt", Data: mime, Last: true}}}
	select {
	case <-stuck.started:
	case <-ctx.Done():
		t.Fatal("execution did not start")
	}
	short, shortCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer shortCancel()
	second := &channelStream{ctx: short, incoming: make(chan *controlv1.ControlFrame, 1), sent: make(chan *controlv1.EdgeFrame, 2)}
	secondDone := make(chan error, 1)
	go func() { secondDone <- s.Connect(second) }()
	second.incoming <- openFrame(s, 1)
	select {
	case frame := <-second.sent:
		t.Fatalf("takeover admitted before old execution stopped: %v", frame)
	case err := <-secondDone:
		if err == nil {
			t.Fatal("stuck takeover succeeded")
		}
	case <-ctx.Done():
		t.Fatal("takeover did not time out")
	}
	close(stuck.release)
	select {
	case <-firstDone:
	case <-ctx.Done():
		t.Fatal("old session failed to drain")
	}
	close(first.incoming)
	close(second.incoming)
	third := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 1), sent: make(chan *controlv1.EdgeFrame, 2)}
	thirdDone := make(chan error, 1)
	go func() { thirdDone <- s.Connect(third) }()
	third.incoming <- openFrame(s, 1)
	select {
	case frame := <-third.sent:
		if frame.GetReady() == nil {
			t.Fatal("new session not ready")
		}
	case <-ctx.Done():
		t.Fatal("drained takeover blocked")
	}
	if (<-third.sent).GetReplayComplete() == nil {
		t.Fatal("missing replay barrier")
	}
	close(third.incoming)
	if err := <-thirdDone; err != nil {
		t.Fatal(err)
	}
}

func TestTakeoverRechecksLeaseAfterDraining(t *testing.T) {
	s, j, _, ctx := setup(t, true)
	old := journal.Fence{ControllerID: "old", Epoch: 1}
	next := journal.Fence{ControllerID: "new", Epoch: 2}
	if err := j.Activate(old); err != nil {
		t.Fatal(err)
	}
	oldCtx, oldCancel := context.WithCancel(ctx)
	s.owner = &sessionOwner{fence: old, ctx: oldCtx, cancel: oldCancel, done: make(chan struct{})}
	g := &mutableGrant{}
	g.valid.Store(true)
	done := make(chan error, 1)
	go func() { _, err := s.enter(ctx, next, g); done <- err }()
	<-oldCtx.Done()
	g.valid.Store(false)
	close(s.owner.done)
	if err := <-done; err == nil {
		t.Fatal("expired lease activated after draining")
	}
	if current, err := j.IsCurrent(old); err != nil || !current {
		t.Fatal("expired takeover changed fence", err)
	}
}

func TestSessionReplaysOldResultsAndRejectsBadAck(t *testing.T) {
	s, j, _, ctx := setup(t, true)
	old := journal.Fence{ControllerID: "controller", Epoch: 1}
	if err := j.Activate(old); err != nil {
		t.Fatal(err)
	}
	if err := j.Begin("old-attempt", "old-delivery", old, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := j.Complete("old-attempt", journal.Accepted, 250, time.Now()); err != nil {
		t.Fatal(err)
	}
	wrong := &controlv1.DeliveryResult{AttemptId: "old-attempt", DeliveryId: "old-delivery", Epoch: 1, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED, SmtpCode: 550}
	stream := &testStream{ctx: ctx, incoming: []*controlv1.ControlFrame{openFrame(s, 2), ackFrame(wrong)}}
	if err := s.Connect(stream); err == nil {
		t.Fatal("mismatched acknowledgement accepted")
	}
	items, err := j.List()
	if err != nil || len(items) != 1 || items[0].Outcome != journal.Accepted {
		t.Fatal("result lost")
	}
	if len(stream.sent) != 2 || stream.sent[1].GetResult().Epoch != 1 {
		t.Fatal("old epoch result not replayed")
	}
}

func TestRouteRejectionReplayKeepsScopeUntilExactAcknowledgement(t *testing.T) {
	s, j, _, ctx := setup(t, true)
	fence := journal.Fence{ControllerID: "controller", Epoch: 1}
	if err := j.Activate(fence); err != nil {
		t.Fatal(err)
	}
	if err := j.Begin("route-attempt", "route-delivery", fence, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := j.Complete("route-attempt", journal.RouteRejected, 554, time.Now()); err != nil {
		t.Fatal(err)
	}
	wrong := &controlv1.DeliveryResult{AttemptId: "route-attempt", DeliveryId: "route-delivery", Epoch: 1, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED, SmtpCode: 554}
	stream := &testStream{ctx: ctx, incoming: []*controlv1.ControlFrame{openFrame(s, 2), ackFrame(wrong)}}
	if err := s.Connect(stream); err == nil {
		t.Fatal("acknowledgement changed the rejection scope")
	}
	items, err := j.List()
	if err != nil || len(items) != 1 || items[0].Outcome != journal.RouteRejected || items[0].SMTPCode != 554 {
		t.Fatal("route result was lost", items, err)
	}
	if len(stream.sent) < 2 || stream.sent[1].GetResult().GetOutcome() != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ROUTE_REJECTED || stream.sent[1].GetResult().GetSmtpCode() != 554 {
		t.Fatal("replayed route result lost scope or code")
	}
	result, err := toResult(items[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ack(&controlv1.ResultAcknowledgement{Result: result}); err != nil {
		t.Fatal("exact route result acknowledgement failed", err)
	}
}

func TestPolicyDenialReplaysUntilExactAcknowledgement(t *testing.T) {
	s, j, _, ctx := setup(t, true)
	fence := journal.Fence{ControllerID: "controller", Epoch: 1}
	if err := j.Activate(fence); err != nil {
		t.Fatal(err)
	}
	if err := j.Begin("policy-attempt", "policy-delivery", fence, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := j.Complete("policy-attempt", journal.RecipientPolicyDenied, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	wrong := &controlv1.DeliveryResult{AttemptId: "policy-attempt", DeliveryId: "policy-delivery", Epoch: 1, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED}
	stream := &testStream{ctx: ctx, incoming: []*controlv1.ControlFrame{openFrame(s, 2), ackFrame(wrong)}}
	if err := s.Connect(stream); err == nil {
		t.Fatal("acknowledgement changed the policy denial")
	}
	items, err := j.List()
	if err != nil || len(items) != 1 || items[0].Outcome != journal.RecipientPolicyDenied {
		t.Fatal("policy result lost", err)
	}
	if len(stream.sent) < 2 || stream.sent[1].GetResult().GetOutcome() != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_RECIPIENT_POLICY_DENIED {
		t.Fatal("policy denial did not replay")
	}
	result, err := toResult(items[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ack(&controlv1.ResultAcknowledgement{Result: result}); err != nil {
		t.Fatal("exact policy acknowledgement failed", err)
	}
}
func TestReplayBarrierRejectsUnreconciledInFlightJournal(t *testing.T) {
	s, j, _, ctx := setup(t, true)
	old := journal.Fence{ControllerID: "controller", Epoch: 1}
	if err := j.Activate(old); err != nil {
		t.Fatal(err)
	}
	if err := j.Begin("old-attempt", "old-delivery", old, time.Now()); err != nil {
		t.Fatal(err)
	}
	stream := &testStream{ctx: ctx, incoming: []*controlv1.ControlFrame{openFrame(s, 2)}}
	if err := s.Connect(stream); err == nil {
		t.Fatal("unreconciled attempt crossed replay barrier")
	}
	for _, frame := range stream.sent {
		if frame.GetReplayComplete() != nil {
			t.Fatal("false replay barrier sent")
		}
	}
}
func TestSessionRequiresVerifiedIdentityAndGrant(t *testing.T) {
	s, _, _, ctx := setup(t, false)
	stream := &testStream{ctx: ctx, incoming: []*controlv1.ControlFrame{openFrame(s, 1)}}
	if err := s.Connect(stream); err == nil {
		t.Fatal("invalid grant accepted")
	}
	s, _, _, _ = setup(t, true)
	stream = &testStream{ctx: context.Background(), incoming: []*controlv1.ControlFrame{openFrame(s, 1)}}
	if err := s.Connect(stream); err == nil {
		t.Fatal("unverified peer accepted")
	}
}
