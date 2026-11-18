package smtptransport

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mx.example"}, DNSNames: []string{"mx.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}))
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return cert, roots
}

func TestSetupRejectionsAreRouteScoped(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responses []string
		commands  []string
		wantRoute bool
	}{
		{"greeting", []string{"554 unavailable\r\n"}, nil, true},
		{"hello", []string{"220 ready\r\n", "550 no EHLO\r\n", "550 no HELO\r\n"}, []string{"EHLO ", "HELO "}, true},
		{"starttls", []string{"220 ready\r\n", "250-mx.example\r\n250 STARTTLS\r\n", "554 TLS unavailable\r\n"}, []string{"EHLO ", "STARTTLS"}, true},
		{"recipient", []string{"220 ready\r\n", "250 hello\r\n", "250 sender ok\r\n", "550 no mailbox\r\n"}, []string{"EHLO ", "MAIL FROM:", "RCPT TO:"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				reader := bufio.NewReader(server)
				for i, response := range tc.responses {
					if i > 0 {
						line, err := reader.ReadString('\n')
						if err != nil || !strings.HasPrefix(line, tc.commands[i-1]) {
							done <- fmt.Errorf("unexpected command %q: %v", line, err)
							return
						}
					}
					if _, err := fmt.Fprint(server, response); err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			outcome, err := Send(ctx, client, Request{MXName: "mx.example", HeloName: "smtp.sender.example", EnvelopeFrom: "bounce@sender.example", Recipient: "to@example.net", MIME: strings.NewReader("Subject: test\r\n\r\nbody\r\n")})
			var reply *ReplyError
			if outcome != NotAccepted || !errors.As(err, &reply) || reply.Code < 500 || errors.Is(err, ErrRouteRejected) != tc.wantRoute {
				t.Fatalf("incorrect rejection scope: outcome=%v error=%v", outcome, err)
			}
			if serverErr := <-done; serverErr != nil {
				t.Fatal(serverErr)
			}
		})
	}
}

func runServer(conn net.Conn, offerTLS bool, cert tls.Certificate, closeBeforeResponse bool) error {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	if _, err := fmt.Fprint(conn, "220 mx.example ESMTP\r\n"); err != nil {
		return err
	}
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "EHLO ") {
		return fmt.Errorf("expected EHLO: %q %v", line, err)
	}
	if offerTLS {
		if _, err = fmt.Fprint(conn, "250-mx.example\r\n250 STARTTLS\r\n"); err != nil {
			return err
		}
		line, err = reader.ReadString('\n')
		if err != nil || line != "STARTTLS\r\n" {
			return fmt.Errorf("expected STARTTLS: %q %v", line, err)
		}
		if _, err = fmt.Fprint(conn, "220 begin TLS\r\n"); err != nil {
			return err
		}
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		if err = tlsConn.Handshake(); err != nil {
			return err
		}
		conn = tlsConn
		reader = bufio.NewReader(conn)
		line, err = reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "EHLO ") {
			return fmt.Errorf("expected TLS EHLO: %q %v", line, err)
		}
	}
	if _, err = fmt.Fprint(conn, "250 mx.example\r\n"); err != nil {
		return err
	}
	for _, command := range []string{"MAIL FROM:", "RCPT TO:", "DATA"} {
		line, err = reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, command) {
			return fmt.Errorf("expected %s: %q %v", command, line, err)
		}
		response := "250 ok\r\n"
		if command == "DATA" {
			response = "354 go ahead\r\n"
		}
		if _, err = fmt.Fprint(conn, response); err != nil {
			return err
		}
	}
	data, err := textproto.NewReader(reader).ReadDotBytes()
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), "test body") {
		return fmt.Errorf("message missing")
	}
	if closeBeforeResponse {
		return nil
	}
	if _, err = fmt.Fprint(conn, "250 accepted\r\n"); err != nil {
		return err
	}
	line, err = reader.ReadString('\n')
	if err != nil || line != "QUIT\r\n" {
		return fmt.Errorf("expected QUIT: %q %v", line, err)
	}
	_, err = fmt.Fprint(conn, "221 bye\r\n")
	return err
}

