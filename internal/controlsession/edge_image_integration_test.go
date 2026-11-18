package controlsession

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/smtp"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/controltls"
	"github.com/zyno-io/thinpost/internal/leaseproof"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const imageFixtureController = "spiffe://thinpost.fixture/control"
const imageFixtureWorker = "spiffe://thinpost.fixture/edge"

// Writes only freshly generated synthetic credentials to a caller-provided
// disposable directory. It must never be used with real deployment secrets.
func TestEdgeImageGenerateFixtureCertificates(t *testing.T) {
	dir := os.Getenv("THINPOST_TEST_EDGE_IMAGE_GENERATE")
	if dir == "" {
		t.Skip("edge image fixture generation not enabled")
	}
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(2 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	issue := func(name, hostname, identity string, usage x509.ExtKeyUsage, serial int64) {
		t.Helper()
		key, keyErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		if hostname != "" {
			cert.DNSNames = []string{hostname}
		}
		if identity != "" {
			uri, uriErr := url.Parse(identity)
			if uriErr != nil {
				t.Fatal(uriErr)
			}
			cert.URIs = []*url.URL{uri}
		}
		der, certErr := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
		if certErr != nil {
			t.Fatal(certErr)
		}
		keyDER, keyErr := x509.MarshalPKCS8PrivateKey(key)
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		write(name+".crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		write(name+".key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	}
	issue("edge", "edge.fixture.invalid", imageFixtureWorker, x509.ExtKeyUsageServerAuth, 2)
	issue("smtp", "smtp.fixture.invalid", "", x509.ExtKeyUsageServerAuth, 3)
	issue("client", "", imageFixtureController, x509.ExtKeyUsageClientAuth, 4)
	issue("wrong-client", "", "spiffe://thinpost.fixture/unapproved", x509.ExtKeyUsageClientAuth, 5)
	issue("edge-next", "edge.fixture.invalid", imageFixtureWorker, x509.ExtKeyUsageServerAuth, 6)
	issue("smtp-next", "smtp.fixture.invalid", "", x509.ExtKeyUsageServerAuth, 7)
	write("recipients.txt", []byte("canary@example.net\n"))
}

// Talks only to a synthetic image instance in the caller's isolated network.
// This exercises the process and stream, not a real database or deployment.
func TestEdgeImageControlAndBounce(t *testing.T) {
	dir := os.Getenv("THINPOST_TEST_EDGE_IMAGE_FIXTURE")
	journalID := os.Getenv("THINPOST_TEST_EDGE_IMAGE_JOURNAL")
	if dir == "" || journalID == "" {
		t.Skip("edge image fixture not enabled")
	}
	version, err := strconv.ParseUint(os.Getenv("THINPOST_TEST_EDGE_IMAGE_PROTOCOL"), 10, 32)
	if err != nil || version < 6 || version > 7 {
		t.Fatal("unsupported fixture protocol version")
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("invalid fixture CA")
	}
	clientCert, err := tls.LoadX509KeyPair(filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key"))
	if err != nil {
		t.Fatal(err)
	}
	wrongCert, err := tls.LoadX509KeyPair(filepath.Join(dir, "wrong-client.crt"), filepath.Join(dir, "wrong-client.key"))
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := controltls.Client(roots, clientCert, "edge.fixture.invalid", imageFixtureWorker)
	if err != nil {
		t.Fatal(err)
	}
	for _, certs := range [][]tls.Certificate{nil, {wrongCert}} {
		badTLS := &tls.Config{RootCAs: roots, ServerName: "edge.fixture.invalid", Certificates: certs, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}}
		requireRejectedControlTLS(t, badTLS)
	}
	if err := imageFixtureBounce(roots); err == nil {
		t.Fatal("bounce succeeded without a control session")
	} else {
		var smtpErr *textproto.Error
		if !errors.As(err, &smtpErr) || smtpErr.Code != 451 {
			t.Fatal("disconnected bounce did not return SMTP 451", err)
		}
	}
	connection, err := grpc.NewClient("passthrough:///127.0.0.1:7443", grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	stream, err := controlv1.NewEdgeControlClient(connection).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	signer, ok := clientCert.PrivateKey.(crypto.Signer)
	if !ok {
		t.Fatal("fixture client key cannot sign")
	}
	const session = "0123456789abcdef0123456789abcdef"
	lease := leaseproof.Lease{WorkerID: "fixture-worker", ControllerID: "fixture-controller", JournalID: journalID, Identity: imageFixtureController, Epoch: 1, ExpiresUnixMilli: time.Now().Add(time.Minute).UnixMilli()}
	payload, signature, err := leaseproof.Sign(lease, signer)
	if err != nil {
		t.Fatal(err)
	}
	open := &controlv1.Open{WorkerId: lease.WorkerID, ControllerId: lease.ControllerID, JournalId: journalID, Epoch: 1, SessionId: session, ProtocolVersion: uint32(version), LeasePayload: payload, LeaseSignature: signature}
	if err = stream.Send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_Open{Open: open}}); err != nil {
		t.Fatal(err)
	}
	ready, err := stream.Recv()
	if err != nil || ready.GetReady() == nil || ready.GetReady().JournalId != journalID || ready.GetReady().ProtocolVersion != uint32(version) {
		t.Fatal("valid session was not ready", err)
	}
	replay, err := stream.Recv()
	if err != nil || replay.GetReplayComplete() == nil {
		t.Fatal("missing replay completion", err)
	}
	if err = stream.Send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_ReconciliationComplete{ReconciliationComplete: &controlv1.ReconciliationComplete{WorkerId: lease.WorkerID, JournalId: journalID, SessionId: session, Epoch: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_Wake{Wake: &controlv1.Wake{}}}); err != nil {
		t.Fatal(err)
	}
	pull, err := stream.Recv()
	if err != nil || pull.GetPull() == nil {
		t.Fatal("missing pull after reconciliation", err)
	}
	for _, accepted := range []bool{true, false} {
		result := make(chan error, 1)
		go func() { result <- imageFixtureBounce(roots) }()
		start, recvErr := stream.Recv()
		if recvErr != nil || start.GetBounce() == nil {
			t.Fatal("missing bounce start", recvErr)
		}
		chunk, recvErr := stream.Recv()
		if recvErr != nil || chunk.GetBounceChunk() == nil || !chunk.GetBounceChunk().Last {
			t.Fatal("missing bounce body", recvErr)
		}
		bounce, data := start.GetBounce(), chunk.GetBounceChunk()
		digest := sha256.Sum256(data.Data)
		if bounce.BounceId != data.Id || bounce.RecipientToken != "0123456789abcdef0123456789abcdef" || data.Offset != 0 || bounce.ExpectedSize != uint64(len(data.Data)) || !bytes.Equal(bounce.Sha256, digest[:]) || !bytes.Equal(data.Data, []byte("Subject: synthetic\r\n\r\nfixture\r\n")) {
			t.Fatal("bounce content was not bound to its digest")
		}
		select {
		case <-result:
			t.Fatal("SMTP completed before the control acknowledgement")
		case <-time.After(100 * time.Millisecond):
		}
		if err = stream.Send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_BounceAck{BounceAck: &controlv1.BounceAcknowledgement{BounceId: bounce.BounceId, DurablyAccepted: accepted}}}); err != nil {
			t.Fatal(err)
		}
		bounceErr := <-result
		if accepted && bounceErr != nil {
			t.Fatal("acknowledged bounce failed", bounceErr)
		}
		if !accepted {
			var smtpErr *textproto.Error
			if !errors.As(bounceErr, &smtpErr) || smtpErr.Code != 451 {
				t.Fatal("failed acknowledgement did not return SMTP 451", bounceErr)
			}
		}
	}
	// Exercise new handshakes during a partial key rotation and after the
	// matching key arrives. The already-established control stream is retained.
	copyFixture := func(from, to string) {
		t.Helper()
		data, readErr := os.ReadFile(filepath.Join(dir, from))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if writeErr := os.WriteFile(filepath.Join(dir, to), data, 0644); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	controlSerial := func() (int64, error) {
		config := clientTLS.Clone()
		config.NextProtos = []string{"h2"}
		conn, dialErr := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", "127.0.0.1:7443", config)
		if dialErr != nil {
			return 0, dialErr
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64(), nil
	}
	smtpSerial := func() (int64, error) {
		client, dialErr := smtp.Dial("11.42.51.2:25")
		if dialErr != nil {
			return 0, dialErr
		}
		defer client.Close()
		if tlsErr := client.StartTLS(&tls.Config{RootCAs: roots, ServerName: "smtp.fixture.invalid", MinVersion: tls.VersionTLS12}); tlsErr != nil {
			return 0, tlsErr
		}
		state, ok := client.TLSConnectionState()
		if !ok || len(state.PeerCertificates) == 0 {
			return 0, errors.New("SMTP did not negotiate TLS")
		}
		return state.PeerCertificates[0].SerialNumber.Int64(), nil
	}
	if serial, serialErr := controlSerial(); serialErr != nil || serial != 2 {
		t.Fatal("unexpected original control certificate", serial, serialErr)
	}
	copyFixture("edge-next.crt", "edge.crt")
	if _, serialErr := controlSerial(); serialErr == nil {
		t.Fatal("control accepted a partially updated keypair")
	}
	copyFixture("edge-next.key", "edge.key")
	if serial, serialErr := controlSerial(); serialErr != nil || serial != 6 {
		t.Fatal("control did not recover with the rotated certificate", serial, serialErr)
	}
	if serial, serialErr := smtpSerial(); serialErr != nil || serial != 3 {
		t.Fatal("unexpected original SMTP certificate", serial, serialErr)
	}
	copyFixture("smtp-next.crt", "smtp.crt")
	if _, serialErr := smtpSerial(); serialErr == nil {
		t.Fatal("SMTP accepted a partially updated keypair")
	}
	copyFixture("smtp-next.key", "smtp.key")
	if serial, serialErr := smtpSerial(); serialErr != nil || serial != 7 {
		t.Fatal("SMTP did not recover with the rotated certificate", serial, serialErr)
	}
	result := make(chan error, 1)
	go func() { result <- imageFixtureBounce(roots) }()
	start, recvErr := stream.Recv()
	if recvErr != nil || start.GetBounce() == nil {
		t.Fatal("existing control stream did not receive a bounce after rotation", recvErr)
	}
	chunk, recvErr := stream.Recv()
	if recvErr != nil || chunk.GetBounceChunk() == nil || !chunk.GetBounceChunk().Last {
		t.Fatal("existing control stream did not receive bounce content after rotation", recvErr)
	}
	if err = stream.Send(&controlv1.ControlFrame{Body: &controlv1.ControlFrame_BounceAck{BounceAck: &controlv1.BounceAcknowledgement{BounceId: start.GetBounce().BounceId}}}); err != nil {
		t.Fatal("existing control stream did not send a bounce acknowledgement after rotation", err)
	}
	var smtpErr *textproto.Error
	if bounceErr := <-result; !errors.As(bounceErr, &smtpErr) || smtpErr.Code != 451 {
		t.Fatal("existing control stream did not finish the bounce after rotation", bounceErr)
	}
}

// TLS 1.3 can report handshake success to the client before the server's
// client-certificate rejection arrives. Send the HTTP/2 preface and read so a
// delayed TLS alert cannot be mistaken for a successful authentication.
func requireRejectedControlTLS(t *testing.T, config *tls.Config) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", "127.0.0.1:7443", 3*time.Second)
	if err != nil {
		t.Fatal("control listener unavailable", err)
	}
	defer connection.Close()
	if err = connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	secured := tls.Client(connection, config)
	err = secured.Handshake()
	if err == nil {
		_, err = secured.Write(append([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"), []byte{0, 0, 0, 4, 0, 0, 0, 0, 0}...))
	}
	if err == nil {
		var response [9]byte
		_, err = secured.Read(response[:])
	}
	if err == nil {
		t.Fatal("unauthorized controller completed the TLS handshake")
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatal("control TLS did not reject the unauthorized client", err)
	}
}

func imageFixtureBounce(roots *x509.CertPool) error {
	client, err := smtp.Dial("11.42.51.2:25")
	if err != nil {
		return err
	}
	defer client.Close()
	if err = client.StartTLS(&tls.Config{RootCAs: roots, ServerName: "smtp.fixture.invalid", MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
	if err = client.Mail(""); err != nil {
		return err
	}
	if err = client.Rcpt("b+0123456789abcdef0123456789abcdef@bounce.fixture.invalid"); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = writer.Write([]byte("Subject: synthetic\r\n\r\nfixture\r\n")); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}
