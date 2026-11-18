package edgesession

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/bounce"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

const inboundMessageID = "0123456789abcdef0123456789abcdef"

type inboundSession struct {
	s      *Server
	stream *channelStream
	done   chan error
	ctx    context.Context
}

// startInbound opens a session against a worker with inbound enabled. The
// control side advertises the capability only when withCap is set.
func startInbound(t *testing.T, withCap bool) *inboundSession {
	t.Helper()
	s, _, _, _ := setup(t, true)
	s.EnableInbound()
	return startInboundSession(t, s, withCap)
}

func startInboundSession(t *testing.T, s *Server, withCap bool) *inboundSession {
	t.Helper()
	ctx := verifiedContext(t, "spiffe://thinpost.test/staging/controller/one")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	t.Cleanup(cancel)
	stream := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 64), sent: make(chan *controlv1.EdgeFrame, 64)}
	done := make(chan error, 1)
	go func() { done <- s.Connect(stream) }()
	open := openFrame(s, 1)
	if withCap {
		open.GetOpen().Capabilities = []string{edgeprotocol.CapInbound}
	}
	stream.incoming <- open
	ready := <-stream.sent
	if ready.GetReady() == nil {
		t.Fatal("no ready frame")
	}
	if got := edgeprotocol.HasCapability(ready.GetReady().Capabilities, edgeprotocol.CapInbound); !got {
		t.Fatal("an inbound-enabled worker did not advertise the capability")
	}
	if (<-stream.sent).GetReplayComplete() == nil {
		t.Fatal("missing replay barrier")
	}
	stream.incoming <- reconcileFrame(s, 1)
	return &inboundSession{s: s, stream: stream, done: done, ctx: ctx}
}

func (is *inboundSession) routes(offset uint32, last bool, routes ...*controlv1.InboundRoute) {
	is.stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_Routes{Routes: &controlv1.InboundRoutes{Offset: offset, Last: last, Routes: routes}}}
}

func route(domain string, group uint32, state controlv1.InboundRouteState) *controlv1.InboundRoute {
	return &controlv1.InboundRoute{Domain: domain, Group: group, State: state}
}

const (
	accept   = controlv1.InboundRouteState_INBOUND_ROUTE_STATE_ACCEPT
	tempfail = controlv1.InboundRouteState_INBOUND_ROUTE_STATE_TEMPFAIL
)

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func lookup(s *Server, domain string) bounce.RouteState {
	_, state := s.Inbound().Lookup(domain)
	return state
}

func TestSnapshotPagingAndAtomicSwap(t *testing.T) {
	is := startInbound(t, true)
	is.routes(0, true, route("a.example", 1, accept), route("b.example", 2, tempfail))
	waitFor(t, "first snapshot", func() bool { return lookup(is.s, "a.example") == bounce.RouteAccept })
	if group, state := is.s.Inbound().Lookup("B.Example"); state != bounce.RouteTempfailState || group != 2 {
		t.Fatalf("tempfail route: %d %v", group, state)
	}
	if lookup(is.s, "other.example") != bounce.RouteUnknown {
		t.Fatal("an unlisted domain is not refused")
	}
	// A new snapshot arrives in two pages. Until the last page the old one is
	// used in full; afterwards only the new one.
	is.routes(0, false, route("c.example", 1, accept))
	time.Sleep(50 * time.Millisecond)
	if lookup(is.s, "a.example") != bounce.RouteAccept || lookup(is.s, "c.example") != bounce.RouteUnknown {
		t.Fatal("a partial snapshot was applied")
	}
	is.routes(1, true, route("d.example", 1, accept))
	waitFor(t, "second snapshot", func() bool { return lookup(is.s, "c.example") == bounce.RouteAccept })
	if lookup(is.s, "a.example") != bounce.RouteUnknown || lookup(is.s, "d.example") != bounce.RouteAccept {
		t.Fatal("the old snapshot leaked into the new one")
	}
	// A page that does not continue the snapshot ends the session.
	is.routes(7, true, route("e.example", 1, accept))
	if err := <-is.done; err == nil {
		t.Fatal("an out-of-order routes page was accepted")
	}
	// Without a live session the snapshot answers tempfail, never accept.
	if lookup(is.s, "c.example") != bounce.RouteTempfailNoSession || lookup(is.s, "other.example") != bounce.RouteUnknown {
		t.Fatal("snapshot without a session")
	}
}

