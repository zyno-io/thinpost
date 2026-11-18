package controlsession

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/inbound"
)

type fakeInbound struct {
	mu      sync.Mutex
	got     map[string]inbound.Message
	started chan string
	release chan inbound.Verdict
	routes  []inbound.Route
	builds  int
}

func newFakeInbound() *fakeInbound {
	return &fakeInbound{got: map[string]inbound.Message{}, started: make(chan string, 16), release: make(chan inbound.Verdict, 16)}
}

func (f *fakeInbound) Receive(ctx context.Context, m inbound.Message) inbound.Verdict {
	f.mu.Lock()
	f.got[m.ID] = m
	f.mu.Unlock()
	f.started <- m.ID
	select {
	case v := <-f.release:
		return v
	case <-ctx.Done():
		return inbound.Tempfail
	}
}
func (f *fakeInbound) Routes(context.Context) ([]inbound.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.builds++
	return append([]inbound.Route(nil), f.routes...), nil
}
func (f *fakeInbound) setRoutes(routes ...inbound.Route) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes = routes
}

const idA, idB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type inboundHarness struct {
	runner *Runner
	out    chan *controlv1.ControlFrame
	in     chan *controlv1.EdgeFrame
	open   *controlv1.Open
	done   chan error
	cancel context.CancelFunc
}

// startInbound runs a session to the point where reconciliation is complete.
// edgeCap controls whether the worker advertises the capability.
func startInboundSession(t *testing.T, store Inbound, edgeCap bool, tune func(*Runner)) *inboundHarness {
	t.Helper()
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	runner.Inbound = store
	runner.RoutesInterval = 50 * time.Millisecond
	if tune != nil {
		tune(runner)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	h := &inboundHarness{runner: runner, out: out, in: in, done: make(chan error, 1), cancel: cancel}
	go func() { h.done <- runner.Run(ctx) }()
	h.open = (<-out).GetOpen()
	if h.open == nil {
		t.Fatal("missing open")
	}
	ready := readyFrame(h.open)
	if edgeCap {
		ready.GetReady().Capabilities = []string{edgeprotocol.CapInbound}
	}
	in <- ready
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	assertReconciled(t, <-out, h.open)
	return h
}

// next returns the next frame that is not a wake.
func (h *inboundHarness) next(t *testing.T) *controlv1.ControlFrame {
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

func (h *inboundHarness) start(id string, body []byte) {
	digest := sha256.Sum256(body)
	h.in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Inbound{Inbound: &controlv1.InboundStart{InboundId: id, ExpectedSize: uint64(len(body)), Sha256: digest[:], MailFrom: "a@example.org", RcptTo: []string{"u@mail.test"}, ClientIp: "192.0.2.1", Helo: "mx.example.org", TlsVersion: "TLS 1.3", TlsCipher: "TLS_AES_128_GCM_SHA256", ReceivedUnixMs: 1700000000000, Receiver: "smtp.mx.test", Group: 3}}}
}
func (h *inboundHarness) chunk(id string, offset int, data []byte, last bool) {
	h.in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_InboundChunk{InboundChunk: &controlv1.DataChunk{Id: id, Offset: uint64(offset), Data: data, Last: last}}}
}

