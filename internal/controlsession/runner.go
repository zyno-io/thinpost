// Package controlsession drives a trusted-initiated edge stream. Runtime
// connection, DNS, object-store work preparation and process wiring remain
// separate responsibilities.
package controlsession

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/netip"
	"sync"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/egress"
	"github.com/zyno-io/thinpost/internal/inbound"
	"github.com/zyno-io/thinpost/internal/journal"
	"github.com/zyno-io/thinpost/internal/oplog"
	"github.com/zyno-io/thinpost/internal/postgres"
	"github.com/zyno-io/thinpost/internal/submission"
)

var ErrLeaseExpired = errors.New("controller lease expired")
var ErrDraining = errors.New("controller session drained")

type Stream interface {
	Send(*controlv1.ControlFrame) error
	Recv() (*controlv1.EdgeFrame, error)
}
type ConnectFunc func(context.Context) (Stream, error)
type Issuer interface {
	Acquire(context.Context, string, string, time.Duration) (postgres.ControllerLease, []byte, []byte, error)
	Renew(context.Context, postgres.ControllerLease, time.Duration) (postgres.ControllerLease, []byte, []byte, error)
	Release(context.Context, postgres.ControllerLease) error
}
type Results interface {
	CommitDeliveryResultIdentified(context.Context, delivery.Result) (delivery.Committed, error)
}
type Starts interface {
	AuthorizeDeliveryStart(context.Context, postgres.ControllerLease, string, string, string, string, [32]byte, int64) error
}
type Reconciler interface {
	ReconcileOrphanedAttempts(context.Context, postgres.ControllerLease) (bool, error)
}

// Work.Next must commit a delivery attempt before returning the corresponding
// signed, immutable MIME bytes. ErrNoWork leaves the worker's pull pending.
// caps is what this session's worker reported (or the legacy equivalent); Next
// must claim and describe only deliveries the worker can serve.
type Work interface {
	Next(context.Context, postgres.ControllerLease, delivery.EdgeCapabilities) (*controlv1.DeliveryStart, []byte, error)
}

// Reports records the sources a worker reported, best effort and for display
// only: it never gates delivery, and its failure never ends the session.
type Reports interface {
	RecordEdgeSources(context.Context, postgres.ControllerLease, delivery.EdgeCapabilities) error
}

// Bounces must return nil only after the untrusted body and its receipt have
// both been durably committed. A failed commit results in a negative ack.
type Bounces interface {
	Forward(context.Context, string, string, []byte) error
}

// SourcePolicy checks the sending sources an Edge reports.
type SourcePolicy interface {
	CheckSources([]netip.Addr) error
}

type Runner struct {
	WorkerID, ControllerID, JournalID   string
	Issuer                              Issuer
	Results                             Results
	Starts                              Starts
	Reconciler                          Reconciler
	Work                                Work
	Bounces                             Bounces
	Connect                             ConnectFunc
	LeaseTTL, ClockMargin, PollInterval time.Duration
	// Legacy is what a worker that reports no sources can serve: its primary
	// address (control's own configuration) and the global bounce domain.
	Legacy delivery.EdgeCapabilities
	// Sources is optional. When set, an Edge whose Ready reports a source the
	// policy refuses (outside the infrastructure ranges, or named by a route
	// exception) is refused like any invalid Ready. The Edge applies no
	// destination policy, so this is what keeps extra sources from being
	// routed to as destinations.
	Sources SourcePolicy
	// Reports is optional.
	Reports Reports
	// Inbound is optional. When set, the session advertises the inbound
	// capability and, if the worker does too, receives inbound mail and pushes
	// the routing snapshot every RoutesInterval (DefaultRoutesInterval if zero).
	Inbound        Inbound
	RoutesInterval time.Duration
	// InboundPartialTTL overrides DefaultInboundPartialTTL (tests).
	InboundPartialTTL time.Duration
	// Submission is optional. When set, the session advertises the submission
	// capability and, if the worker does too, serves credential checks and
	// SMTP submissions.
	Submission Submission
	// SubmissionBudget overrides edgeprotocol.ControlSubmissionBudget (tests).
	// The budget is owned by the Runner, not the session, because the bridge
	// reuses one Runner across reconnects and a worker still unwinding from an
	// earlier stream keeps its bytes reserved.
	SubmissionBudget int64
	// SubmissionPartialTTL overrides DefaultSubmissionPartialTTL (tests).
	SubmissionPartialTTL time.Duration

	// budget is created on first use (see submissionBudget). It is a plain
	// pointer so a Runner stays copyable.
	budget *byteBudget
}
type receive struct {
	frame *controlv1.EdgeFrame
	err   error
}
type acquired struct {
	lease              postgres.ControllerLease
	payload, signature []byte
	err                error
}
type workResult struct {
	start *controlv1.DeliveryStart
	mime  []byte
	err   error
}

