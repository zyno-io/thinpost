package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/controltls"
)

// This opt-in fixture uses only generated credentials and tests a real Secret
// volume projection. The caller changes the projected phase and pairs while
// these TLS configurations remain alive.
func TestProjectedCertificateRotation(t *testing.T) {
	dir := os.Getenv("THINPOST_TEST_PROJECTED_CERTIFICATES")
	if dir == "" {
		t.Skip("projected certificate fixture not enabled")
	}
	path := func(name string) string { return filepath.Join(dir, name) }
	ca, err := os.ReadFile(path("ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("invalid fixture CA")
	}
	client, err := tls.LoadX509KeyPair(path("client.crt"), path("client.key"))
	if err != nil {
		t.Fatal(err)
	}
	controlServer, err := privateControlTLS(path("edge.crt"), path("edge.key"), []string{path("smtp.crt")}, roots, []string{"spiffe://thinpost.fixture/control"})
	if err != nil {
		t.Fatal(err)
	}
	smtpServer, err := publicSMTPTLS(path("smtp.crt"), path("smtp.key"), "smtp.fixture.invalid", path("edge.crt"), roots)
	if err != nil {
		t.Fatal(err)
	}
	controlClient, err := controltls.Client(roots, client, "edge.fixture.invalid", "spiffe://thinpost.fixture/edge")
	if err != nil {
		t.Fatal(err)
	}
	smtpClient := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "smtp.fixture.invalid"}
	handshake := func(serverConfig, clientConfig *tls.Config) (int64, error) {
		serverConn, clientConn := net.Pipe()
		defer serverConn.Close()
		defer clientConn.Close()
		deadline := time.Now().Add(3 * time.Second)
		_ = serverConn.SetDeadline(deadline)
		_ = clientConn.SetDeadline(deadline)
		server, client := tls.Server(serverConn, serverConfig), tls.Client(clientConn, clientConfig)
		done := make(chan error, 1)
		go func() { done <- server.Handshake() }()
		clientErr := client.Handshake()
		serverErr := <-done
		if clientErr != nil {
			return 0, clientErr
		}
		if serverErr != nil {
			return 0, serverErr
		}
		return client.ConnectionState().PeerCertificates[0].SerialNumber.Int64(), nil
	}
	check := func(wantControl, wantSMTP int64, fail bool) {
		t.Helper()
		for _, item := range []struct {
			name           string
			server, client *tls.Config
			serial         int64
		}{
			{"control", controlServer, controlClient, wantControl}, {"smtp", smtpServer, smtpClient, wantSMTP},
		} {
			serial, err := handshake(item.server, item.client)
			if fail && err == nil {
				t.Fatalf("%s accepted mismatched keypair", item.name)
			}
			if !fail && (err != nil || serial != item.serial) {
				t.Fatalf("%s: serial %d, error %v", item.name, serial, err)
			}
		}
	}
	waitPhase := func(want string) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Minute)
		for time.Now().Before(deadline) {
			value, readErr := os.ReadFile(path("phase"))
			if readErr == nil && strings.TrimSpace(string(value)) == want {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for projected phase %s", want)
	}
	check(2, 3, false)
	fmt.Println("PROJECTION_INITIAL_OK")
	waitPhase("mismatched")
	check(0, 0, true)
	fmt.Println("PROJECTION_MISMATCH_REJECTED")
	waitPhase("rotated")
	check(6, 7, false)
	fmt.Println("PROJECTION_ROTATED_OK")
}