func TestInboundCapabilityAdvertisedOnlyWithService(t *testing.T) {
	runner, out, _ := setupRunner(&fakeIssuer{}, &fakeResults{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	if open := (<-out).GetOpen(); open == nil || len(open.Capabilities) != 0 {
		t.Fatalf("a control plane without inbound offered %v", open.GetCapabilities())
	}
	h := startInboundSession(t, newFakeInbound(), true, nil)
	if !edgeprotocol.HasCapability(h.open.Capabilities, edgeprotocol.CapInbound) {
		t.Fatal("the capability was not advertised")
	}
}

func TestInboundAcknowledgedOnlyAfterReceiveReturns(t *testing.T) {
	store := newFakeInbound()
	h := startInboundSession(t, store, true, nil)
	// Drain the first routes push so it cannot be mistaken for an acknowledgement.
	if h.next(t).GetRoutes() == nil {
		t.Fatal("no initial routes")
	}
	body := []byte(strings.Repeat("x", edgeprotocol.MaxChunkBytes+5))
	h.start(idA, body)
	h.chunk(idA, 0, body[:edgeprotocol.MaxChunkBytes], false)
	h.chunk(idA, edgeprotocol.MaxChunkBytes, body[edgeprotocol.MaxChunkBytes:], true)
	if id := <-store.started; id != idA {
		t.Fatal(id)
	}
	select {
	case frame := <-h.out:
		if frame.GetInboundAck() != nil {
			t.Fatal("acknowledged before the commit returned")
		}
	case <-time.After(200 * time.Millisecond):
	}
	got := store.got[idA]
	if got.MailFrom != "a@example.org" || len(got.Rcpts) != 1 || got.ClientIP != "192.0.2.1" || got.Helo != "mx.example.org" || got.TLS.Version != "TLS 1.3" || got.Receiver != "smtp.mx.test" || got.Group != 3 || string(got.Body) != string(body) || got.Received.UnixMilli() != 1700000000000 {
		t.Fatalf("%+v", got)
	}
	store.release <- inbound.Accepted
	for {
		frame := h.next(t)
		if ack := frame.GetInboundAck(); ack != nil {
			if ack.InboundId != idA || ack.Verdict != controlv1.InboundVerdict_INBOUND_VERDICT_ACCEPTED {
				t.Fatalf("%v", ack)
			}
			break
		}
	}
}

func TestInboundVerdictsAreMapped(t *testing.T) {
	for verdict, want := range map[inbound.Verdict]controlv1.InboundVerdict{
		inbound.Accepted: controlv1.InboundVerdict_INBOUND_VERDICT_ACCEPTED,
		inbound.Tempfail: controlv1.InboundVerdict_INBOUND_VERDICT_TEMPFAIL,
		inbound.Rejected: controlv1.InboundVerdict_INBOUND_VERDICT_REJECTED,
	} {
		store := newFakeInbound()
		h := startInboundSession(t, store, true, nil)
		h.start(idA, []byte("x"))
		h.chunk(idA, 0, []byte("x"), true)
		<-store.started
		store.release <- verdict
		for {
			if ack := h.next(t).GetInboundAck(); ack != nil {
				if ack.Verdict != want {
					t.Fatalf("%v mapped to %v", verdict, ack.Verdict)
				}
				break
			}
		}
		h.cancel()
	}
}

func TestInboundAssemblersRunConcurrentlyAndAFifthIsTempfailed(t *testing.T) {
	store := newFakeInbound()
	h := startInboundSession(t, store, true, nil)
	ids := []string{"00000000000000000000000000000001", "00000000000000000000000000000002", "00000000000000000000000000000003", "00000000000000000000000000000004"}
	// All four start before any finishes, and their chunks interleave.
	for _, id := range ids {
		h.start(id, []byte("ab"))
	}
	for _, id := range ids {
		h.chunk(id, 0, []byte("a"), false)
	}
	for _, id := range ids {
		h.chunk(id, 1, []byte("b"), true)
	}
	for range ids {
		<-store.started
	}
	// Four are being committed. A fifth means the worker's count drifted from
	// ours: it is answered with a tempfail (its chunks swallowed) and the session
	// carries on.
	fifth := "00000000000000000000000000000005"
	h.start(fifth, []byte("xyz"))
	h.chunk(fifth, 0, []byte("xy"), false)
	h.chunk(fifth, 2, []byte("z"), true)
	for {
		if ack := h.next(t).GetInboundAck(); ack != nil {
			if ack.InboundId != fifth || ack.Verdict != controlv1.InboundVerdict_INBOUND_VERDICT_TEMPFAIL {
				t.Fatalf("%v", ack)
			}
			break
		}
	}
	if len(store.got) != 4 {
		t.Fatal("the over-limit message reached the intake")
	}
	// The four committing messages are unaffected.
	for range ids {
		store.release <- inbound.Accepted
	}
	acked := 0
	for acked < 4 {
		if ack := h.next(t).GetInboundAck(); ack != nil && ack.Verdict == controlv1.InboundVerdict_INBOUND_VERDICT_ACCEPTED {
			acked++
		}
	}
	select {
	case err := <-h.done:
		t.Fatalf("the session ended: %v", err)
	default:
	}
}

// An envelope that can never be valid is answered REJECTED; only framing
// violations end the session.
func TestInboundInvalidEnvelopeIsRejectedNotFatal(t *testing.T) {
	store := newFakeInbound()
	h := startInboundSession(t, store, true, nil)
	digest := sha256.Sum256([]byte("x"))
	h.in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Inbound{Inbound: &controlv1.InboundStart{InboundId: idA, ExpectedSize: 1, Sha256: digest[:], MailFrom: strings.Repeat("a", 411), Helo: strings.Repeat("h", 601), RcptTo: []string{"u@mail.test"}}}}
	h.chunk(idA, 0, []byte("x"), true)
	for {
		if ack := h.next(t).GetInboundAck(); ack != nil {
			if ack.InboundId != idA || ack.Verdict != controlv1.InboundVerdict_INBOUND_VERDICT_REJECTED {
				t.Fatalf("%v", ack)
			}
			break
		}
	}
	if len(store.got) != 0 {
		t.Fatal("an invalid envelope reached the intake")
	}
	// The next message is received normally.
	h.start(idB, []byte("x"))
	h.chunk(idB, 0, []byte("x"), true)
	<-store.started
	select {
	case err := <-h.done:
		t.Fatalf("the session ended: %v", err)
	default:
	}
}

// A message the worker abandoned (its own timeout) is dropped after the TTL, a
// straggling chunk is ignored, and a drain does not wait for it.
func TestInboundAbandonedPartialsExpire(t *testing.T) {
	store := newFakeInbound()
	h := startInboundSession(t, store, true, func(r *Runner) { r.InboundPartialTTL = 400 * time.Millisecond })
	for _, id := range []string{"00000000000000000000000000000001", "00000000000000000000000000000002", "00000000000000000000000000000003", "00000000000000000000000000000004"} {
		h.start(id, []byte("ab"))
		h.chunk(id, 0, []byte("a"), false)
	}
	time.Sleep(800 * time.Millisecond)
	// The slots are free again: a new message is received, not tempfailed.
	h.start(idA, []byte("x"))
	h.chunk(idA, 0, []byte("x"), true)
	select {
	case <-store.started:
	case <-time.After(3 * time.Second):
		t.Fatal("expired partials still hold their slots")
	}
	// A late chunk of an expired item is ignored.
	h.chunk("00000000000000000000000000000001", 1, []byte("b"), true)
	store.release <- inbound.Accepted
	for {
		if ack := h.next(t).GetInboundAck(); ack != nil {
			if ack.InboundId != idA || ack.Verdict != controlv1.InboundVerdict_INBOUND_VERDICT_ACCEPTED {
				t.Fatalf("%v", ack)
			}
			break
		}
	}
	select {
	case err := <-h.done:
		t.Fatalf("a straggling chunk ended the session: %v", err)
	default:
	}
}

func TestInboundDrainDoesNotWaitForAbandonedPartial(t *testing.T) {
	store := newFakeInbound()
	drain := make(chan struct{})
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	runner.Inbound, runner.RoutesInterval, runner.InboundPartialTTL = store, 50*time.Millisecond, 400*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.RunDraining(ctx, drain) }()
	open := (<-out).GetOpen()
	ready := readyFrame(open)
	ready.GetReady().Capabilities = []string{edgeprotocol.CapInbound}
	in <- ready
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	assertReconciled(t, <-out, open)
	digest := sha256.Sum256([]byte("ab"))
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Inbound{Inbound: &controlv1.InboundStart{InboundId: idA, ExpectedSize: 2, Sha256: digest[:], RcptTo: []string{"u@m.t"}}}}
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_InboundChunk{InboundChunk: &controlv1.DataChunk{Id: idA, Data: []byte("a")}}}
	close(drain)
	go func() {
		for range out {
		}
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrDraining) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the drain waited for an abandoned partial")
	}
}

