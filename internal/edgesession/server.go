// Package edgesession implements a bounded trusted-initiated control stream.
// Runtime listener setup and production resource limits remain required.
package edgesession

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"sync"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/bounce"
	"github.com/zyno-io/thinpost/internal/controltls"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/journal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Grant must be verified locally against authenticated lease evidence and must
// expire without contacting trusted from edge. Deadline is a local monotonic
// deadline where available; Valid must become false no later than that time.
// The runtime must wire the locally verified authority, never an unconditional grant.
type Grant interface {
	Valid() bool
	Deadline() time.Time
}
type Authority interface {
	Authorize(context.Context, string, *controlv1.Open) (Grant, error)
}
type Journal interface {
	Identity() (string, error)
	Activate(journal.Fence) error
	IsCurrent(journal.Fence) (bool, error)
	List() ([]journal.Attempt, error)
	Remaining() (int, error)
	Acknowledge(journal.Attempt) error
}
type Executor interface {
	Execute(context.Context, journal.Fence, *controlv1.DeliveryStart, []byte, func() bool, func(context.Context, *controlv1.DeliveryStart) error) (*controlv1.DeliveryResult, error)
}

type Server struct {
	controlv1.UnimplementedEdgeControlServer
	workerID   string
	identities []string
	journal    Journal
	executor   Executor
	authority  Authority
	slots      chan struct{}
	bounces    bounceBridge
	inbound    inboundBridge
	submission submissionBridge
	takeover   sync.Mutex
	owner      *sessionOwner
	// sources and bounceDomains are reported in Ready. Advertise sets them once
	// at startup, before the server accepts a stream.
	sources, sourceHostnames, bounceDomains []string
	// Tests may shorten the permit deadline; production cannot lengthen it.
	permitTimeout time.Duration
}

const maxPermitWait = 30 * time.Second

type sessionOwner struct {
	fence  journal.Fence
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	work   sync.WaitGroup
}

// enter serializes ownership transitions. No replacement may open admission
// until the former session's SMTP goroutines have exited and journaled results.
func (s *Server) enter(ctx context.Context, fence journal.Fence, grant Grant) (*sessionOwner, error) {
	s.takeover.Lock()
	defer s.takeover.Unlock()
	if ctx.Err() != nil || grant == nil || !grant.Valid() {
		return nil, errors.New("controller lease unavailable")
	}
	if previous := s.owner; previous != nil {
		if fence.Epoch < previous.fence.Epoch || fence.Epoch == previous.fence.Epoch && fence.ControllerID != previous.fence.ControllerID {
			return nil, journal.ErrStaleController
		}
		previous.cancel()
		select {
		case <-previous.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if ctx.Err() != nil || !grant.Valid() {
		return nil, errors.New("controller lease unavailable")
	}
	if err := s.journal.Activate(fence); err != nil {
		return nil, err
	}
	if err := s.bounces.activate(fence); err != nil {
		return nil, err
	}
	if err := s.inbound.activate(fence); err != nil {
		return nil, err
	}
	if err := s.submission.activate(fence); err != nil {
		return nil, err
	}
	ownerCtx, cancel := context.WithCancel(ctx)
	owner := &sessionOwner{fence: fence, ctx: ownerCtx, cancel: cancel, done: make(chan struct{})}
	s.owner = owner
	return owner, nil
}

type leaseState struct {
	mu      sync.RWMutex
	grant   Grant
	changed chan struct{}
}

func (s *leaseState) valid() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.grant != nil && s.grant.Valid() && time.Now().Before(s.grant.Deadline())
}

// set serializes renewal with expiry. A late renewal cannot revive a grant
// merely because the deadline guard has not yet been scheduled.
func (s *leaseState) set(grant Grant) bool {
	s.mu.Lock()
	if s.grant == nil || !s.grant.Valid() || !time.Now().Before(s.grant.Deadline()) || grant == nil || !grant.Valid() || !time.Now().Before(grant.Deadline()) {
		s.mu.Unlock()
		return false
	}
	s.grant = grant
	s.mu.Unlock()
	select {
	case s.changed <- struct{}{}:
	default:
	}
	return true
}

// guard cancels in-flight SMTP work when the current grant expires, even when
// the peer is idle. A renewal wakes the guard to install the new deadline.
func (s *leaseState) guard(ctx context.Context, cancel context.CancelFunc) {
	for {
		s.mu.RLock()
		grant := s.grant
		if grant == nil || !grant.Valid() || grant.Deadline().IsZero() {
			s.mu.RUnlock()
			cancel()
			return
		}
		deadline := grant.Deadline()
		s.mu.RUnlock()
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-s.changed:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			s.mu.RLock()
			current := s.grant
			if current == nil || !current.Valid() || !time.Now().Before(current.Deadline()) {
				cancel()
				s.mu.RUnlock()
				return
			}
			s.mu.RUnlock()
		}
	}
}

