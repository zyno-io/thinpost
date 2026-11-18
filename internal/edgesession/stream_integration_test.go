package edgesession

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// This uses a real gRPC stream but no network listener or SMTP worker. The
// client stops receiving to exhaust the HTTP/2 flow-control window.
type countedTransport struct {
	grpc.BidiStreamingServer[controlv1.ControlFrame, controlv1.EdgeFrame]
	started, finished atomic.Int64
}

func (s *countedTransport) Send(frame *controlv1.EdgeFrame) error {
	s.started.Add(1)
	err := s.BidiStreamingServer.Send(frame)
	s.finished.Add(1)
	return err
}

type stalledStreamService struct {
	controlv1.UnimplementedEdgeControlServer
	transport chan *countedTransport
	cancel    chan context.CancelFunc
	done      chan struct{}
}

func (s *stalledStreamService) Connect(raw grpc.BidiStreamingServer[controlv1.ControlFrame, controlv1.EdgeFrame]) error {
	defer close(s.done)
	ctx, cancel := context.WithCancel(raw.Context())
	defer cancel()
	transport := &countedTransport{BidiStreamingServer: raw}
	stream := &lockedStream{BidiStreamingServer: transport, ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1)}
	s.transport <- transport
	s.cancel <- cancel
	frame := &controlv1.EdgeFrame{Body: &controlv1.EdgeFrame_BounceChunk{BounceChunk: &controlv1.DataChunk{Data: make([]byte, 64<<10)}}}
	for {
		if err := stream.Send(frame); err != nil {
			return err
		}
	}
}

func TestBlockedGRPCWriterExitsAfterHandlerReturn(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	service := &stalledStreamService{transport: make(chan *countedTransport, 1), cancel: make(chan context.CancelFunc, 1), done: make(chan struct{})}
	controlv1.RegisterEdgeControlServer(server, service)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///edge", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return listener.DialContext(ctx)
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := controlv1.NewEdgeControlClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Send(&controlv1.ControlFrame{}); err != nil {
		t.Fatal(err)
	}
	transport := <-service.transport
	cancelOwner := <-service.cancel
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var previous int64 = -1
	stable := 0
	for stable < 5 {
		select {
		case <-ctx.Done():
			t.Fatal("gRPC write did not reach flow control")
		case <-ticker.C:
			started, finished := transport.started.Load(), transport.finished.Load()
			if started > finished && finished == previous {
				stable++
			} else {
				stable = 0
			}
			previous = finished
		}
	}
	cancelOwner()
	select {
	case <-service.done:
	case <-ctx.Done():
		t.Fatal("handler did not return after cancellation")
	}
	for transport.started.Load() != transport.finished.Load() {
		select {
		case <-ctx.Done():
			t.Fatal("gRPC transport writer did not exit after handler teardown")
		case <-ticker.C:
		}
	}
}