func TestDeliveryTLSAndAmbiguity(t *testing.T) {
	cert, roots := testCert(t)
	for _, tc := range []struct {
		name                                 string
		offer, require, untrusted, loseReply bool
		want                                 Outcome
		wantErr                              bool
	}{
		{"required TLS succeeds", true, true, false, false, Accepted, false},
		{"required TLS unavailable", false, true, false, false, NotAccepted, true},
		{"invalid certificate rejected", true, true, true, false, NotAccepted, true},
		{"opportunistic cleartext", false, false, false, false, Accepted, false},
		{"lost final response", false, false, false, true, Uncertain, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			serverErr := make(chan error, 1)
			go func() { serverErr <- runServer(server, tc.offer, cert, tc.loseReply) }()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			trusted := roots
			if tc.untrusted {
				trusted = x509.NewCertPool()
			}
			outcome, err := Send(ctx, client, Request{MXName: "mx.example", HeloName: "smtp.sender.example", EnvelopeFrom: "bounce@sender.example", Recipient: "to@example.net", RequireTLS: tc.require, Roots: trusted, MIME: strings.NewReader("Subject: test\r\n\r\ntest body\r\n")})
			if outcome != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("outcome %v error %v", outcome, err)
			}
			serverResult := <-serverErr
			if !tc.wantErr && serverResult != nil {
				t.Fatal(serverResult)
			}
		})
	}
}

func TestRequiredTLSRejectsTrustedCertificateForWrongMX(t *testing.T) {
	cert, roots := testCert(t) // trusted for mx.example, not approved.example
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- runServer(server, true, cert, false) }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	outcome, err := Send(ctx, client, Request{MXName: "approved.example", HeloName: "smtp.sender.example", EnvelopeFrom: "bounce@sender.example", Recipient: "to@example.net", RequireTLS: true, Roots: roots, MIME: strings.NewReader("Subject: test\r\n\r\ntest body\r\n")})
	if outcome != NotAccepted || err == nil {
		t.Fatal("trusted certificate for another MX was accepted", outcome, err)
	}
	if err = <-done; err == nil {
		t.Fatal("SMTP server continued past the failed TLS handshake")
	}
}

func TestRejectsInternationalAddressWithoutSMTPUTF8(t *testing.T) {
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- runServer(server, false, tls.Certificate{}, false) }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	outcome, err := Send(ctx, client, Request{MXName: "mx.example", HeloName: "smtp.sender.example", EnvelopeFrom: "bounce@sender.example", Recipient: "josé@example.net", MIME: strings.NewReader("Subject: test\r\n\r\ntest body\r\n")})
	if outcome != NotAccepted || err == nil || !strings.Contains(err.Error(), "SMTPUTF8") {
		t.Fatalf("outcome %v error %v", outcome, err)
	}
	<-done
}

func TestReplyErrorsNeverRetainReplyText(t *testing.T) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		reader := bufio.NewReader(server)
		fmt.Fprint(server, "220 ready\r\n")
		for _, response := range []string{"250 hello\r\n", "250 sender ok\r\n", "550 5.1.1 <victim@example.com> no such user\r\n"} {
			if _, err := reader.ReadString('\n'); err != nil {
				return
			}
			fmt.Fprint(server, response)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	outcome, err := Send(ctx, client, Request{MXName: "mx.example", HeloName: "smtp.sender.example", EnvelopeFrom: "bounce@sender.example", Recipient: "victim@example.com", MIME: strings.NewReader("Subject: test\r\n\r\nbody\r\n")})
	var reply *ReplyError
	if outcome != NotAccepted || !errors.As(err, &reply) || reply.Code != 550 || reply.Stage != "RCPT TO" {
		t.Fatalf("unexpected result: %v %v", outcome, err)
	}
	if err.Error() != "SMTP RCPT TO rejected (550)" {
		t.Fatalf("reply text leaked: %q", err.Error())
	}
}