// Run maintains one stream. Any stream or lease failure cancels the connection;
// the caller reconnects and reconciles outstanding attempts. ClockMargin must
// exceed the monitored skew between trusted, the database and edge.
func (r *Runner) Run(parent context.Context) error {
	return r.RunDraining(parent, nil)
}

// RunDraining stops requesting work after drain closes, but keeps the stream
// and lease alive until already claimed work and in-progress results complete.
// Cancel parent to enforce a deadline; an interrupted attempt is reconciled
// by the replacement controller as usual.
func (r *Runner) RunDraining(parent context.Context, drain <-chan struct{}) error {
	if r.Issuer == nil || r.Results == nil || r.Starts == nil || r.Reconciler == nil || r.Work == nil || r.Connect == nil || r.WorkerID == "" || r.ControllerID == "" || !journal.ValidIdentity(r.JournalID) || r.LeaseTTL < 5*time.Second || r.LeaseTTL > 5*time.Minute || r.ClockMargin <= 0 || r.ClockMargin >= r.LeaseTTL/2 || r.PollInterval <= 0 {
		return errors.New("invalid control session configuration")
	}
	select {
	case <-drain:
		return ErrDraining
	default:
	}
	ctx, cancel := context.WithCancel(parent)
	lease, payload, signature, err := r.Issuer.Acquire(ctx, r.WorkerID, r.ControllerID, r.LeaseTTL)
	if err != nil {
		cancel()
		return err
	}
	acquiredLease := lease
	defer func() {
		// Cancellation comes first; a replacement still has to drain the old
		// worker before replay or new work. Cleanup must survive parent shutdown.
		cancel()
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if releaseErr := r.Issuer.Release(cleanup, acquiredLease); releaseErr != nil {
			log.Print("failed to release controller lease; waiting for expiry")
		}
	}()
	if !r.validLease(lease) {
		return ErrLeaseExpired
	}
	select {
	case <-drain:
		return ErrDraining
	default:
	}
	updates := make(chan time.Time, 1)
	expired := make(chan struct{}, 1)
	go guard(ctx, cancel, lease.ExpiresAt.Add(-r.ClockMargin), updates, expired)
	stream, err := r.Connect(ctx)
	if err != nil {
		return err
	}
	var sendMu sync.Mutex
	var sessionRaw [16]byte
	if _, err = rand.Read(sessionRaw[:]); err != nil {
		return err
	}
	sessionID := hex.EncodeToString(sessionRaw[:])
	send := func(frame *controlv1.ControlFrame) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		sendMu.Lock()
		defer sendMu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		return stream.Send(frame)
	}
	if err = send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_Open{Open: &controlv1.Open{WorkerId: r.WorkerID, ControllerId: r.ControllerID, JournalId: r.JournalID, Epoch: lease.Epoch, LeasePayload: payload, LeaseSignature: signature, SessionId: sessionID, ProtocolVersion: edgeprotocol.Version, Capabilities: r.capabilities()}}}); err != nil {
		return err
	}
	incoming := make(chan receive, 1)
	go func() {
		for {
			frame, err := stream.Recv()
			select {
			case incoming <- receive{frame, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	renewTimer := time.NewTimer(r.LeaseTTL / 3)
	defer renewTimer.Stop()
	pollTimer := time.NewTimer(time.Hour)
	if !pollTimer.Stop() {
		<-pollTimer.C
	}
	defer pollTimer.Stop()
	renewals := make(chan acquired, 1)
	workItems := make(chan workResult, 1)
	sentWork := make(chan error, 1)
	committed := make(chan struct {
		result *controlv1.DeliveryResult
		err    error
	}, 1)
	bounceCommitted := make(chan struct {
		id  string
		err error
	}, 1)
	var bounce *bounceAssembler
	bounceActive := false
	var pending *acquired
	var activeStart *controlv1.DeliveryStart
	var startAuthorized, permitActive bool
	type permitted struct {
		request *controlv1.StartRequest
		err     error
	}
	permits := make(chan permitted, 1)
	reconciledResult := make(chan error, 1)
	var pendingHash [32]byte
	ready, replayed, reconciled, pulled, workActive, resultActive := false, false, false, false, false, false
	draining := false
	// Inbound mail: up to edgeprotocol.MaxInboundItems messages are assembling or
	// being committed at once. inboundOn is set only when both sides advertised
	// the capability.
	inboundOn := false
	items := make(map[string]*inboundItem)
	// closed remembers IDs that expired or were discarded, with the time, so a
	// straggling chunk of an item the worker already gave up on is ignored, not
	// fatal. Entries are evicted by age. Completed items are not remembered: a
	// chunk after the last one is a real protocol violation.
	closed := make(map[string]time.Time)
	inboundProcessing := 0
	partialTTL := r.InboundPartialTTL
	if partialTTL <= 0 {
		partialTTL = DefaultInboundPartialTTL
	}
	expiry := time.NewTicker(partialTTL / 4)
	defer expiry.Stop()
	assembling := func() int {
		n := 0
		for _, item := range items {
			if !item.discard {
				n++
			}
		}
		return n
	}
	inboundDone := make(chan struct {
		id      string
		verdict controlv1.InboundVerdict
	}, edgeprotocol.MaxInboundItems)
	// SMTP submission: its own lane, with the same shape as inbound but a
	// process-wide byte budget. subOn is set only when both sides advertised.
	subOn := false
	lane := newSubmissionLane(r)
	defer lane.releaseAll()
	subExpiry := time.NewTicker(lane.ttl / 4)
	defer subExpiry.Stop()
	subDone := make(chan submissionDone, edgeprotocol.MaxSubmissionItems)
	authFinished := make(chan authDone, edgeprotocol.MaxAuthPending)
	routesInterval := r.RoutesInterval
	if routesInterval <= 0 {
		routesInterval = DefaultRoutesInterval
	}
	routesTimer := time.NewTimer(time.Hour)
	if !routesTimer.Stop() {
		<-routesTimer.C
	}
	defer routesTimer.Stop()
	routesDone := make(chan routesPush, 1)
	var routesActive, routesSent, routesDrainSent bool
	var routesHash [32]byte
	// pushRoutes reports whether a push was started.
	pushRoutes := func() bool {
		if routesActive || !inboundOn {
			return false
		}
		routesActive = true
		tempfailAll := draining
		known, last := routesSent, routesHash
		go func() {
			push := routesPush{}
			routes, err := r.Inbound.Routes(ctx)
			if err != nil {
				push.err = err
			} else if pages, hash, buildErr := routePages(routes, tempfailAll); buildErr != nil {
				push.err = buildErr
			} else if !known || hash != last {
				for _, page := range pages {
					if err = send(page); err != nil {
						push.sendErr = err
						break
					}
				}
				push.hash, push.sent = hash, push.sendErr == nil
			}
			select {
			case routesDone <- push:
			case <-ctx.Done():
			}
		}()
		return true
	}
	// caps is set from Ready, before any work is requested.
	caps := r.Legacy
	startWork := func() {
		if !draining {
			select {
			case <-drain:
				draining = true
				drain = nil
			default:
			}
		}
		if workActive || draining {
			return
		}
		workActive = true
		snapshot, snapshotCaps := lease, caps
		go func() {
			start, mime, err := r.Work.Next(ctx, snapshot, snapshotCaps)
			select {
			case workItems <- workResult{start, mime, err}:
			case <-ctx.Done():
			}
		}()
	}
	for {
		if !draining {
			select {
			case <-drain:
				draining = true
				drain = nil
			default:
			}
		}
		if draining && inboundOn && reconciled && !routesDrainSent {
			// Tell the worker to answer 451 at RCPT while this controller drains.
			// A periodic push still in flight may carry the old states, so this
			// is marked sent only once its own push has started.
			routesDrainSent = pushRoutes()
		}
		if draining && !workActive && activeStart == nil && !resultActive && !permitActive && bounce == nil && !bounceActive && len(items) == 0 && inboundProcessing == 0 && lane.idle() && !routesActive && (!inboundOn || !reconciled || routesDrainSent) {
			return ErrDraining
		}
		select {
		case <-drain:
			draining = true
			drain = nil
		case <-ctx.Done():
			select {
			case <-expired:
				return ErrLeaseExpired
			default:
				return ctx.Err()
			}
		case <-renewTimer.C:
			if pending != nil {
				return errors.New("lease renewal was not acknowledged")
			}
			snapshot := lease
			go func() {
				l, p, s, e := r.Issuer.Renew(ctx, snapshot, r.LeaseTTL)
				select {
				case renewals <- acquired{l, p, s, e}:
				case <-ctx.Done():
				}
			}()
		case renewed := <-renewals:
			if renewed.err != nil {
				return renewed.err
			}
			if !r.validLease(renewed.lease) || renewed.lease.Epoch != lease.Epoch {
				return errors.New("controller lease changed")
			}
			pending = &renewed
			pendingHash = sha256.Sum256(renewed.payload)
			if err = send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_LeaseRenewal{LeaseRenewal: &controlv1.LeaseRenewal{LeasePayload: renewed.payload, LeaseSignature: renewed.signature}}}); err != nil {
				return err
			}
		case item := <-incoming:
			if item.err != nil {
				if errors.Is(item.err, io.EOF) {
					return io.EOF
				}
				return item.err
			}
			if item.frame == nil {
				return errors.New("empty edge frame")
			}
			switch body := item.frame.Body.(type) {
			case *controlv1.EdgeFrame_Ready:
				if ready || body.Ready.GetWorkerId() != r.WorkerID || body.Ready.GetJournalId() != r.JournalID || body.Ready.GetEpoch() != lease.Epoch || body.Ready.GetSessionId() != sessionID || body.Ready.GetProtocolVersion() != edgeprotocol.Version {
					return errors.New("invalid edge readiness")
				}
				reported, valid := capabilitiesFromReady(body.Ready, r.Legacy)
				if !valid || !edgeprotocol.ValidCapabilities(body.Ready.GetCapabilities()) {
					return errors.New("invalid edge readiness")
				}
				if !edgeprotocol.ValidSubmissionPorts(body.Ready.GetSubmissionPorts()) {
					return errors.New("invalid edge readiness")
				}
				if r.Sources != nil && reported.Explicit && r.Sources.CheckSources(reported.Addresses) != nil {
					return errors.New("invalid edge readiness")
				}
				inboundOn = r.Inbound != nil && edgeprotocol.HasCapability(body.Ready.GetCapabilities(), edgeprotocol.CapInbound)
				subOn = r.Submission != nil && edgeprotocol.HasCapability(body.Ready.GetCapabilities(), edgeprotocol.CapSubmission)
				if subOn {
					// What the worker observes on its AUTH ports is shown only when
					// this controller can serve it too.
					reported.SubmissionPorts = make([]int, 0, len(body.Ready.GetSubmissionPorts()))
					for _, port := range body.Ready.GetSubmissionPorts() {
						reported.SubmissionPorts = append(reported.SubmissionPorts, int(port))
					}
				}
				ready, caps = true, reported
				if r.Reports != nil {
					// Display only, off the session's path: a slow or failed
					// write must not stall or end the stream.
					snapshot := lease
					go func() {
						if err := r.Reports.RecordEdgeSources(ctx, snapshot, reported); err != nil && ctx.Err() == nil {
							log.Print("failed to record edge sources")
						}
					}()
				}
			case *controlv1.EdgeFrame_ReplayComplete:
				if !ready || replayed || resultActive {
					return errors.New("invalid replay completion")
				}
				replayed = true
				snapshot := lease
				go func() {
					for {
						complete, reconcileErr := r.Reconciler.ReconcileOrphanedAttempts(ctx, snapshot)
						if reconcileErr != nil || complete {
							select {
							case reconciledResult <- reconcileErr:
							case <-ctx.Done():
							}
							return
						}
					}
				}()
			case *controlv1.EdgeFrame_LeaseAccepted:
				if pending == nil || body.LeaseAccepted.GetEpoch() != lease.Epoch || len(body.LeaseAccepted.GetPayloadSha256()) != sha256.Size || subtle.ConstantTimeCompare(body.LeaseAccepted.GetPayloadSha256(), pendingHash[:]) != 1 {
					return errors.New("invalid lease acknowledgement")
				}
				lease = pending.lease
				pending = nil
				select {
				case updates <- lease.ExpiresAt.Add(-r.ClockMargin):
				case <-ctx.Done():
					return ctx.Err()
				}
				renewTimer.Reset(r.LeaseTTL / 3)
			case *controlv1.EdgeFrame_Result:
				if !ready || resultActive || body.Result == nil {
					return errors.New("invalid edge result")
				}
				if replayed {
					if activeStart == nil || body.Result.AttemptId != activeStart.AttemptId || body.Result.DeliveryId != activeStart.DeliveryId || body.Result.Epoch != activeStart.Epoch || permitActive {
						return errors.New("unexpected delivery result")
					}
					if !startAuthorized && ((body.Result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED && body.Result.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_RECIPIENT_POLICY_DENIED) || body.Result.SmtpCode != 0) {
						return errors.New("unpermitted SMTP result")
					}
				}
				resultActive = true
				result := body.Result
				go func() {
					outcome, err := mapResult(r.WorkerID, r.JournalID, result)
					if err == nil {
						var committed delivery.Committed
						if committed, err = r.Results.CommitDeliveryResultIdentified(ctx, outcome); err == nil && !committed.Replayed {
							oplog.Delivery(committed.AppID, committed.MessageID, outcome.DeliveryID, oplog.Status(outcome.Outcome), outcome.SMTPCode, committed.SourceName)
						}
					}
					select {
					case committed <- struct {
						result *controlv1.DeliveryResult
						err    error
					}{result, err}:
					case <-ctx.Done():
					}
				}()
			case *controlv1.EdgeFrame_Pull:
				if !reconciled || pulled || body.Pull.GetAvailableSlots() != 1 {
					return errors.New("invalid edge pull")
				}
				pulled = true
				if !draining {
					startWork()
				}
			case *controlv1.EdgeFrame_StartRequest:
				request := body.StartRequest
				if !replayed || activeStart == nil || request == nil || permitActive || startAuthorized || request.WorkerId != r.WorkerID || request.JournalId != r.JournalID || request.SessionId != sessionID || request.AttemptId != activeStart.AttemptId || request.DeliveryId != activeStart.DeliveryId || request.Epoch != lease.Epoch {
					return errors.New("invalid start request")
				}
				digest, err := edgeprotocol.DeliveryDigest(activeStart)
				if err != nil || len(request.DescriptorSha256) != sha256.Size || subtle.ConstantTimeCompare(digest[:], request.DescriptorSha256) != 1 {
					return errors.New("invalid start descriptor")
				}
				permitActive = true
				snapshot := lease
				trackingCutoff := activeStart.TrackingSendCutoffUnixMs
				go func() {
					err := r.Starts.AuthorizeDeliveryStart(ctx, snapshot, request.AttemptId, request.DeliveryId, sessionID, r.JournalID, digest, trackingCutoff)
					select {
					case permits <- permitted{request: request, err: err}:
					case <-ctx.Done():
					}
				}()
			case *controlv1.EdgeFrame_Bounce:
				if !reconciled || r.Bounces == nil || bounce != nil || bounceActive {
					return errors.New("bounce not available")
				}
				bounce, err = newBounceAssembler(body.Bounce)
				if err != nil {
					return err
				}
			case *controlv1.EdgeFrame_BounceChunk:
				if bounce == nil {
					return errors.New("unexpected bounce chunk")
				}
				data, chunkErr := bounce.add(body.BounceChunk)
				if chunkErr != nil {
					return chunkErr
				}
				if data != nil {
					item := bounce
					bounce = nil
					bounceActive = true
					go func() {
						err := r.Bounces.Forward(ctx, item.start.RecipientToken, item.start.BounceId, data)
						select {
						case bounceCommitted <- struct {
							id  string
							err error
						}{item.start.BounceId, err}:
						case <-ctx.Done():
						}
					}()
				}
			case *controlv1.EdgeFrame_Inbound:
				start := body.Inbound
				if !reconciled || !inboundOn || !edgeprotocol.ValidInboundFraming(start) || items[start.InboundId] != nil {
					return errors.New("inbound not available")
				}
				item := &inboundItem{start: start, began: time.Now()}
				if assembling()+inboundProcessing >= edgeprotocol.MaxInboundItems {
					// More than four in flight means the worker's count has drifted
					// from ours (it gave up on one that we still hold). Refuse this
					// one with a tempfail, swallowing its chunks, and keep the session.
					if len(items) >= 4*edgeprotocol.MaxInboundItems {
						return errors.New("inbound not available")
					}
					item.discard = true
				} else if item.asm, err = edgeprotocol.NewInboundAssembler(start); err != nil {
					return err
				}
				items[start.InboundId] = item
			case *controlv1.EdgeFrame_InboundChunk:
				chunk := body.InboundChunk
				item := items[chunk.GetId()]
				if item == nil {
					if _, ok := closed[chunk.GetId()]; ok {
						continue
					}
					return errors.New("unexpected inbound chunk")
				}
				var data []byte
				finished := false
				if item.discard {
					size := item.start.ExpectedSize
					if chunk.Offset != item.received || len(chunk.Data) == 0 || len(chunk.Data) > edgeprotocol.MaxChunkBytes || uint64(len(chunk.Data)) > size-item.received {
						return errors.New("invalid inbound chunk")
					}
					item.received += uint64(len(chunk.Data))
					if chunk.Last != (item.received == size) {
						return errors.New("invalid final inbound chunk")
					}
					finished = chunk.Last
				} else {
					var chunkErr error
					if data, chunkErr = item.asm.Add(chunk); chunkErr != nil {
						return chunkErr
					}
					finished = data != nil
				}
				if !finished {
					continue
				}
				delete(items, item.start.InboundId)
				if item.discard {
					closed[item.start.InboundId] = time.Now()
				}
				start := item.start
				if item.discard || !edgeprotocol.ValidInboundStart(start) {
					// Refused without committing anything: a tempfail when over the
					// limit, a permanent rejection for an envelope that can never be
					// valid. Neither ends the session.
					verdict := controlv1.InboundVerdict_INBOUND_VERDICT_TEMPFAIL
					if !item.discard {
						verdict = controlv1.InboundVerdict_INBOUND_VERDICT_REJECTED
					}
					if err = send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_InboundAck{InboundAck: &controlv1.InboundAcknowledgement{InboundId: start.InboundId, Verdict: verdict}}}); err != nil {
						return err
					}
					continue
				}
				inboundProcessing++
				deadline := item.began.Add(inbound.ReceiveTimeout)
				go func() {
					// The 45 second budget starts when the message started, not
					// when its last chunk arrived.
					receiveCtx, stop := context.WithDeadline(ctx, deadline)
					defer stop()
					verdict := inboundVerdict(r.Inbound.Receive(receiveCtx, inboundMessage(start, data)))
					select {
					case inboundDone <- struct {
						id      string
						verdict controlv1.InboundVerdict
					}{start.InboundId, verdict}:
					case <-ctx.Done():
					}
				}()
			case *controlv1.EdgeFrame_Auth:
				if !reconciled || !subOn || !edgeprotocol.ValidSubmissionID(body.Auth.GetRequestId()) {
					return errors.New("submission not available")
				}
				if verdict := lane.verify(ctx, body.Auth, draining, authFinished); verdict != controlv1.SubmissionAuthVerdict_SUBMISSION_AUTH_VERDICT_UNSPECIFIED {
					if err = send(authResultFrame(body.Auth.RequestId, verdict)); err != nil {
						return err
					}
				}
			case *controlv1.EdgeFrame_Submission:
				start := body.Submission
				if !reconciled || !subOn || !edgeprotocol.ValidSubmissionFraming(start) {
					return errors.New("submission not available")
				}
				if err = lane.begin(start, draining); err != nil {
					return err
				}
			case *controlv1.EdgeFrame_SubmissionChunk:
				if !subOn {
					return errors.New("unexpected submission chunk")
				}
				finished, chunkErr := lane.add(body.SubmissionChunk)
				if chunkErr != nil {
					return chunkErr
				}
				if finished.item == nil {
					continue
				}
				if finished.data == nil {
					// Refused without committing anything; neither ends the session.
					if err = send(refusalAck(finished.item.start.SubmissionId, finished.item.verdict)); err != nil {
						return err
					}
					continue
				}
				lane.process(ctx, finished.item, finished.data, submission.Fence{WorkerID: r.WorkerID, ControllerID: r.ControllerID, Epoch: lease.Epoch}, subDone)
			default:
				return errors.New("edge frame is not implemented")
			}
		case done := <-authFinished:
			lane.authPending--
			// Replied only while this controller still holds a valid lease.
			if !r.validLease(lease) {
				return ErrLeaseExpired
			}
			if err = send(authResultFrame(done.id, done.verdict)); err != nil {
				return err
			}
		case done := <-subDone:
			lane.processing--
			// The verdict was decided after the durable commit. It may be sent
			// only while this controller still holds a valid lease.
			if !r.validLease(lease) {
				return ErrLeaseExpired
			}
			if err = send(submissionAck(done.id, done.result)); err != nil {
				return err
			}
		case <-subExpiry.C:
			lane.expire()
		case done := <-inboundDone:
			inboundProcessing--
			// The verdict was decided after the durable commit. It may be sent
			// only while this controller still holds a valid lease.
			if !r.validLease(lease) {
				return ErrLeaseExpired
			}
			if err = send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_InboundAck{InboundAck: &controlv1.InboundAcknowledgement{InboundId: done.id, Verdict: done.verdict}}}); err != nil {
				return err
			}
		case <-expiry.C:
			// A partial the worker abandoned (its own timeout) would otherwise
			// hold a slot, and block a drain, forever.
			for id, item := range items {
				if time.Since(item.began) > partialTTL {
					delete(items, id)
					closed[id] = time.Now()
				}
			}
			for id, at := range closed {
				if time.Since(at) > 2*partialTTL {
					delete(closed, id)
				}
			}
		case <-routesTimer.C:
			pushRoutes()
		case push := <-routesDone:
			routesActive = false
			if push.sendErr != nil {
				return push.sendErr
			}
			if push.err != nil && ctx.Err() != nil {
				continue
			}
			if push.sent {
				routesSent, routesHash = true, push.hash
			}
			if push.err != nil {
				// A snapshot that could not be built is skipped; the worker keeps
				// the previous one and answers 451 once the session ends.
				log.Print("failed to build inbound routes")
			}
			if !draining {
				routesTimer.Reset(routesInterval)
			}
		case done := <-committed:
			resultActive = false
			if done.err != nil {
				return done.err
			}
			if err = send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_ResultAck{ResultAck: &controlv1.ResultAcknowledgement{Result: done.result}}}); err != nil {
				return err
			}
			if replayed {
				activeStart = nil
				startAuthorized = false
			}
			if reconciled && !pulled && !draining {
				if err = send(wake()); err != nil {
					return err
				}
			}
		case reconcileErr := <-reconciledResult:
			if reconcileErr != nil {
				return reconcileErr
			}
			if !r.validLease(lease) {
				return ErrLeaseExpired
			}
			ack := &controlv1.ReconciliationComplete{WorkerId: r.WorkerID, JournalId: r.JournalID, SessionId: sessionID, Epoch: lease.Epoch}
			if err = send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_ReconciliationComplete{ReconciliationComplete: ack}}); err != nil {
				return err
			}
			reconciled = true
			if inboundOn {
				pushRoutes()
			}
			if !draining {
				if err = send(wake()); err != nil {
					return err
				}
			}
		case done := <-bounceCommitted:
			bounceActive = false
			if err = send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_BounceAck{BounceAck: &controlv1.BounceAcknowledgement{BounceId: done.id, DurablyAccepted: done.err == nil}}}); err != nil {
				return err
			}
		case done := <-permits:
			permitActive = false
			if done.err != nil {
				return done.err
			}
			if activeStart == nil || !r.validLease(lease) {
				return ErrLeaseExpired
			}
			startAuthorized = true
			if err = send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_StartPermit{StartPermit: &controlv1.StartPermit{Request: done.request}}}); err != nil {
				return err
			}
		case item := <-workItems:
			workActive = false
			if errors.Is(item.err, delivery.ErrNoWork) {
				pollTimer.Reset(r.PollInterval)
				continue
			}
			if item.err != nil {
				return item.err
			}
			if !pulled || item.start == nil || item.start.Epoch != lease.Epoch || !edgeprotocol.VerifyMessage(item.start, item.mime) {
				return errors.New("invalid dispatch work")
			}
			if activeStart != nil {
				return errors.New("delivery already active")
			}
			activeStart = item.start
			pulled = false
			go func() {
				if e := sendDelivery(ctx, send, item.start, item.mime); e != nil {
					select {
					case sentWork <- e:
					case <-ctx.Done():
					}
				} else {
					select {
					case sentWork <- nil:
					case <-ctx.Done():
					}
				}
			}()
		case err := <-sentWork:
			if err != nil {
				return err
			}
		case <-pollTimer.C:
			if pulled && !draining {
				startWork()
			}
		}
	}
}
func (r *Runner) validLease(lease postgres.ControllerLease) bool {
	return lease.WorkerID == r.WorkerID && lease.ControllerID == r.ControllerID && lease.Epoch > 0 && time.Now().Add(r.ClockMargin).Before(lease.ExpiresAt)
}
func guard(ctx context.Context, cancel context.CancelFunc, deadline time.Time, updates <-chan time.Time, expired chan<- struct{}) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case deadline = <-updates:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(time.Until(deadline))
		case <-timer.C:
			select {
			case expired <- struct{}{}:
			default:
			}
			cancel()
			return
		}
	}
}
func wake() *controlv1.ControlFrame {
	return &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Wake{Wake: &controlv1.Wake{}}}
}
func sendDelivery(ctx context.Context, send func(*controlv1.ControlFrame) error, start *controlv1.DeliveryStart, mime []byte) error {
	if err := send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_Delivery{Delivery: start}}); err != nil {
		return err
	}
	for offset := 0; offset < len(mime); {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		end := offset + edgeprotocol.MaxChunkBytes
		if end > len(mime) {
			end = len(mime)
		}
		data := bytes.Clone(mime[offset:end])
		if err := send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_MimeChunk{MimeChunk: &controlv1.DataChunk{Id: start.AttemptId, Offset: uint64(offset), Data: data, Last: end == len(mime)}}}); err != nil {
			return err
		}
		offset = end
	}
	return nil
}
func mapResult(worker, journalID string, result *controlv1.DeliveryResult) (delivery.Result, error) {
	if result == nil {
		return delivery.Result{}, errors.New("missing result")
	}
	outcome := ""
	switch result.Outcome {
	case controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED:
		outcome = "accepted"
	case controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED:
		outcome = "rejected"
	case controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ROUTE_REJECTED:
		outcome = "route_rejected"
	case controlv1.DeliveryOutcome_DELIVERY_OUTCOME_RECIPIENT_POLICY_DENIED:
		outcome = delivery.OutcomeRecipientPolicyDenied
	case controlv1.DeliveryOutcome_DELIVERY_OUTCOME_UNCERTAIN:
		outcome = "uncertain"
	default:
		return delivery.Result{}, errors.New("invalid result")
	}
	// The stage and enhanced code are advisory. A value this control plane
	// cannot validate (or that contradicts the reply code) is dropped rather
	// than failing the result, which would wedge journal replay.
	stage := ""
	switch result.SmtpStage {
	case controlv1.SmtpStage_SMTP_STAGE_MAIL:
		stage = "MAIL"
	case controlv1.SmtpStage_SMTP_STAGE_RCPT:
		stage = "RCPT"
	case controlv1.SmtpStage_SMTP_STAGE_DATA:
		stage = "DATA"
	}
	enhanced := result.SmtpEnhanced
	if result.SmtpCode < 400 || result.SmtpCode > 599 || !journal.ValidEnhanced(enhanced) || int(enhanced[0]-'0') != int(result.SmtpCode)/100 {
		enhanced = ""
	}
	if result.SmtpCode < 400 || result.SmtpCode > 599 {
		stage = ""
	}
	return delivery.Result{AttemptID: result.AttemptId, DeliveryID: result.DeliveryId, WorkerID: worker, JournalID: journalID, Epoch: result.Epoch, Outcome: outcome, SMTPCode: int(result.SmtpCode), SMTPStage: stage, SMTPEnhanced: enhanced}, nil
}

