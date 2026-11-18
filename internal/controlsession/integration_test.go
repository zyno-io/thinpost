package controlsession

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/smtp"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/bounce"
	"github.com/zyno-io/thinpost/internal/controllease"
	"github.com/zyno-io/thinpost/internal/controltls"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
	"github.com/zyno-io/thinpost/internal/edgesession"
	"github.com/zyno-io/thinpost/internal/journal"
	"github.com/zyno-io/thinpost/internal/leaseproof"
	"github.com/zyno-io/thinpost/internal/postgres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
)

type fakeLeaseRepo struct{}

type integrationBounceStore struct {
	started chan struct{}
	release chan struct{}
}

func (s *integrationBounceStore) Forward(ctx context.Context, token, id string, body []byte) error {
	if token != "0123456789abcdef0123456789abcdef" || len(id) != 32 || string(body) != "Subject: failure\r\n\r\nbody\r\n" {
		return errors.New("invalid bounce")
	}
	close(s.started)
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (fakeLeaseRepo) AcquireControllerLease(_ context.Context, worker, controller string, ttl time.Duration) (postgres.ControllerLease, error) {
	return postgres.ControllerLease{WorkerID: worker, ControllerID: controller, Epoch: 1, ExpiresAt: time.Now().Add(ttl)}, nil
}
func (fakeLeaseRepo) RenewControllerLease(_ context.Context, lease postgres.ControllerLease, ttl time.Duration) (postgres.ControllerLease, error) {
	lease.ExpiresAt = time.Now().Add(ttl)
	return lease, nil
}
func (fakeLeaseRepo) ReleaseControllerLease(context.Context, postgres.ControllerLease) (bool, error) {
	return true, nil
}

type journalExecutor struct{ journal *journal.Journal }

func (e journalExecutor) Execute(ctx context.Context, fence journal.Fence, start *controlv1.DeliveryStart, _ []byte, _ func() bool, permit func(context.Context, *controlv1.DeliveryStart) error) (*controlv1.DeliveryResult, error) {
	if err := e.journal.Begin(start.AttemptId, start.DeliveryId, fence, time.Now()); err != nil {
		return nil, err
	}
	if err := permit(ctx, start); err != nil {
		return nil, err
	}
	if err := e.journal.Complete(start.AttemptId, journal.Accepted, 250, time.Now()); err != nil {
		return nil, err
	}
	return &controlv1.DeliveryResult{AttemptId: start.AttemptId, DeliveryId: start.DeliveryId, Epoch: start.Epoch, Outcome: controlv1.DeliveryOutcome_DELIVERY_OUTCOME_ACCEPTED, SmtpCode: 250}, nil
}
func certificates(t *testing.T) (*x509.CertPool, tls.Certificate, tls.Certificate) {
	return certificatesWithLifetimes(t, time.Hour, time.Hour)
}

func certificatesWithLifetimes(t *testing.T, clientLifetime, serverLifetime time.Duration) (*x509.CertPool, tls.Certificate, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	issue := func(identity, dns string, usage x509.ExtKeyUsage) tls.Certificate {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, err := url.Parse(identity)
		if err != nil {
			t.Fatal(err)
		}
		expires := time.Now().Add(serverLifetime)
		if usage == x509.ExtKeyUsageClientAuth {
			expires = time.Now().Add(clientLifetime)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(2), URIs: []*url.URL{uri}, NotBefore: time.Now().Add(-time.Hour), NotAfter: expires, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		if dns != "" {
			template.DNSNames = []string{dns}
		}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		priv, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: priv}))
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
	return roots, issue("spiffe://thinpost.test/staging/controller/service", "", x509.ExtKeyUsageClientAuth), issue("spiffe://thinpost.test/staging/worker/one", "edge.internal", x509.ExtKeyUsageServerAuth)
}

func TestMTLSStreamEndsBeforeControllerCertificateExpires(t *testing.T) {
	testMTLSStreamCertificateExpiry(t, false)
}

func TestMTLSStreamEndsBeforeWorkerCertificateExpires(t *testing.T) {
	testMTLSStreamCertificateExpiry(t, true)
}

