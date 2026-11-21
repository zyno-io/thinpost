package bounce

import (
	"crypto/tls"
	"testing"
	"time"
)

// Each sending IP may use its own bounce domain; every configured one is
// accepted, and a valid token is accepted at any other domain.
func TestBounceAcceptsEveryConfiguredDomain(t *testing.T) {
	client, _ := setupDomains(t, 1024, "bounce.example", "bounce.hosting.example")
	if err := client.Mail(""); err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"bounce.example", "Bounce.Hosting.Example"} {
		if err := client.Rcpt("b+0123456789abcdef0123456789abcdef@" + domain); err != nil {
			t.Fatalf("%s: %v", domain, err)
		}
		if err := client.Reset(); err != nil {
			t.Fatal(err)
		}
		if err := client.Mail(""); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.Rcpt("b+0123456789abcdef0123456789abcdef@other.example"); err != nil {
		t.Fatalf("unconfigured domain: %v", err)
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{{}}}
	if _, err := NewServer(Config{Hostname: "smtp.mx.example", TLS: tlsConfig, MaxBytes: 1, MaxConcurrent: 1, ForwardTimeout: time.Second, Forwarder: &fakeForwarder{}}); err == nil {
		t.Fatal("accepted a receiver with no bounce domain")
	}
}
