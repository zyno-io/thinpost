package bounce

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

type fakeForwarder struct {
	received chan string
	release  chan error
}

func (f *fakeForwarder) Forward(ctx context.Context, token, id string, body []byte) error {
	f.received <- fmt.Sprintf("%s:%s:%s", token, id, body)
	select {
	case err := <-f.release:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func setup(t *testing.T, maxBytes int64) (*smtp.Client, *fakeForwarder) {
	return setupDomains(t, maxBytes, "bounce.example")
}
func setupDomains(t *testing.T, maxBytes int64, domains ...string) (*smtp.Client, *fakeForwarder) {
	t.Helper()
	f := &fakeForwarder{received: make(chan string, 1), release: make(chan error, 1)}
	server, err := NewServer(Config{Hostname: "smtp.mx.example", Domains: domains, TLS: &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return nil, errors.New("test certificate unavailable")
	}}, MaxBytes: maxBytes, MaxConcurrent: 2, ForwardTimeout: time.Second, Forwarder: f})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	client, err := smtp.Dial(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, f
}
func begin(t *testing.T, c *smtp.Client) {
	t.Helper()
	if err := c.Mail(""); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("b+0123456789abcdef0123456789abcdef@bounce.example"); err != nil {
		t.Fatal(err)
	}
}
func smtpCode(err error) int {
	var e *textproto.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}
func TestBounceAcknowledgedOnlyAfterDurableForward(t *testing.T) {
	client, forwarder := setup(t, 1024)
	begin(t, client)
	writer, err := client.Data()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte("Subject: bounce\r\n\r\nbody\r\n"))
	done := make(chan error, 1)
	go func() { done <- writer.Close() }()
	select {
	case received := <-forwarder.received:
		if !strings.HasPrefix(received, "0123456789abcdef0123456789abcdef:") || !strings.Contains(received, "Subject: bounce") {
			t.Fatal("incorrect forward")
		}
	case <-time.After(time.Second):
		t.Fatal("no forward")
	}
	select {
	case err = <-done:
		t.Fatalf("SMTP acknowledged before trusted commit: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	forwarder.release <- nil
	if err = <-done; err != nil {
		t.Fatalf("committed bounce rejected: %v", err)
	}
}
func TestBounceFailureAndRecipientRestrictions(t *testing.T) {
	t.Run("forward failure", func(t *testing.T) {
		client, forwarder := setup(t, 1024)
		begin(t, client)
		writer, err := client.Data()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte("body\r\n"))
		done := make(chan error, 1)
		go func() { done <- writer.Close() }()
		<-forwarder.received
		forwarder.release <- errors.New("secret internal detail")
		err = <-done
		if smtpCode(err) != 451 || strings.Contains(err.Error(), "secret internal detail") {
			t.Fatalf("failure %v", err)
		}
	})
	t.Run("no relay", func(t *testing.T) {
		client, _ := setup(t, 1024)
		if err := client.Mail(""); err != nil {
			t.Fatal(err)
		}
		if err := client.Rcpt("person@other.example"); smtpCode(err) != 550 {
			t.Fatalf("recipient %v", err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		client, forwarder := setup(t, 64)
		begin(t, client)
		writer, err := client.Data()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = writer.Write([]byte(strings.Repeat("a", 70) + "\r\n"))
		err = writer.Close()
		if smtpCode(err) != 552 {
			t.Fatalf("size result %v", err)
		}
		select {
		case <-forwarder.received:
			t.Fatal("forwarded oversized body")
		default:
		}
	})
}