type lockedStream struct {
	grpc.BidiStreamingServer[controlv1.ControlFrame, controlv1.EdgeFrame]
	ctx    context.Context
	cancel context.CancelFunc
	gate   chan struct{}
}

func (s *lockedStream) Send(frame *controlv1.EdgeFrame) error {
	// A gRPC Send may wait for the peer's flow-control window. Return from the
	// handler on cancellation or timeout so gRPC closes the RPC and unblocks the
	// one outstanding writer. The gate prevents concurrent transport writes.
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case s.gate <- struct{}{}:
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-timer.C:
		s.cancel()
		return status.Error(codes.DeadlineExceeded, "control stream write timed out")
	}
	if err := s.ctx.Err(); err != nil {
		<-s.gate
		return err
	}
	result := make(chan error, 1)
	go func() {
		defer func() { <-s.gate }()
		result <- s.BidiStreamingServer.Send(frame)
	}()
	select {
	case err := <-result:
		return err
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-timer.C:
		s.cancel()
		return status.Error(codes.DeadlineExceeded, "control stream write timed out")
	}
}

// Forward fulfills the SMTP bounce receiver contract without creating any
// edge-to-trusted connection. The caller supplies its bounded deadline.
func (s *Server) Forward(ctx context.Context, token, id string, body []byte) error {
	return s.bounces.Forward(ctx, token, id, body)
}

// New requires TLS-verified controller URI identities. Individual process
// IDs are bound to the authenticated certificate by the signed lease proof.
// The gRPC server must use controltls.Server credentials and configured message
// limits. This constructor does not create a listener or connect into trusted.
func New(workerID string, identities []string, journal Journal, executor Executor, authority Authority, maxSessions int) (*Server, error) {
	if workerID == "" || len(identities) == 0 || journal == nil || executor == nil || authority == nil || maxSessions <= 0 || maxSessions > 4 {
		return nil, errors.New("invalid edge session configuration")
	}
	s := &Server{workerID: workerID, journal: journal, executor: executor, authority: authority, slots: make(chan struct{}, maxSessions)}
	for _, identity := range identities {
		if identity == "" {
			return nil, errors.New("invalid controller identity")
		}
		s.identities = append(s.identities, identity)
	}
	return s, nil
}

// Advertise sets the outbound source addresses (primary first) and the bounce
// domains this worker reports in Ready. Call it once, before serving. A worker
// that never calls it reports neither and is treated as a legacy worker.
// EnableInbound makes the worker advertise the inbound capability and route
// inbound mail through Inbound. Call it once, before serving. Without it the
// worker never advertises the capability, never sends an inbound frame and
// refuses any it receives.
func (s *Server) EnableInbound() {
	s.inbound.mu.Lock()
	s.inbound.enabled = true
	s.inbound.mu.Unlock()
}

// EnableSubmission makes the worker advertise the submission capability (and
// the given public AUTH ports in Ready) and route submissions through
// Submission. Call it once, before serving. Without it the worker never
// advertises the capability, never sends a submission frame and refuses any it
// receives.
//
// The ports must be valid (at most 8, distinct, 1 to 65535); a bad list is an
// error so a misconfigured worker fails at startup instead of being refused by
// every control plane.
func (s *Server) EnableSubmission(ports ...uint32) error {
	if !edgeprotocol.ValidSubmissionPorts(ports) {
		return errors.New("invalid submission ports")
	}
	s.submission.mu.Lock()
	s.submission.enabled = true
	s.submission.ports = append([]uint32(nil), ports...)
	s.submission.mu.Unlock()
	return nil
}

// Submission is the SMTP submission listeners' view of the submission path.
func (s *Server) Submission() SubmissionClient { return &s.submission }