// maxReportedSources and maxReportedBounceDomains bound a worker's Ready lists.
const (
	maxReportedSources       = egress.MaxSources
	maxReportedBounceDomains = 24
)

// capabilitiesFromReady validates a worker's reported sources, their optional
// SMTP names (absent, or one per source), and bounce domains. No sources means a legacy worker, which gets legacy. Reported
// sources must be unique, literal, canonical public IPv4 addresses; the first is
// the primary. Bounce domains must be normalized and unique, and a worker that
// reports domains without sources is malformed.
func capabilitiesFromReady(ready *controlv1.Ready, legacy delivery.EdgeCapabilities) (delivery.EdgeCapabilities, bool) {
	sources, domains, hostnames := ready.GetSourceAddresses(), ready.GetBounceDomains(), ready.GetSourceHostnames()
	if len(sources) == 0 {
		return legacy, len(domains) == 0 && len(hostnames) == 0
	}
	if len(hostnames) != 0 && len(hostnames) != len(sources) {
		return delivery.EdgeCapabilities{}, false
	}
	if len(sources) > maxReportedSources || len(domains) > maxReportedBounceDomains {
		return delivery.EdgeCapabilities{}, false
	}
	caps := delivery.EdgeCapabilities{Explicit: true}
	seen := make(map[netip.Addr]struct{}, len(sources))
	for _, value := range sources {
		address, err := netip.ParseAddr(value)
		if err != nil || address.String() != value || !egress.PublicSource(address) {
			return delivery.EdgeCapabilities{}, false
		}
		if _, duplicate := seen[address]; duplicate {
			return delivery.EdgeCapabilities{}, false
		}
		seen[address] = struct{}{}
		caps.Addresses = append(caps.Addresses, address)
	}
	seenHostnames := make(map[string]struct{}, len(hostnames))
	for _, name := range hostnames {
		normalized, ok := admin.NormalizeDomain(name)
		if !ok || normalized != name || len(name) > 219 {
			return delivery.EdgeCapabilities{}, false
		}
		if _, duplicate := seenHostnames[name]; duplicate {
			return delivery.EdgeCapabilities{}, false
		}
		seenHostnames[name] = struct{}{}
		caps.Hostnames = append(caps.Hostnames, name)
	}
	seenDomains := make(map[string]struct{}, len(domains))
	for _, domain := range domains {
		normalized, ok := admin.NormalizeDomain(domain)
		if !ok || normalized != domain || len(domain) > 219 {
			return delivery.EdgeCapabilities{}, false
		}
		if _, duplicate := seenDomains[domain]; duplicate {
			return delivery.EdgeCapabilities{}, false
		}
		seenDomains[domain] = struct{}{}
		caps.BounceDomains = append(caps.BounceDomains, domain)
	}
	return caps, true
}

// DefaultInboundPartialTTL is how long control keeps an inbound message the
// worker has not finished sending. The worker gives up on a transfer after 60
// seconds, so anything older is abandoned.
const DefaultInboundPartialTTL = 70 * time.Second

// inboundItem is one inbound message being received. A discard item holds no
// data: its chunks are counted and dropped and the message is answered with a
// tempfail.
type inboundItem struct {
	start    *controlv1.InboundStart
	asm      *edgeprotocol.InboundAssembler
	discard  bool
	received uint64
	began    time.Time
}

// routesPush is the outcome of one routing snapshot build and push.
type routesPush struct {
	hash    [32]byte
	sent    bool
	err     error
	sendErr error
}

func (r *Runner) capabilities() []string {
	var caps []string
	if r.Inbound != nil {
		caps = append(caps, edgeprotocol.CapInbound)
	}
	if r.Submission != nil {
		caps = append(caps, edgeprotocol.CapSubmission)
	}
	return caps
}