// slowRoutes blocks its first Routes call so a periodic push is in flight when
// the drain begins.
type slowRoutes struct {
	*fakeInbound
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *slowRoutes) Routes(ctx context.Context) ([]inbound.Route, error) {
	blocked := false
	s.once.Do(func() { blocked = true })
	if blocked {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
		}
	}
	return s.fakeInbound.Routes(ctx)
}

func TestInboundDrainSnapshotNotSkippedWhilePushInFlight(t *testing.T) {
	store := &slowRoutes{fakeInbound: newFakeInbound(), entered: make(chan struct{}), release: make(chan struct{})}
	store.setRoutes(inbound.Route{Domain: "a.test", AppID: idA, Accept: true})
	drain := make(chan struct{})
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	runner.Inbound, runner.RoutesInterval = store, 50*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.RunDraining(ctx, drain) }()
	open := (<-out).GetOpen()
	ready := readyFrame(open)
	ready.GetReady().Capabilities = []string{edgeprotocol.CapInbound}
	in <- ready
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	assertReconciled(t, <-out, open)
	<-store.entered // the first push is building its snapshot
	close(drain)
	time.Sleep(100 * time.Millisecond)
	close(store.release)
	var states []controlv1.InboundRouteState
	deadline := time.After(5 * time.Second)
	for {
		select {
		case frame := <-out:
			if page := frame.GetRoutes(); page != nil {
				states = append(states, page.Routes[0].State)
			}
			continue
		case err := <-done:
			if !errors.Is(err, ErrDraining) {
				t.Fatal(err)
			}
		case <-deadline:
			t.Fatal("no drain")
		}
		break
	}
	// Frames sent before the session ended may still be queued.
	for drained := false; !drained; {
		select {
		case frame := <-out:
			if page := frame.GetRoutes(); page != nil {
				states = append(states, page.Routes[0].State)
			}
		default:
			drained = true
		}
	}
	if len(states) == 0 || states[len(states)-1] != controlv1.InboundRouteState_INBOUND_ROUTE_STATE_TEMPFAIL {
		t.Fatalf("the worker was never told to answer 451 on drain: %v", states)
	}
}