// Which answer a worker gives for a domain depends on what it knows: with no
// snapshot yet (a restart or reconnect) it queues the sender with 451, because
// live mail must not bounce; once a control plane without the capability has
// reconciled it answers 550 as it always did; with a snapshot but no session its
// domains are 451 and the others 550.
func TestRoutingStatesWithoutASnapshot(t *testing.T) {
	disabled, _, _, _ := setup(t, true)
	if lookup(disabled, "a.example") != bounce.RouteUnknown {
		t.Fatal("a worker with inbound disabled accepted or deferred a domain")
	}
	s, _, _, _ := setup(t, true)
	s.EnableInbound()
	if lookup(s, "a.example") != bounce.RouteTempfailNoSession {
		t.Fatal("a freshly started worker with no snapshot must answer 451, not 550")
	}
	// A session with the capability that has not pushed a snapshot yet: still 451.
	is := startInboundSession(t, s, true)
	waitFor(t, "attach", func() bool { is.s.inbound.mu.Lock(); defer is.s.inbound.mu.Unlock(); return is.s.inbound.active != nil })
	if lookup(s, "a.example") != bounce.RouteTempfailNoSession {
		t.Fatal("before the first snapshot")
	}
	is.routes(0, true, route("a.example", 1, accept))
	waitFor(t, "snapshot", func() bool { return lookup(s, "a.example") == bounce.RouteAccept })
	cancelSession(is)
	if lookup(s, "a.example") != bounce.RouteTempfailNoSession || lookup(s, "b.example") != bounce.RouteUnknown {
		t.Fatal("snapshot with the session down: its domains 451, others 550")
	}
	// An old control plane reconciles: 550 for everything.
	old := startInboundOn(t, s, false)
	waitFor(t, "old control", func() bool { return lookup(s, "a.example") == bounce.RouteUnknown })
	cancelSession(old)
	if lookup(s, "a.example") != bounce.RouteUnknown {
		t.Fatal("after an old control plane the worker must refuse")
	}
	// A capable control plane returns: back to 451 until it pushes a snapshot.
	again := startInboundOn(t, s, true)
	waitFor(t, "capable again", func() bool { return lookup(s, "a.example") == bounce.RouteTempfailNoSession })
	cancelSession(again)
}

// A control plane without the capability gets no inbound frames from the worker
// and cannot send any; the old snapshot is forgotten so the worker answers 550.
func TestInboundFramesRefusedWithoutCapability(t *testing.T) {
	is := startInbound(t, true)
	is.routes(0, true, route("a.example", 1, accept))
	waitFor(t, "snapshot", func() bool { return lookup(is.s, "a.example") == bounce.RouteAccept })
	cancelSession(is)

	old := startInboundOn(t, is.s, false)
	waitFor(t, "reconciliation", func() bool { return lookup(is.s, "a.example") == bounce.RouteUnknown })
	old.routes(0, true, route("a.example", 1, accept))
	if err := <-old.done; err == nil {
		t.Fatal("routes accepted from a control plane that did not advertise the capability")
	}
	if _, err := is.s.Inbound().Forward(context.Background(), bounce.Message{ID: inboundMessageID, Rcpts: []string{"a@a.example"}, Body: []byte("x")}); err == nil {
		t.Fatal("forwarded with no inbound session")
	}
}

func TestInboundAckWithoutCapabilityIsAnError(t *testing.T) {
	is := startInbound(t, false)
	is.stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_InboundAck{InboundAck: &controlv1.InboundAcknowledgement{InboundId: inboundMessageID, Verdict: controlv1.InboundVerdict_INBOUND_VERDICT_ACCEPTED}}}
	if err := <-is.done; err == nil {
		t.Fatal("an inbound acknowledgement was accepted without the capability")
	}
}

func cancelSession(is *inboundSession) {
	close(is.stream.incoming)
	<-is.done
}