// Inbound is the SMTP receiver's view of the inbound path.
func (s *Server) Inbound() bounce.Inbound { return &s.inbound }

func (s *Server) Advertise(sources, hostnames, bounceDomains []string) {
	s.sources = append([]string(nil), sources...)
	s.sourceHostnames = append([]string(nil), hostnames...)
	s.bounceDomains = append([]string(nil), bounceDomains...)
}
func (s *Server) Connect(stream grpc.BidiStreamingServer[controlv1.ControlFrame, controlv1.EdgeFrame]) error {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return status.Error(codes.ResourceExhausted, "edge session capacity reached")
	}
	p, ok := peer.FromContext(stream.Context())
	if !ok {
		return status.Error(codes.Unauthenticated, "verified TLS identity required")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return status.Error(codes.Unauthenticated, "verified TLS identity required")
	}
	identity, err := controltls.Identity(tlsInfo.State, s.identities)
	if err != nil {
		return status.Error(codes.Unauthenticated, "verified TLS identity required")
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil || open.WorkerId != s.workerID || open.ControllerId == "" || open.Epoch == 0 || !validBounceID(open.SessionId) || open.ProtocolVersion != edgeprotocol.Version || !edgeprotocol.ValidCapabilities(open.Capabilities) {
		return status.Error(codes.PermissionDenied, "invalid controller session")
	}
	// Inbound frames flow only when both sides advertised the capability.
	inboundNegotiated := s.inbound.isEnabled() && edgeprotocol.HasCapability(open.Capabilities, edgeprotocol.CapInbound)
	var capabilities []string
	if s.inbound.isEnabled() {
		capabilities = []string{edgeprotocol.CapInbound}
	}
	submissionNegotiated := s.submission.isEnabled() && edgeprotocol.HasCapability(open.Capabilities, edgeprotocol.CapSubmission)
	var submissionPorts []uint32
	if s.submission.isEnabled() {
		capabilities = append(capabilities, edgeprotocol.CapSubmission)
		submissionPorts = s.submission.reportedPorts()
	}
	journalID, err := s.journal.Identity()
	if err != nil || !journal.ValidIdentity(journalID) || open.JournalId != journalID {
		return status.Error(codes.FailedPrecondition, "journal identity mismatch")
	}
	grant, err := s.authority.Authorize(stream.Context(), identity, open)
	if err != nil || grant == nil || !grant.Valid() {
		return status.Error(codes.PermissionDenied, "controller lease unavailable")
	}
	fence := journal.Fence{ControllerID: open.ControllerId, Epoch: open.Epoch}
	owner, err := s.enter(stream.Context(), fence, grant)
	if err != nil {
		return status.Error(codes.FailedPrecondition, "controller fence rejected")
	}
	defer func() { owner.cancel(); owner.work.Wait(); close(owner.done) }()
	stream = &lockedStream{BidiStreamingServer: stream, ctx: owner.ctx, cancel: owner.cancel, gate: make(chan struct{}, 1)}
	lease := &leaseState{grant: grant, changed: make(chan struct{}, 1)}
	go lease.guard(owner.ctx, owner.cancel)
	authorized := func() bool {
		if owner.ctx.Err() != nil || !lease.valid() {
			return false
		}
		current, err := s.journal.IsCurrent(fence)
		return err == nil && current
	}
	if !authorized() {
		return status.Error(codes.PermissionDenied, "controller lease expired")
	}
	if err = stream.Send(&controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Ready{Ready: &controlv1.Ready{WorkerId: s.workerID, JournalId: journalID, Epoch: open.Epoch, SessionId: open.SessionId, ProtocolVersion: edgeprotocol.Version, SourceAddresses: s.sources, SourceHostnames: s.sourceHostnames, BounceDomains: s.bounceDomains, Capabilities: capabilities, SubmissionPorts: submissionPorts}}}); err != nil {
		return err
	}
	// Replay one result at a time and wait for its exact acknowledgement. This
	// bounds stream buffering and avoids two-way flow-control deadlocks.
	pending, err := s.journal.List()
	if err != nil {
		return status.Error(codes.Internal, "journal unavailable")
	}
	for _, attempt := range pending {
		if attempt.Outcome == journal.InFlight {
			return status.Error(codes.FailedPrecondition, "unreconciled journal attempt")
		}
		result, err := toResult(attempt)
		if err != nil {
			return status.Error(codes.Internal, "invalid journal result")
		}
		if err = s.report(owner.ctx, stream, result, identity, open, lease, nil); err != nil {
			return err
		}
	}
	if !authorized() {
		return status.Error(codes.PermissionDenied, "controller lease expired")
	}
	if err = stream.Send(&controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}); err != nil {
		return err
	}
	var connection *bounceConnection
	var inboundConn *inboundConnection
	var submissionConn *submissionConnection
	var routes edgeprotocol.RoutesAssembler
	defer func() {
		if connection != nil {
			s.bounces.detach(connection)
		}
		if inboundConn != nil {
			s.inbound.detach(inboundConn)
		}
		if submissionConn != nil {
			s.submission.detach(submissionConn)
		}
	}()
	reconciled := false
	var assembler *edgeprotocol.Assembler
	var started *controlv1.DeliveryStart
	var pendingResult *controlv1.DeliveryResult
	pulled := false
	executing := false
	type received struct {
		frame *controlv1.ControlFrame
		err   error
	}
	type execution struct {
		result *controlv1.DeliveryResult
		err    error
	}
	type permitCall struct {
		request *controlv1.StartRequest
		reply   chan error
	}
	incoming := make(chan received, 1)
	executed := make(chan execution, 1)
	permitCalls := make(chan permitCall, 1)
	var pendingPermit *permitCall
	requestPermit := func(ctx context.Context, start *controlv1.DeliveryStart) error {
		digest, err := edgeprotocol.DeliveryDigest(start)
		if err != nil {
			return err
		}
		call := permitCall{request: &controlv1.StartRequest{WorkerId: s.workerID, JournalId: journalID, SessionId: open.SessionId, AttemptId: start.AttemptId, DeliveryId: start.DeliveryId, Epoch: start.Epoch, DescriptorSha256: digest[:]}, reply: make(chan error, 1)}
		timeout := maxPermitWait
		if s.permitTimeout > 0 && s.permitTimeout < timeout {
			timeout = s.permitTimeout
		}
		permitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		// A timed-out request can already have committed at Trusted. End this
		// stream before reporting or taking other work, so a delayed permit
		// cannot race a new attempt. The journaled result is replayed on the
		// next session after the old executor has exited.
		stop := func() error {
			owner.cancel()
			return permitCtx.Err()
		}
		if permitCtx.Err() != nil {
			return stop()
		}
		select {
		case permitCalls <- call:
		case <-permitCtx.Done():
			return stop()
		}
		select {
		case err := <-call.reply:
			if permitCtx.Err() != nil {
				return stop()
			}
			return err
		case <-permitCtx.Done():
			return stop()
		}
	}
	go func() {
		for {
			frame, err := stream.Recv()
			select {
			case incoming <- received{frame: frame, err: err}:
			case <-owner.ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		var frame *controlv1.ControlFrame
		select {
		case item := <-incoming:
			if errors.Is(item.err, io.EOF) {
				return nil
			}
			if item.err != nil {
				return item.err
			}
			frame = item.frame
			if frame == nil {
				return status.Error(codes.InvalidArgument, "empty control frame")
			}
		case item := <-executed:
			executing = false
			if !authorized() {
				return status.Error(codes.PermissionDenied, "controller lease expired")
			}
			if item.err != nil || item.result == nil {
				return status.Error(codes.Internal, "delivery execution failed")
			}
			pendingResult = item.result
			if err = stream.Send(&controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Result{Result: item.result}}); err != nil {
				return err
			}
			continue
		case call := <-permitCalls:
			if !executing || pendingPermit != nil || !authorized() {
				return status.Error(codes.FailedPrecondition, "start request not authorized")
			}
			pendingPermit = &call
			if err = stream.Send(&controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_StartRequest{StartRequest: call.request}}); err != nil {
				return err
			}
			continue
		case <-owner.ctx.Done():
			return owner.ctx.Err()
		}
		switch body := frame.Body.(type) {
		case *controlv1.ControlFrame_LeaseRenewal:
			grant, err = s.renew(stream.Context(), identity, open, body.LeaseRenewal)
			if err != nil {
				return status.Error(codes.PermissionDenied, "controller lease renewal rejected")
			}
			if !lease.set(grant) {
				return status.Error(codes.PermissionDenied, "controller lease renewal arrived too late")
			}
			if err = ackRenewal(stream, open.Epoch, body.LeaseRenewal.LeasePayload); err != nil {
				return err
			}
		case *controlv1.ControlFrame_ReconciliationComplete:
			ack := body.ReconciliationComplete
			if reconciled || ack == nil || ack.WorkerId != s.workerID || ack.JournalId != journalID || ack.SessionId != open.SessionId || ack.Epoch != open.Epoch || !authorized() {
				return status.Error(codes.FailedPrecondition, "invalid reconciliation acknowledgement")
			}
			connection, err = s.bounces.attach(fence, stream.Send, authorized)
			if err != nil {
				return status.Error(codes.FailedPrecondition, "controller fence rejected")
			}
			if inboundNegotiated {
				inboundConn, err = s.inbound.attach(fence, stream.Send, authorized)
				if err != nil {
					return status.Error(codes.FailedPrecondition, "controller fence rejected")
				}
			} else {
				s.inbound.withoutCapability()
			}
			if submissionNegotiated {
				submissionConn, err = s.submission.attach(fence, stream.Send, authorized)
				if err != nil {
					return status.Error(codes.FailedPrecondition, "controller fence rejected")
				}
			}
			reconciled = true
		case *controlv1.ControlFrame_Routes:
			if inboundConn == nil || !authorized() {
				return status.Error(codes.FailedPrecondition, "inbound routes not negotiated")
			}
			complete, done, err := routes.Add(body.Routes)
			if err != nil {
				return status.Error(codes.InvalidArgument, "invalid inbound routes")
			}
			if done && !s.inbound.setRoutes(inboundConn, complete) {
				return status.Error(codes.PermissionDenied, "controller lease expired")
			}
		case *controlv1.ControlFrame_InboundAck:
			if inboundConn == nil || !authorized() {
				return status.Error(codes.FailedPrecondition, "inbound acknowledgement not negotiated")
			}
			s.inbound.acknowledge(inboundConn, body.InboundAck)
		case *controlv1.ControlFrame_AuthResult:
			if submissionConn == nil || !authorized() {
				return status.Error(codes.FailedPrecondition, "submission not negotiated")
			}
			if body.AuthResult == nil {
				return status.Error(codes.InvalidArgument, "invalid submission frame")
			}
			s.submission.acknowledgeAuth(submissionConn, body.AuthResult)
		case *controlv1.ControlFrame_SubmissionAck:
			if submissionConn == nil || !authorized() {
				return status.Error(codes.FailedPrecondition, "submission not negotiated")
			}
			if body.SubmissionAck == nil {
				return status.Error(codes.InvalidArgument, "invalid submission frame")
			}
			s.submission.acknowledge(submissionConn, body.SubmissionAck)
		case *controlv1.ControlFrame_Wake:
			if !reconciled {
				return status.Error(codes.FailedPrecondition, "reconciliation not acknowledged")
			}
			if assembler != nil || pulled || executing || pendingResult != nil {
				continue
			}
			if !authorized() {
				return status.Error(codes.PermissionDenied, "controller lease expired")
			}
			remaining, err := s.journal.Remaining()
			if err != nil {
				return status.Error(codes.Internal, "journal unavailable")
			}
			if remaining > 0 {
				if err = stream.Send(&controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Pull{Pull: &controlv1.Pull{AvailableSlots: 1}}}); err != nil {
					return err
				}
				pulled = true
			}
		case *controlv1.ControlFrame_Delivery:
			if !reconciled || !pulled || assembler != nil || !authorized() || body.Delivery.GetEpoch() != open.Epoch {
				return status.Error(codes.FailedPrecondition, "delivery not authorized")
			}
			assembler, err = edgeprotocol.NewAssembler(body.Delivery)
			if err != nil {
				return status.Error(codes.InvalidArgument, "invalid delivery")
			}
			started = body.Delivery
		case *controlv1.ControlFrame_MimeChunk:
			if assembler == nil || !authorized() {
				return status.Error(codes.FailedPrecondition, "delivery not authorized")
			}
			mime, err := assembler.Add(body.MimeChunk)
			if err != nil {
				return status.Error(codes.InvalidArgument, "invalid delivery chunk")
			}
			if mime != nil {
				if !authorized() {
					return status.Error(codes.PermissionDenied, "controller lease expired")
				}
				start := started
				assembler = nil
				started = nil
				pulled = false
				executing = true
				owner.work.Add(1)
				go func() {
					defer owner.work.Done()
					result, err := s.executor.Execute(owner.ctx, fence, start, mime, authorized, requestPermit)
					select {
					case executed <- execution{result: result, err: err}:
					case <-owner.ctx.Done():
					}
				}()
			}
		case *controlv1.ControlFrame_ResultAck:
			if !authorized() || pendingResult == nil || body.ResultAck == nil || body.ResultAck.Result == nil || !sameResult(pendingResult, body.ResultAck.Result) {
				return status.Error(codes.FailedPrecondition, "result acknowledgement mismatch")
			}
			if err = s.ack(body.ResultAck); err != nil && !errors.Is(err, journal.ErrNotFound) {
				return status.Error(codes.FailedPrecondition, "invalid result acknowledgement")
			}
			pendingResult = nil
		case *controlv1.ControlFrame_BounceAck:
			if !reconciled || !authorized() {
				return status.Error(codes.PermissionDenied, "controller lease expired")
			}
			s.bounces.acknowledge(connection, body.BounceAck)
		case *controlv1.ControlFrame_StartPermit:
			if pendingPermit == nil || body.StartPermit == nil || body.StartPermit.Request == nil || !proto.Equal(pendingPermit.request, body.StartPermit.Request) || !authorized() {
				return status.Error(codes.FailedPrecondition, "invalid SMTP start permit")
			}
			pendingPermit.reply <- nil
			pendingPermit = nil
		default:
			return status.Error(codes.Unimplemented, "control frame not implemented")
		}
	}
}
func (s *Server) report(ctx context.Context, stream grpc.BidiStreamingServer[controlv1.ControlFrame, controlv1.EdgeFrame], result *controlv1.DeliveryResult, identity string, open *controlv1.Open, lease *leaseState, connection *bounceConnection) error {
	if result == nil {
		return status.Error(codes.Internal, "missing delivery result")
	}
	if ctx.Err() != nil || !lease.valid() {
		return status.Error(codes.PermissionDenied, "controller lease expired")
	}
	if err := stream.Send(&controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Result{Result: result}}); err != nil {
		return err
	}
	for {
		type received struct {
			frame *controlv1.ControlFrame
			err   error
		}
		incoming := make(chan received, 1)
		go func() { frame, err := stream.Recv(); incoming <- received{frame: frame, err: err} }()
		var item received
		select {
		case item = <-incoming:
		case <-ctx.Done():
			return ctx.Err()
		}
		frame, err := item.frame, item.err
		if err != nil {
			return err
		}
		if ctx.Err() != nil || !lease.valid() {
			return status.Error(codes.PermissionDenied, "controller lease expired")
		}
		if frame.GetWake() != nil {
			continue
		}
		if renewal := frame.GetLeaseRenewal(); renewal != nil {
			next, err := s.renew(stream.Context(), identity, open, renewal)
			if err != nil {
				return status.Error(codes.PermissionDenied, "controller lease renewal rejected")
			}
			if !lease.set(next) {
				return status.Error(codes.PermissionDenied, "controller lease renewal arrived too late")
			}
			if err = ackRenewal(stream, open.Epoch, renewal.LeasePayload); err != nil {
				return err
			}
			continue
		}
		if ack := frame.GetBounceAck(); ack != nil && connection != nil {
			if ctx.Err() != nil || !lease.valid() {
				return status.Error(codes.PermissionDenied, "controller lease expired")
			}
			s.bounces.acknowledge(connection, ack)
			continue
		}
		ack := frame.GetResultAck()
		if ctx.Err() != nil || !lease.valid() || ack == nil || ack.Result == nil || !sameResult(result, ack.Result) {
			return status.Error(codes.FailedPrecondition, "result acknowledgement mismatch")
		}
		if err = s.ack(ack); err != nil && !errors.Is(err, journal.ErrNotFound) {
			return status.Error(codes.FailedPrecondition, "invalid result acknowledgement")
		}
		return nil
	}
}