func TestMutualTLSRejectsDifferentJournalBeforeFencing(t *testing.T) {
	roots, clientCert, serverCert := certificates(t)
	controllerIdentity := "spiffe://thinpost.test/staging/controller/service"
	workerIdentity := "spiffe://thinpost.test/staging/worker/one"
	serverTLS, err := controltls.Server(roots, serverCert, []string{controllerIdentity})
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := controltls.Client(roots, clientCert, "edge.internal", workerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	actualID, err := j.Identity()
	if err != nil {
		t.Fatal(err)
	}
	wrongID := "00000000000000000000000000000000"
	if wrongID == actualID {
		wrongID = "ffffffffffffffffffffffffffffffff"
	}
	authority, err := leaseproof.NewAuthority(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := edgesession.New("worker", []string{controllerIdentity}, j, journalExecutor{j}, authority, 2)
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
		t.Fatal("unexpected key")
	}
	lease := leaseproof.Lease{WorkerID: "worker", ControllerID: "replica", JournalID: wrongID, Identity: controllerIdentity, Epoch: 1, ExpiresUnixMilli: time.Now().Add(time.Minute).UnixMilli()}
	payload, signature, err := leaseproof.Sign(lease, signer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := controlv1.NewEdgeControlClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	open := &controlv1.Open{WorkerId: "worker", ControllerId: "replica", JournalId: wrongID, Epoch: 1, SessionId: "0123456789abcdef0123456789abcdef", ProtocolVersion: edgeprotocol.Version, LeasePayload: payload, LeaseSignature: signature}
	if err = stream.Send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_Open{Open: open}}); err != nil {
		t.Fatal(err)
	}
	if frame, err := stream.Recv(); err == nil || frame != nil {
		t.Fatal("different journal was accepted", err)
	}
	if current, err := j.IsCurrent(journal.Fence{ControllerID: "replica", Epoch: 1}); err != nil || current {
		t.Fatal("journal mismatch changed the fence", err)
	}
}