// startInboundOn opens another session against the same worker.
func startInboundOn(t *testing.T, s *Server, withCap bool) *inboundSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(verifiedContext(t, "spiffe://thinpost.test/staging/controller/one"), 10*time.Second)
	t.Cleanup(cancel)
	stream := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 64), sent: make(chan *controlv1.EdgeFrame, 64)}
	done := make(chan error, 1)
	go func() { done <- s.Connect(stream) }()
	open := openFrame(s, 2)
	if withCap {
		open.GetOpen().Capabilities = []string{edgeprotocol.CapInbound}
	}
	stream.incoming <- open
	if (<-stream.sent).GetReady() == nil || (<-stream.sent).GetReplayComplete() == nil {
		t.Fatal("no replay barrier")
	}
	stream.incoming <- reconcileFrame(s, 2)
	return &inboundSession{s: s, stream: stream, done: done, ctx: ctx}
}

func forward(is *inboundSession, id string, body []byte) chan struct {
	verdict bounce.Verdict
	err     error
} {
	result := make(chan struct {
		verdict bounce.Verdict
		err     error
	}, 1)
	go func() {
		v, err := is.s.Inbound().Forward(is.ctx, bounce.Message{ID: id, MailFrom: "sender@example.org", Rcpts: []string{"user@a.example"}, Helo: "mx.example.org", Receiver: "smtp.mx.example", Group: 1, Body: body})
		result <- struct {
			verdict bounce.Verdict
			err     error
		}{v, err}
	}()
	return result
}

func readInbound(t *testing.T, is *inboundSession) (*controlv1.InboundStart, []byte) {
	t.Helper()
	frame := <-is.stream.sent
	start := frame.GetInbound()
	if start == nil {
		t.Fatalf("expected an inbound start, got %v", frame)
	}
	var body []byte
	for {
		chunk := (<-is.stream.sent).GetInboundChunk()
		if chunk == nil || chunk.Id != start.InboundId || chunk.Offset != uint64(len(body)) {
			t.Fatal("bad chunk")
		}
		body = append(body, chunk.Data...)
		if chunk.Last {
			break
		}
	}
	digest := sha256.Sum256(body)
	if string(digest[:]) != string(start.Sha256) || uint64(len(body)) != start.ExpectedSize {
		t.Fatal("start does not describe the body")
	}
	return start, body
}

func (is *inboundSession) ack(id string, verdict controlv1.InboundVerdict) {
	is.stream.incoming <- &controlv1.ControlFrame{Body: &controlv1.ControlFrame_InboundAck{InboundAck: &controlv1.InboundAcknowledgement{InboundId: id, Verdict: verdict}}}
}

func TestForwardReturnsVerdictOnlyAfterAcknowledgement(t *testing.T) {
	is := startInbound(t, true)
	waitFor(t, "attach", func() bool { is.s.inbound.mu.Lock(); defer is.s.inbound.mu.Unlock(); return is.s.inbound.active != nil })
	body := make([]byte, 3*edgeprotocol.MaxChunkBytes+17)
	for i := range body {
		body[i] = byte(i)
	}
	result := forward(is, inboundMessageID, body)
	start, got := readInbound(t, is)
	if start.MailFrom != "sender@example.org" || len(start.RcptTo) != 1 || start.Receiver != "smtp.mx.example" || string(got) != string(body) {
		t.Fatalf("start %v", start)
	}
	select {
	case <-result:
		t.Fatal("verdict returned before the acknowledgement")
	case <-time.After(50 * time.Millisecond):
	}
	// An acknowledgement for another ID is ignored.
	is.ack("abcdef0123456789abcdef0123456789", controlv1.InboundVerdict_INBOUND_VERDICT_ACCEPTED)
	select {
	case <-result:
		t.Fatal("another message's acknowledgement completed this one")
	case <-time.After(50 * time.Millisecond):
	}
	is.ack(inboundMessageID, controlv1.InboundVerdict_INBOUND_VERDICT_ACCEPTED)
	if r := <-result; r.err != nil || r.verdict != bounce.VerdictAccepted {
		t.Fatalf("verdict %v %v", r.verdict, r.err)
	}
	for verdict, want := range map[controlv1.InboundVerdict]bounce.Verdict{controlv1.InboundVerdict_INBOUND_VERDICT_TEMPFAIL: bounce.VerdictTempfail, controlv1.InboundVerdict_INBOUND_VERDICT_REJECTED: bounce.VerdictRejected} {
		result = forward(is, "fedcba9876543210fedcba9876543210", []byte("x"))
		readInbound(t, is)
		is.ack("fedcba9876543210fedcba9876543210", verdict)
		if r := <-result; r.err != nil || r.verdict != want {
			t.Fatalf("verdict %v: %v %v", verdict, r.verdict, r.err)
		}
	}
}