func ackRenewal(stream grpc.BidiStreamingServer[controlv1.ControlFrame, controlv1.EdgeFrame], epoch uint64, payload []byte) error {
	digest := sha256.Sum256(payload)
	return stream.Send(&controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_LeaseAccepted{LeaseAccepted: &controlv1.LeaseAccepted{Epoch: epoch, PayloadSha256: digest[:]}}})
}

func (s *Server) renew(ctx context.Context, identity string, open *controlv1.Open, renewal *controlv1.LeaseRenewal) (Grant, error) {
	if renewal == nil {
		return nil, errors.New("missing lease renewal")
	}
	next := &controlv1.Open{WorkerId: open.WorkerId, ControllerId: open.ControllerId, JournalId: open.JournalId, Epoch: open.Epoch, LeasePayload: renewal.LeasePayload, LeaseSignature: renewal.LeaseSignature}
	grant, err := s.authority.Authorize(ctx, identity, next)
	if err != nil || grant == nil || !grant.Valid() {
		return nil, errors.New("invalid lease renewal")
	}
	return grant, nil
}
func (s *Server) ack(ack *controlv1.ResultAcknowledgement) error {
	if ack == nil || ack.Result == nil {
		return journal.ErrInvalidTransition
	}
	r := ack.Result
	outcome := ""
	switch r.Outcome {
	case controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED:
		outcome = journal.Accepted
	case controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED:
		outcome = journal.Rejected
	case controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ROUTE_REJECTED:
		outcome = journal.RouteRejected
	case controlv1.DeliveryOutcome_DELIVERY_OUTCOME_RECIPIENT_POLICY_DENIED:
		outcome = journal.RecipientPolicyDenied
	case controlv1.DeliveryOutcome_DELIVERY_OUTCOME_UNCERTAIN:
		outcome = journal.Uncertain
	default:
		return journal.ErrInvalidTransition
	}
	return s.journal.Acknowledge(journal.Attempt{ID: r.AttemptId, DeliveryID: r.DeliveryId, Epoch: r.Epoch, Outcome: outcome, SMTPCode: int(r.SmtpCode), SMTPStage: stageName(r.SmtpStage), SMTPEnhanced: r.SmtpEnhanced})
}
func toResult(a journal.Attempt) (*controlv1.DeliveryResult, error) {
	outcome := controlv1.DeliveryOutcome_DELIVERY_OUTCOME_UNSPECIFIED
	switch a.Outcome {
	case journal.Accepted:
		outcome = controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED
	case journal.Rejected:
		outcome = controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED
	case journal.RouteRejected:
		outcome = controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ROUTE_REJECTED
	case journal.RecipientPolicyDenied:
		outcome = controlv1.DeliveryOutcome_DELIVERY_OUTCOME_RECIPIENT_POLICY_DENIED
	case journal.Uncertain:
		outcome = controlv1.DeliveryOutcome_DELIVERY_OUTCOME_UNCERTAIN
	default:
		return nil, errors.New("invalid journal outcome")
	}
	return &controlv1.DeliveryResult{AttemptId: a.ID, DeliveryId: a.DeliveryID, Epoch: a.Epoch, Outcome: outcome, SmtpCode: uint32(a.SMTPCode), SmtpStage: stageValue(a.SMTPStage), SmtpEnhanced: a.SMTPEnhanced}, nil
}
func stageName(s controlv1.SmtpStage) string {
	switch s {
	case controlv1.SmtpStage_SMTP_STAGE_MAIL:
		return "MAIL"
	case controlv1.SmtpStage_SMTP_STAGE_RCPT:
		return "RCPT"
	case controlv1.SmtpStage_SMTP_STAGE_DATA:
		return "DATA"
	}
	return ""
}
func stageValue(s string) controlv1.SmtpStage {
	switch s {
	case "MAIL":
		return controlv1.SmtpStage_SMTP_STAGE_MAIL
	case "RCPT":
		return controlv1.SmtpStage_SMTP_STAGE_RCPT
	case "DATA":
		return controlv1.SmtpStage_SMTP_STAGE_DATA
	}
	return controlv1.SmtpStage_SMTP_STAGE_UNSPECIFIED
}
func sameResult(a, b *controlv1.DeliveryResult) bool {
	return a.AttemptId == b.AttemptId && a.DeliveryId == b.DeliveryId && a.Epoch == b.Epoch && a.Outcome == b.Outcome && a.SmtpCode == b.SmtpCode && a.SmtpStage == b.SmtpStage && a.SmtpEnhanced == b.SmtpEnhanced
}