func TestInboundRefusedWithoutCapabilityOrService(t *testing.T) {
	// The worker did not advertise the capability.
	h := startInboundSession(t, newFakeInbound(), false, nil)
	h.start(idA, []byte("x"))
	if err := <-h.done; err == nil {
		t.Fatal("inbound accepted from a worker without the capability")
	}
	// No inbound service configured at all.
	h = startInboundSession(t, nil, true, nil)
	h.start(idA, []byte("x"))
	if err := <-h.done; err == nil {
		t.Fatal("inbound accepted without a service")
	}
}

func TestInboundProtocolViolationsEndTheSession(t *testing.T) {
	for name, send := range map[string]func(h *inboundHarness){
		"unknown chunk":   func(h *inboundHarness) { h.chunk(idA, 0, []byte("x"), true) },
		"duplicate start": func(h *inboundHarness) { h.start(idA, []byte("xy")); h.start(idA, []byte("xy")) },
		"bad digest":      func(h *inboundHarness) { h.start(idA, []byte("xy")); h.chunk(idA, 0, []byte("xz"), true) },
		"zero size":       func(h *inboundHarness) { h.start(idA, nil) },
		"gap":             func(h *inboundHarness) { h.start(idA, []byte("xy")); h.chunk(idA, 1, []byte("y"), true) },
		"oversize declare": func(h *inboundHarness) {
			h.in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Inbound{Inbound: &controlv1.InboundStart{InboundId: idA, ExpectedSize: edgeprotocol.MaxInboundAssembleBytes + 1, Sha256: make([]byte, 32), RcptTo: []string{"u@m.t"}}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := startInboundSession(t, newFakeInbound(), true, nil)
			send(h)
			if err := <-h.done; err == nil {
				t.Fatal("violation accepted")
			}
		})
	}
}