func TestFifthConcurrentInboundIsRefused(t *testing.T) {
	is := startInbound(t, true)
	waitFor(t, "attach", func() bool { is.s.inbound.mu.Lock(); defer is.s.inbound.mu.Unlock(); return is.s.inbound.active != nil })
	ids := []string{"00000000000000000000000000000001", "00000000000000000000000000000002", "00000000000000000000000000000003", "00000000000000000000000000000004"}
	var results []chan struct {
		verdict bounce.Verdict
		err     error
	}
	for _, id := range ids {
		results = append(results, forward(is, id, []byte("x")))
	}
	// Starts and chunks of concurrent messages interleave on the stream.
	starts, chunks := 0, 0
	for starts+chunks < 2*len(ids) {
		frame := <-is.stream.sent
		switch {
		case frame.GetInbound() != nil:
			starts++
		case frame.GetInboundChunk() != nil:
			chunks++
		default:
			t.Fatalf("unexpected frame %v", frame)
		}
	}
	fifth := forward(is, "00000000000000000000000000000005", []byte("x"))
	if r := <-fifth; r.err == nil {
		t.Fatal("a fifth in-flight inbound message was forwarded")
	}
	for i, id := range ids {
		is.ack(id, controlv1.InboundVerdict_INBOUND_VERDICT_ACCEPTED)
		if r := <-results[i]; r.err != nil || r.verdict != bounce.VerdictAccepted {
			t.Fatalf("%s: %v %v", id, r.verdict, r.err)
		}
	}
}

func TestSessionEndFailsInFlightAndDetaches(t *testing.T) {
	is := startInbound(t, true)
	is.routes(0, true, route("a.example", 1, accept))
	waitFor(t, "snapshot", func() bool { return lookup(is.s, "a.example") == bounce.RouteAccept })
	result := forward(is, inboundMessageID, []byte("x"))
	readInbound(t, is)
	cancelSession(is)
	if r := <-result; r.err == nil {
		t.Fatal("an in-flight message survived its session")
	}
	if lookup(is.s, "a.example") != bounce.RouteTempfailNoSession {
		t.Fatal("a closed session still accepts")
	}
}

func TestAcknowledgementIgnoredWhenLeaseExpired(t *testing.T) {
	s, _, _, ctx := setup(t, true)
	// A grant that can be revoked while the session runs.
	grantState := &mutableGrant{}
	grantState.valid.Store(true)
	s.authority = mutableAuthority{grant: grantState}
	s.EnableInbound()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream := &channelStream{ctx: ctx, incoming: make(chan *controlv1.ControlFrame, 64), sent: make(chan *controlv1.EdgeFrame, 64)}
	done := make(chan error, 1)
	go func() { done <- s.Connect(stream) }()
	open := openFrame(s, 1)
	open.GetOpen().Capabilities = []string{edgeprotocol.CapInbound}
	stream.incoming <- open
	<-stream.sent
	<-stream.sent
	stream.incoming <- reconcileFrame(s, 1)
	is := &inboundSession{s: s, stream: stream, done: done, ctx: ctx}
	waitFor(t, "attach", func() bool { s.inbound.mu.Lock(); defer s.inbound.mu.Unlock(); return s.inbound.active != nil })
	result := forward(is, inboundMessageID, []byte("x"))
	readInbound(t, is)
	grantState.valid.Store(false)
	is.ack(inboundMessageID, controlv1.InboundVerdict_INBOUND_VERDICT_ACCEPTED)
	if r := <-result; r.err == nil {
		t.Fatal("acknowledged after the lease expired")
	}
}

