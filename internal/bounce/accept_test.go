package bounce

import (
	"crypto/tls"
	"net"
	"net/smtp"
	"testing"
	"time"
)

// The Edge passes its listener hostnames as the bounce domains: a bounce
// address is accepted at every one of them, on any listener, and nowhere else.
func TestBouncesAreAcceptedAtEveryListenerHostnameAndNotElsewhere(t *testing.T) {
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
	const token = "0123456789abcdef0123456789abcdef"
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
			if err = client.Rcpt("b+" + token + "@" + domain); err != nil {
				t.Errorf("listener %s refused a bounce at %s: %v", hostnames[i], domain, err)
			}
			if err = client.Reset(); err != nil {
				t.Fatal(err)
			}
		}
		// Names that are not a listener hostname, including a former global
		// bounce domain and a parent of a listener name.
		for _, domain := range []string{"bounce.example.net", "example.net", "other.example", "mx.example.net.evil.example"} {
			if err = client.Mail(""); err != nil {
				t.Fatal(err)
			}
			if err = client.Rcpt("b+" + token + "@" + domain); smtpCode(err) != 550 {
				t.Errorf("listener %s accepted a bounce at %s: %v", hostnames[i], domain, err)
			}
			if err = client.Reset(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
