package edgesession

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/controltls"
	"github.com/zyno-io/thinpost/internal/edgeworker"
	"github.com/zyno-io/thinpost/internal/journal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func permitTestCertificates(t *testing.T) (*x509.CertPool, tls.Certificate, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
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
	issue := func(identity string, usage x509.ExtKeyUsage, serial int64) tls.Certificate {
		key, keyErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		uri, uriErr := url.Parse(identity)
		if uriErr != nil {
			t.Fatal(uriErr)
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		if usage == x509.ExtKeyUsageServerAuth {
			cert.DNSNames = []string{"edge.internal"}
		}
		der, certErr := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
		if certErr != nil {
			t.Fatal(certErr)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	}
	return roots, issue("spiffe://thinpost.test/staging/controller/one", x509.ExtKeyUsageClientAuth, 2), issue("spiffe://thinpost.test/staging/worker/one", x509.ExtKeyUsageServerAuth, 3)
}

// This uses verified mutual TLS over a real loopback TCP gRPC connection and
// the actual worker and journal. The controller is a synthetic gRPC client;
// it does not exercise the Trusted database or a deployed network path.
func TestPermitTimeoutOverMutualTLS(t *testing.T) {
	s, j, _, _ := setup(t, true)
	s.permitTimeout = 150 * time.Millisecond
	dialer := &timeoutTestDialer{}
	s.executor = &edgeworker.Worker{Journal: j, Dialer: dialer, Recipients: timeoutTestPolicy{}, HeloName: "smtp.sender.example"}
	roots, clientCert, serverCert := permitTestCertificates(t)
	serverTLS, err := controltls.Server(roots, serverCert, []string{"spiffe://thinpost.test/staging/controller/one"})
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := controltls.Client(roots, clientCert, "edge.internal", "spiffe://thinpost.test/staging/worker/one")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	controlv1.RegisterEdgeControlServer(server, s)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	connection, err := grpc.NewClient("passthrough:///edge.internal", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}), grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client, err := controlv1.NewEdgeControlClient(connection).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Send(openFrame(s, 1)); err != nil {
		t.Fatal(err)
	}
	ready, err := client.Recv()
	if err != nil || ready.GetReady() == nil {
		t.Fatal("missing readiness", err)
	}
	replay, err := client.Recv()
	if err != nil || replay.GetReplayComplete() == nil {
		t.Fatal("missing replay completion", err)
	}
	if err = client.Send(reconcileFrame(s, 1)); err != nil {
		t.Fatal(err)
	}
	if err = client.Send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_Wake{Wake: &controlv1.Wake{}}}); err != nil {
		t.Fatal(err)
	}
	pull, err := client.Recv()
	if err != nil || pull.GetPull() == nil {
		t.Fatal("missing pull", err)
	}
	mime := []byte("synthetic message")
	digest := sha256.Sum256(mime)
	start := &controlv1.DeliveryStart{AttemptId: "attempt", DeliveryId: "delivery", Epoch: 1, MxName: "mx.example", DestinationIp: "8.8.8.8", EnvelopeFrom: "bounce@example.com", Recipient: "to@example.net", ExpectedSize: uint64(len(mime)), Sha256: digest[:]}
	if err = client.Send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_Delivery{Delivery: start}}); err != nil {
		t.Fatal(err)
	}
	if err = client.Send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_MimeChunk{MimeChunk: &controlv1.DataChunk{Id: "attempt", Data: mime, Last: true}}}); err != nil {
		t.Fatal(err)
	}
	requestFrame, err := client.Recv()
	if err != nil || requestFrame.GetStartRequest() == nil {
		t.Fatal("missing start request", err)
	}
	if _, err = client.Recv(); err == nil {
		t.Fatal("permit timeout did not close the real stream")
	}
	if dialer.calls.Load() != 0 {
		t.Fatal("SMTP was dialed without a permit")
	}
	items, err := j.List()
	if err != nil || len(items) != 1 || items[0].Outcome != journal.Rejected {
		t.Fatal("unsent result not journaled", items, err)
	}
	// A previously committed authorization may arrive after timeout. The old
	// RPC is closed; reconnect must replay before accepting new work.
	_ = client.Send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_StartPermit{StartPermit: &controlv1.StartPermit{Request: requestFrame.GetStartRequest()}}})
	next, err := controlv1.NewEdgeControlClient(connection).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Send(openFrame(s, 2)); err != nil {
		t.Fatal(err)
	}
	ready, err = next.Recv()
	if err != nil || ready.GetReady() == nil {
		t.Fatal("replacement not ready", err)
	}
	replayedFrame, err := next.Recv()
	if err != nil || replayedFrame.GetResult() == nil {
		t.Fatal("missing replay", err)
	}
	replayed := replayedFrame.GetResult()
	if replayed.AttemptId != start.AttemptId || replayed.Outcome != controlv1.DeliveryOutcome_DELIVERY_OUTCOME_REJECTED || replayed.SmtpCode != 0 {
		t.Fatal("wrong replay result")
	}
	if err = next.Send(ackFrame(replayed)); err != nil {
		t.Fatal(err)
	}
	replay, err = next.Recv()
	if err != nil || replay.GetReplayComplete() == nil {
		t.Fatal("replay did not complete", err)
	}
	items, err = j.List()
	if err != nil || len(items) != 0 || dialer.calls.Load() != 0 {
		t.Fatal("attempt not safely settled", items, err)
	}
}