func testMTLSStreamCertificateExpiry(t *testing.T, workerExpires bool) {
	t.Helper()
	clientLifetime, serverLifetime := 7*time.Second, time.Hour
	if workerExpires {
		clientLifetime, serverLifetime = serverLifetime, clientLifetime
	}
	roots, clientCert, serverCert := certificatesWithLifetimes(t, clientLifetime, serverLifetime)
	controllerIdentity := "spiffe://thinpost.test/staging/controller/service"
	workerIdentity := "spiffe://thinpost.test/staging/worker/one"
	expiringCert := clientCert
	if workerExpires {
		expiringCert = serverCert
	}
	leaf, err := x509.ParseCertificate(expiringCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := controltls.Server(roots, serverCert, []string{controllerIdentity})
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := controltls.Client(roots, clientCert, "edge.internal", workerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	var transport credentials.TransportCredentials = credentials.NewTLS(clientTLS)
	if workerExpires {
		transport, err = controltls.ExpiringClientCredentials(clientTLS, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
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
	authority, err := leaseproof.NewAuthority(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := edgesession.New("worker", []string{controllerIdentity}, j, journalExecutor{j}, authority, 2)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	controlv1.RegisterEdgeControlServer(server, edge)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///edge.internal", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }), grpc.WithTransportCredentials(transport))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, err := controlv1.NewEdgeControlClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	signer, ok := clientCert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("unexpected key")
	}
	lease := leaseproof.Lease{WorkerID: "worker", ControllerID: "replica", JournalID: journalID, Identity: controllerIdentity, Epoch: 1, ExpiresUnixMilli: time.Now().Add(time.Minute).UnixMilli()}
	payload, signature, err := leaseproof.Sign(lease, signer)
	if err != nil {
		t.Fatal(err)
	}
	open := &controlv1.Open{WorkerId: "worker", ControllerId: "replica", JournalId: journalID, Epoch: 1, SessionId: "0123456789abcdef0123456789abcdef", ProtocolVersion: edgeprotocol.Version, LeasePayload: payload, LeaseSignature: signature}
	if err = stream.Send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_Open{Open: open}}); err != nil {
		t.Fatal(err)
	}
	ready, err := stream.Recv()
	if err != nil || ready.GetReady() == nil {
		t.Fatal("stream did not become ready", err)
	}
	replayed, err := stream.Recv()
	if err != nil || replayed.GetReplayComplete() == nil {
		t.Fatal("stream did not finish replay", err)
	}
	if _, err = stream.Recv(); err == nil || ctx.Err() != nil {
		t.Fatal("expired peer certificate did not terminate stream", err)
	}
	if !time.Now().Before(leaf.NotAfter) {
		t.Fatal("stream survived until certificate expiry")
	}
}
func TestMutualTLSStreamPersistsThenAcknowledges(t *testing.T) {
	roots, clientCert, serverCert := certificates(t)
	controllerIdentity := "spiffe://thinpost.test/staging/controller/service"
	workerIdentity := "spiffe://thinpost.test/staging/worker/one"
	serverTLS, err := controltls.Server(roots, serverCert, []string{controllerIdentity})
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := controltls.Client(roots, clientCert, "edge.internal", workerIdentity)
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
	edge, err := edgesession.New("worker", []string{controllerIdentity}, j, journalExecutor{j}, authority, 2)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)), grpc.MaxRecvMsgSize(128<<10), grpc.MaxSendMsgSize(128<<10))
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
		t.Fatal("unexpected key")
	}
	issuer := &controllease.Issuer{Repository: fakeLeaseRepo{}, Signer: signer, Identity: controllerIdentity, JournalID: journalID}
	results := &fakeResults{}
	store := &integrationBounceStore{started: make(chan struct{}), release: make(chan struct{})}
	runner := &Runner{WorkerID: "worker", ControllerID: "instance", JournalID: journalID, Issuer: issuer, Results: results, Starts: results, Reconciler: results, Work: &fakeWork{}, Bounces: store, LeaseTTL: 30 * time.Second, ClockMargin: 6 * time.Second, PollInterval: time.Second, Connect: func(ctx context.Context) (Stream, error) { return controlv1.NewEdgeControlClient(conn).Connect(ctx) }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	deadline := time.After(4 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("delivery result was not committed and acknowledged")
		case <-ticker.C:
			items, err := j.List()
			if err != nil {
				t.Fatal(err)
			}
			if results.wasCommitted() && len(items) == 0 {
				server, err := bounce.NewServer(bounce.Config{Hostname: "smtp.example.com", Domains: []string{"bounce.example.com"}, TLS: &tls.Config{Certificates: []tls.Certificate{serverCert}}, MaxBytes: 1024, MaxConcurrent: 1, ForwardTimeout: 3 * time.Second, Forwarder: edge})
				if err != nil {
					t.Fatal(err)
				}
				smtpListener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				go func() { _ = server.Serve(smtpListener) }()
				defer server.Close()
				smtpResult := make(chan error, 1)
				go func() {
					client, err := smtp.Dial(smtpListener.Addr().String())
					if err != nil {
						smtpResult <- err
						return
					}
					defer client.Close()
					if err = client.Mail(""); err != nil {
						smtpResult <- err
						return
					}
					if err = client.Rcpt("b+0123456789abcdef0123456789abcdef@bounce.example.com"); err != nil {
						smtpResult <- err
						return
					}
					writer, err := client.Data()
					if err != nil {
						smtpResult <- err
						return
					}
					if _, err = writer.Write([]byte("Subject: failure\r\n\r\nbody\r\n")); err != nil {
						smtpResult <- err
						return
					}
					smtpResult <- writer.Close()
				}()
				select {
				case <-store.started:
				case <-time.After(time.Second):
					t.Fatal("bounce did not reach trusted")
				}
				select {
				case err := <-smtpResult:
					t.Fatalf("SMTP replied before durable commit: %v", err)
				default:
				}
				close(store.release)
				select {
				case err := <-smtpResult:
					if err != nil {
						t.Fatalf("committed bounce rejected: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("SMTP acknowledgement missing")
				}
				cancel()
				err = <-done
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("session exit: %v", err)
				}
				return
			}
		}
	}
}
