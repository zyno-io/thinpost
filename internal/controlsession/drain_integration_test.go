package controlsession

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/controllease"
	"github.com/zyno-io/thinpost/internal/controltls"
	"github.com/zyno-io/thinpost/internal/edgesession"
	"github.com/zyno-io/thinpost/internal/journal"
	"github.com/zyno-io/thinpost/internal/leaseproof"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
)

type drainingExecutor struct {
	journal *journal.Journal
	started chan struct{}
	finish  chan struct{}
}

func (e *drainingExecutor) Execute(ctx context.Context, fence journal.Fence, start *controlv1.DeliveryStart, _ []byte, _ func() bool, permit func(context.Context, *controlv1.DeliveryStart) error) (*controlv1.DeliveryResult, error) {
	if err := e.journal.Begin(start.AttemptId, start.DeliveryId, fence, time.Now()); err != nil {
		return nil, err
	}
	if err := permit(ctx, start); err != nil {
		return nil, err
	}
	close(e.started)
	select {
	case <-e.finish:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := e.journal.Complete(start.AttemptId, journal.Accepted, 250, time.Now()); err != nil {
		return nil, err
	}
	return &controlv1.DeliveryResult{AttemptId: start.AttemptId, DeliveryId: start.DeliveryId, Epoch: start.Epoch, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED, SmtpCode: 250}, nil
}

func TestDrainPreservesActiveWorkAcrossMutualTLSStream(t *testing.T) {
	roots, clientCert, serverCert := certificates(t)
	controllerID := "spiffe://thinpost.test/staging/controller/service"
	workerID := "spiffe://thinpost.test/staging/worker/one"
	serverTLS, err := controltls.Server(roots, serverCert, []string{controllerID})
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := controltls.Client(roots, clientCert, "edge.internal", workerID)
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	journalID, err := j.Identity()
	if err != nil {
		t.Fatal(err)
	}
	authority, err := leaseproof.NewAuthority(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	executor := &drainingExecutor{journal: j, started: make(chan struct{}), finish: make(chan struct{})}
	edge, err := edgesession.New("worker", []string{controllerID}, j, executor, authority, 2)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	controlv1.RegisterEdgeControlServer(server, edge)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///edge.internal", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }), grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	signer, ok := clientCert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("unexpected client key")
	}
	results := &fakeResults{}
	issuer := &controllease.Issuer{Repository: fakeLeaseRepo{}, Signer: signer, Identity: controllerID, JournalID: journalID}
	runner := &Runner{WorkerID: "worker", ControllerID: "instance", JournalID: journalID, Issuer: issuer, Results: results, Starts: results, Reconciler: results, Work: &fakeWork{}, LeaseTTL: 30 * time.Second, ClockMargin: 6 * time.Second, PollInterval: time.Second, Connect: func(ctx context.Context) (Stream, error) { return controlv1.NewEdgeControlClient(conn).Connect(ctx) }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drain := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- runner.RunDraining(ctx, drain) }()
	select {
	case <-executor.started:
	case <-ctx.Done():
		t.Fatal("delivery did not reach the permitted execution stage")
	}
	close(drain)
	select {
	case err := <-done:
		t.Fatal("session ended before active delivery finished", err)
	default:
	}
	close(executor.finish)
	select {
	case err := <-done:
		if !errors.Is(err, ErrDraining) || !results.wasCommitted() {
			t.Fatal("active result was not committed before drain", err)
		}
	case <-ctx.Done():
		t.Fatal("session did not finish draining")
	}
}
