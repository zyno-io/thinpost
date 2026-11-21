package bounce

import (
	"crypto/tls"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"
)

// The Edge passes its listener hostnames as the bounce domains: a bounce
// address is accepted at every one of them, on any listener, and at any other
// domain too (the token, not the domain, identifies the message).
func TestBouncesAreAcceptedAtEveryListenerHostnameAndElsewhere(t *testing.T) {
	hostnames := []string{"mx.example.net", "smtp.hosting.example", "smtp.second.example"}
	var listeners []Listener
	for _, name := range hostnames {
		listeners = append(listeners, Listener{Hostname: name, TLS: &tls.Config{Certificates: []tls.Certificate{selfSigned(t, name)}}})
	}
	f := &fakeForwarder{received: make(chan string, 1), release: make(chan error, 1)}
	servers, err := NewServers(Config{Hostname: hostnames[0], Domains: hostnames, MaxBytes: 1 << 20, MaxConcurrent: 1, ForwardTimeout: time.Second, Forwarder: f}, listeners)
	if err != nil {
		t.Fatal(err)
	}
	recipientID := strings.Repeat("0123456789abcdef", 2)
	for i, server := range servers {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(func() { _ = server.Close() })
		client, err := smtp.Dial(listener.Addr().String())
		if err != nil {
			t.Fatalf("%s: %v", hostnames[i], err)
		}
		t.Cleanup(func() { _ = client.Close() })
		for _, domain := range hostnames {
			if err = client.Mail(""); err != nil {
				t.Fatal(err)
			}
			if err = client.Rcpt("b+" + recipientID + "@" + domain); err != nil {
				t.Errorf("listener %s refused a bounce at %s: %v", hostnames[i], domain, err)
			}
			if err = client.Reset(); err != nil {
				t.Fatal(err)
			}
		}
		// Names that are not a listener hostname are accepted too (a valid token
		// is accepted at any domain, so a customer's Return-Path domain works);
		// control still validates the token before anything is acknowledged.
		for _, domain := range []string{"bounce.example.net", "example.net", "other.example", "mx.example.net.evil.example"} {
			if err = client.Mail(""); err != nil {
				t.Fatal(err)
			}
			if err = client.Rcpt("b+" + recipientID + "@" + domain); err != nil {
				t.Errorf("listener %s refused a bounce at %s: %v", hostnames[i], domain, err)
			}
			if err = client.Reset(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
