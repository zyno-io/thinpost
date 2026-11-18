package leaseproof

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"path/filepath"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/edgesession"
	"github.com/zyno-io/thinpost/internal/journal"
	"google.golang.org/grpc/metadata"
)

type sessionStream struct {
	ctx      context.Context
	incoming []*controlv1.ControlFrame
	sent     []*controlv1.EdgeFrame
}

func (s *sessionStream) Context() context.Context { return s.ctx }
func (s *sessionStream) Recv() (*controlv1.ControlFrame, error) {
	if len(s.incoming) == 0 {
		return nil, io.EOF
	}
	frame := s.incoming[0]
	s.incoming = s.incoming[1:]
	return frame, nil
}
func (s *sessionStream) Send(frame *controlv1.EdgeFrame) error {
	s.sent = append(s.sent, frame)
	return nil
}
func (*sessionStream) SetHeader(metadata.MD) error  { return nil }
func (*sessionStream) SendHeader(metadata.MD) error { return nil }
func (*sessionStream) SetTrailer(metadata.MD)       {}
func (*sessionStream) SendMsg(any) error            { return nil }
func (*sessionStream) RecvMsg(any) error            { return nil }

type unusedExecutor struct{}

func (unusedExecutor) Execute(context.Context, journal.Fence, *controlv1.DeliveryStart, []byte, func() bool, func(context.Context, *controlv1.DeliveryStart) error) (*controlv1.DeliveryResult, error) {
	panic("unexpected delivery")
}
func TestSessionAcceptsOnlySignedLeaseAndRenewal(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := "spiffe://thinpost.test/staging/controller/service"
	j, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	journalID, err := j.Identity()
	if err != nil {
		t.Fatal(err)
	}
	lease := Lease{WorkerID: "worker", ControllerID: "replica-instance", JournalID: journalID, Identity: identity, Epoch: 4, ExpiresUnixMilli: time.Now().Add(time.Minute).UnixMilli()}
	payload, sig, err := Sign(lease, key)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := NewAuthority(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	server, err := edgesession.New("worker", []string{identity}, j, unusedExecutor{}, authority, 2)
	if err != nil {
		t.Fatal(err)
	}
	open := &controlv1.Open{WorkerId: lease.WorkerID, ControllerId: lease.ControllerID, JournalId: journalID, Epoch: lease.Epoch, LeasePayload: payload, LeaseSignature: sig, SessionId: "0123456789abcdef0123456789abcdef", ProtocolVersion: edgeprotocol.Version}
	renewal := &controlv1.LeaseRenewal{LeasePayload: payload, LeaseSignature: sig}
	stream := &sessionStream{ctx: peerContext(t, identity, key.Public()), incoming: []*controlv1.ControlFrame{{Body: &controlv1.ControlFrame_Open{Open: open}}, {Body: &controlv1.ControlFrame_LeaseRenewal{LeaseRenewal: renewal}}}}
	if err = server.Connect(stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.sent) != 3 || stream.sent[0].GetReady() == nil || stream.sent[1].GetReplayComplete() == nil || stream.sent[2].GetLeaseAccepted() == nil {
		t.Fatal("signed lease or renewal was not acknowledged")
	}
	badSig := append([]byte(nil), sig...)
	badSig[0] ^= 1
	bad := &sessionStream{ctx: peerContext(t, identity, key.Public()), incoming: []*controlv1.ControlFrame{{Body: &controlv1.ControlFrame_Open{Open: open}}, {Body: &controlv1.ControlFrame_LeaseRenewal{LeaseRenewal: &controlv1.LeaseRenewal{LeasePayload: payload, LeaseSignature: badSig}}}}}
	if err = server.Connect(bad); err == nil {
		t.Fatal("forged renewal accepted")
	}
}