func TestInboundRoutesPushedOnlyOnChange(t *testing.T) {
	store := newFakeInbound()
	store.setRoutes(inbound.Route{Domain: "b.test", AppID: idB, Accept: true}, inbound.Route{Domain: "a.test", AppID: idA, Accept: false})
	h := startInboundSession(t, store, true, nil)
	page := h.next(t).GetRoutes()
	if page == nil || page.Offset != 0 || !page.Last || len(page.Routes) != 2 || page.Routes[0].Domain != "a.test" || page.Routes[0].Group != 1 || page.Routes[0].State != controlv1.InboundRouteState_INBOUND_ROUTE_STATE_TEMPFAIL || page.Routes[1].Group != 2 || page.Routes[1].State != controlv1.InboundRouteState_INBOUND_ROUTE_STATE_ACCEPT {
		t.Fatalf("first snapshot %v", page)
	}
	// Unchanged for several rebuilds: nothing more is sent.
	time.Sleep(400 * time.Millisecond)
	select {
	case frame := <-h.out:
		if frame.GetWake() == nil {
			t.Fatalf("an unchanged snapshot was resent: %v", frame)
		}
	default:
	}
	store.mu.Lock()
	builds := store.builds
	store.mu.Unlock()
	if builds < 3 {
		t.Fatalf("the snapshot is not rebuilt periodically: %d", builds)
	}
	store.setRoutes(inbound.Route{Domain: "a.test", AppID: idA, Accept: true})
	page = h.next(t).GetRoutes()
	if page == nil || len(page.Routes) != 1 || page.Routes[0].State != controlv1.InboundRouteState_INBOUND_ROUTE_STATE_ACCEPT {
		t.Fatalf("changed snapshot %v", page)
	}
}

func TestInboundRoutesTempfailWhileDraining(t *testing.T) {
	store := newFakeInbound()
	store.setRoutes(inbound.Route{Domain: "a.test", AppID: idA, Accept: true})
	drain := make(chan struct{})
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	runner.Inbound, runner.RoutesInterval = store, 50*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.RunDraining(ctx, drain) }()
	open := (<-out).GetOpen()
	ready := readyFrame(open)
	ready.GetReady().Capabilities = []string{edgeprotocol.CapInbound}
	in <- ready
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	assertReconciled(t, <-out, open)
	var first *controlv1.InboundRoutes
	for first == nil {
		first = (<-out).GetRoutes()
	}
	if first.Routes[0].State != controlv1.InboundRouteState_INBOUND_ROUTE_STATE_ACCEPT {
		t.Fatal("not accepting before the drain")
	}
	close(drain)
	var drained *controlv1.InboundRoutes
	for drained == nil {
		select {
		case frame := <-out:
			drained = frame.GetRoutes()
		case <-time.After(5 * time.Second):
			t.Fatal("no tempfail snapshot on drain")
		}
	}
	if drained.Routes[0].State != controlv1.InboundRouteState_INBOUND_ROUTE_STATE_TEMPFAIL {
		t.Fatal("a draining controller still accepts at RCPT")
	}
	if err := <-done; !errors.Is(err, ErrDraining) {
		t.Fatalf("drain: %v", err)
	}
}