// preInbound shapes: Open without field 9 and Ready without field 8, as an
// older build declares them. Either side upgrades first without a decode error,
// and a new message round-trips through an old decode keeping the new field.
func oldMessage(t *testing.T, name string, keep int32, source proto.Message) *dynamicpb.Message {
	t.Helper()
	desc := source.ProtoReflect().Descriptor()
	var fields []*descriptorpb.FieldDescriptorProto
	for i := 0; i < desc.Fields().Len(); i++ {
		f := desc.Fields().Get(i)
		if int32(f.Number()) > keep {
			continue
		}
		fd := &descriptorpb.FieldDescriptorProto{Name: proto.String(string(f.Name())), Number: proto.Int32(int32(f.Number()))}
		switch f.Kind().String() {
		case "string":
			fd.Type = descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
		case "uint64":
			fd.Type = descriptorpb.FieldDescriptorProto_TYPE_UINT64.Enum()
		case "uint32":
			fd.Type = descriptorpb.FieldDescriptorProto_TYPE_UINT32.Enum()
		case "bytes":
			fd.Type = descriptorpb.FieldDescriptorProto_TYPE_BYTES.Enum()
		default:
			t.Fatalf("unsupported kind %s", f.Kind())
		}
		fd.Label = descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
		if f.IsList() {
			fd.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		}
		fields = append(fields, fd)
	}
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{Name: proto.String(name + ".proto"), Package: proto.String(name), Syntax: proto.String("proto3"), MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Old"), Field: fields}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return dynamicpb.NewMessage(file.Messages().ByName("Old"))
}

func TestOldShapeOpenAndReadyInterwork(t *testing.T) {
	newOpen := &controlv1.Open{WorkerId: "worker", ControllerId: "c", Epoch: 4, SessionId: inboundMessageID, ProtocolVersion: edgeprotocol.Version, JournalId: "j", Capabilities: []string{edgeprotocol.CapInbound}}
	wire, err := proto.Marshal(newOpen)
	if err != nil {
		t.Fatal(err)
	}
	// An old worker decodes the new Open and keeps what it knows.
	old := oldMessage(t, "preInboundOpen", 8, newOpen)
	if err = proto.Unmarshal(wire, old); err != nil {
		t.Fatalf("old worker cannot decode the new Open: %v", err)
	}
	if old.Get(old.Descriptor().Fields().ByName("epoch")).Uint() != 4 {
		t.Fatal("old shape lost a known field")
	}
	// An old control plane's Open has no capabilities: the new worker offers
	// none to it and refuses inbound frames.
	oldWire, err := proto.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var decoded controlv1.Open
	if err = proto.Unmarshal(oldWire, &decoded); err != nil || len(decoded.Capabilities) != 1 {
		t.Fatalf("unknown field not preserved: %v %v", err, decoded.Capabilities)
	}
	newReady := &controlv1.Ready{WorkerId: "worker", Epoch: 4, SessionId: inboundMessageID, ProtocolVersion: edgeprotocol.Version, Capabilities: []string{edgeprotocol.CapInbound}, BounceDomains: []string{"bounce.example"}}
	wire, err = proto.Marshal(newReady)
	if err != nil {
		t.Fatal(err)
	}
	oldReady := oldMessage(t, "preInboundReady", 7, newReady)
	if err = proto.Unmarshal(wire, oldReady); err != nil {
		t.Fatalf("an old control plane cannot decode the new Ready: %v", err)
	}
	// And an old Ready (no capabilities) decodes in the new shape with none, so
	// a new control plane sends no inbound frame.
	oldOnly := &controlv1.Ready{WorkerId: "worker", Epoch: 4}
	wire, _ = proto.Marshal(oldOnly)
	var parsed controlv1.Ready
	if err = proto.Unmarshal(wire, &parsed); err != nil || edgeprotocol.HasCapability(parsed.Capabilities, edgeprotocol.CapInbound) {
		t.Fatal("a Ready without the field reports inbound")
	}
}