// A drain waits for an inbound commit that is still running.
func TestInboundDrainWaitsForCommit(t *testing.T) {
	store := newFakeInbound()
	drain := make(chan struct{})
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	runner.Inbound, runner.RoutesInterval = store, 50*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.RunDraining(ctx, drain) }()
	open := (<-out).GetOpen()
	ready := readyFrame(open)
	ready.GetReady().Capabilities = []string{edgeprotocol.CapInbound}
	in <- ready
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	assertReconciled(t, <-out, open)
	digest := sha256.Sum256([]byte("x"))
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_Inbound{Inbound: &controlv1.InboundStart{InboundId: idA, ExpectedSize: 1, Sha256: digest[:], RcptTo: []string{"u@m.t"}}}}
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_InboundChunk{InboundChunk: &controlv1.DataChunk{Id: idA, Data: []byte("x"), Last: true}}}
	<-store.started
	close(drain)
	select {
	case err := <-done:
		t.Fatalf("drained with a commit in flight: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	store.release <- inbound.Accepted
	var acked bool
	for !acked {
		select {
		case frame := <-out:
			acked = frame.GetInboundAck() != nil
		case <-time.After(5 * time.Second):
			t.Fatal("no acknowledgement")
		}
	}
	if err := <-done; !errors.Is(err, ErrDraining) {
		t.Fatalf("drain: %v", err)
	}
}

// With the lease no longer valid when the commit returns, no acknowledgement is
// sent: the Edge answers 451 and the sender retries.
func TestInboundNotAcknowledgedAfterLeaseExpiry(t *testing.T) {
	store := newFakeInbound()
	h := startInboundSession(t, store, true, func(r *Runner) { r.LeaseTTL, r.ClockMargin = 5*time.Second, 2400*time.Millisecond })
	h.start(idA, []byte("x"))
	h.chunk(idA, 0, []byte("x"), true)
	<-store.started
	time.Sleep(2800 * time.Millisecond)
	store.release <- inbound.Accepted
	err := <-h.done
	if !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("%v", err)
	}
	for {
		select {
		case frame := <-h.out:
			if frame.GetInboundAck() != nil {
				t.Fatal("acknowledged without a valid lease")
			}
		default:
			return
		}
	}
}

func TestRoutePages(t *testing.T) {
	var routes []inbound.Route
	for i := 0; i < 3000; i++ {
		routes = append(routes, inbound.Route{Domain: fmt.Sprintf("d%04d.%s.example.com", i, strings.Repeat("x", 50)), AppID: fmt.Sprintf("%032x", i%7), Accept: i%2 == 0})
	}
	routes = append(routes, inbound.Route{Domain: "UPPER.example.com", AppID: idA}, inbound.Route{Domain: "bad..example.com", AppID: idA})
	pages, hash, err := routePages(routes, false)
	if err != nil || len(pages) < 2 {
		t.Fatalf("%d pages, %v", len(pages), err)
	}
	var assembler edgeprotocol.RoutesAssembler
	var complete []*controlv1.InboundRoute
	for i, frame := range pages {
		page := frame.GetRoutes()
		size := 0
		for _, route := range page.Routes {
			size += edgeprotocol.RoutePageSize(route)
		}
		if size > edgeprotocol.MaxRoutePageBytes {
			t.Fatalf("page %d is %d bytes", i, size)
		}
		got, done, err := assembler.Add(page)
		if err != nil || done != (i == len(pages)-1) {
			t.Fatalf("page %d: %v %v", i, done, err)
		}
		if done {
			complete = got
		}
	}
	if len(complete) != 3000 {
		t.Fatalf("%d routes (invalid names must be skipped)", len(complete))
	}
	// Apps are numbered from one in a stable order.
	groups := map[uint32]bool{}
	for _, r := range complete {
		groups[r.Group] = true
	}
	if len(groups) != 7 || !groups[1] || !groups[7] {
		t.Fatalf("groups %v", groups)
	}
	_, again, _ := routePages(routes, false)
	_, tempfail, _ := routePages(routes, true)
	if again != hash || tempfail == hash {
		t.Fatal("the hash must be stable and change with the states")
	}
	empty, emptyHash, err := routePages(nil, false)
	if err != nil || len(empty) != 1 || !empty[0].GetRoutes().Last || emptyHash == hash {
		t.Fatal("empty snapshot")
	}
	if _, _, err = routePages(make([]inbound.Route, edgeprotocol.MaxRoutes+1), false); err == nil {
		t.Fatal("over-large snapshot accepted")
	}
}

// Only items that expired or were discarded are remembered: many completed
// messages do not push an abandoned one's straggler out of memory, and the
// straggling chunk is ignored.
func TestInboundStragglerAfterManyCompletedMessages(t *testing.T) {
	store := newFakeInbound()
	h := startInboundSession(t, store, true, func(r *Runner) { r.InboundPartialTTL = 800 * time.Millisecond })
	abandoned := "00000000000000000000000000000abc"
	h.start(abandoned, []byte("ab"))
	h.chunk(abandoned, 0, []byte("a"), false)
	time.Sleep(1300 * time.Millisecond) // expires (the worker gave up)
	for i := 0; i < 300; i++ {
		id := fmt.Sprintf("%032x", i+1)
		h.start(id, []byte("x"))
		h.chunk(id, 0, []byte("x"), true)
		<-store.started
		store.release <- inbound.Accepted
		for {
			if ack := h.next(t).GetInboundAck(); ack != nil {
				break
			}
		}
	}
	h.chunk(abandoned, 1, []byte("b"), true)
	// A chunk after a completed message's last chunk is still a violation.
	select {
	case err := <-h.done:
		t.Fatalf("the straggler ended the session: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	h.chunk(fmt.Sprintf("%032x", 5), 1, []byte("y"), true)
	if err := <-h.done; err == nil {
		t.Fatal("a chunk after completion was ignored")
	}
}

// The receive budget starts at InboundStart: a slow transfer leaves less.
func TestInboundReceiveDeadlineStartsAtTheStart(t *testing.T) {
	deadlines := make(chan time.Time, 1)
	store := &deadlineInbound{fakeInbound: newFakeInbound(), deadlines: deadlines}
	h := startInboundSession(t, store, true, nil)
	h.start(idA, []byte("ab"))
	h.chunk(idA, 0, []byte("a"), false)
	time.Sleep(600 * time.Millisecond)
	h.chunk(idA, 1, []byte("b"), true)
	select {
	case d := <-deadlines:
		if remaining := time.Until(d); remaining > inbound.ReceiveTimeout-500*time.Millisecond || remaining < inbound.ReceiveTimeout-5*time.Second {
			t.Fatalf("%v left of the budget", remaining)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("not received")
	}
}

type deadlineInbound struct {
	*fakeInbound
	deadlines chan time.Time
}

func (d *deadlineInbound) Receive(ctx context.Context, m inbound.Message) inbound.Verdict {
	deadline, _ := ctx.Deadline()
	d.deadlines <- deadline
	return inbound.Accepted
}

// A drain before reconciliation has nothing to tell the worker and sends no
// routes, and still ends.
func TestInboundDrainBeforeReconciliationSendsNoRoutes(t *testing.T) {
	store := newFakeInbound()
	store.setRoutes(inbound.Route{Domain: "a.test", AppID: idA, Accept: true})
	drain := make(chan struct{})
	gated := &gatedReconciler{started: make(chan struct{}), release: make(chan error, 1)}
	runner, out, in := setupRunner(&fakeIssuer{}, &fakeResults{})
	runner.Reconciler = gated
	runner.Inbound, runner.RoutesInterval = store, 50*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.RunDraining(ctx, drain) }()
	open := (<-out).GetOpen()
	ready := readyFrame(open)
	ready.GetReady().Capabilities = []string{edgeprotocol.CapInbound}
	in <- ready
	in <- &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_ReplayComplete{ReplayComplete: &controlv1.ReplayComplete{}}}
	<-gated.started
	close(drain)
	select {
	case err := <-done:
		if !errors.Is(err, ErrDraining) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no drain")
	}
	for {
		select {
		case frame := <-out:
			if frame.GetRoutes() != nil {
				t.Fatal("routes were sent before reconciliation")
			}
		default:
			return
		}
	}
}
