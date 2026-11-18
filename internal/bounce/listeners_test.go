package bounce

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"
)

func selfSigned(t *testing.T, name string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// twoListeners serves two names from one receiver core and returns their
// addresses.
func twoListeners(t *testing.T, f *fakeForwarder, inbound Inbound) (string, string) {
	t.Helper()
	listeners := []Listener{
		{Hostname: "smtp.mx.example", TLS: &tls.Config{Certificates: []tls.Certificate{selfSigned(t, "smtp.mx.example")}}},
		{Hostname: "smtp.second.example", TLS: &tls.Config{Certificates: []tls.Certificate{selfSigned(t, "smtp.second.example")}}},
	}
	servers, err := NewServers(Config{Hostname: "smtp.mx.example", Domains: []string{"bounce.example", "smtp.second.example"}, MaxBytes: 1 << 20, MaxConcurrent: 1, ForwardTimeout: time.Second, Forwarder: f, Inbound: inbound, InboundForwardTimeout: 2 * time.Second}, listeners)
	if err != nil || len(servers) != 2 {
		t.Fatalf("servers: %d %v", len(servers), err)
	}
	var addresses []string
	for _, server := range servers {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(func() { _ = server.Close() })
		addresses = append(addresses, listener.Addr().String())
	}
	return addresses[0], addresses[1]
}

func TestEachListenerGreetsAndPresentsItsOwnName(t *testing.T) {
	first, second := twoListeners(t, &fakeForwarder{received: make(chan string, 1), release: make(chan error, 1)}, nil)
	for address, name := range map[string]string{first: "smtp.mx.example", second: "smtp.second.example"} {
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		client, err := smtp.NewClient(conn, "ignored")
		if err != nil {
			t.Fatal(err)
		}
		if err = client.Hello("probe.invalid"); err != nil {
			t.Fatal(err)
		}
		// The 250 reply to EHLO names the listener, as the banner does.
		if ok, _ := client.Extension("STARTTLS"); !ok {
			t.Fatalf("%s: STARTTLS not offered", name)
		}
		roots := x509.NewCertPool()
		var seen string
		config := &tls.Config{ServerName: name, InsecureSkipVerify: true, VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			leaf, err := x509.ParseCertificate(raw[0])
			if err == nil {
				seen = strings.Join(leaf.DNSNames, ",")
			}
			return err
		}, RootCAs: roots}
		if err = client.StartTLS(config); err != nil {
			t.Fatal(err)
		}
		if seen != name {
			t.Fatalf("listener for %s presented a certificate for %q", name, seen)
		}
	}
}

func TestListenerBannerNamesItself(t *testing.T) {
	first, second := twoListeners(t, &fakeForwarder{received: make(chan string, 1), release: make(chan error, 1)}, nil)
	for address, name := range map[string]string{first: "smtp.mx.example", second: "smtp.second.example"} {
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 128)
		n, err := conn.Read(buf)
		_ = conn.Close()
		if err != nil || !strings.HasPrefix(string(buf[:n]), "220 "+name+" ") {
			t.Fatalf("banner for %s: %q %v", name, buf[:n], err)
		}
	}
}

// Both listeners share the single bounce DATA slot, accept the bounce domains
// of every listener, and report the receiver (primary) name for inbound mail.
func TestListenersShareSlotsDomainsAndReceiver(t *testing.T) {
	f := &fakeForwarder{received: make(chan string, 2), release: make(chan error, 2)}
	inbound := newFake()
	first, second := twoListeners(t, f, inbound)
	a, err := smtp.Dial(first)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := smtp.Dial(second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	// A bounce to the other listener's name is accepted on either listener.
	for _, c := range []*smtp.Client{a, b} {
		if err = c.Mail(""); err != nil {
			t.Fatal(err)
		}
		if err = c.Rcpt("b+0123456789abcdef0123456789abcdef@smtp.second.example"); err != nil {
			t.Fatal(err)
		}
		if err = c.Reset(); err != nil {
			t.Fatal(err)
		}
	}
	// Hold the one DATA slot through listener A; listener B must be refused.
	begin(t, a)
	writerA, err := a.Data()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writerA.Write([]byte("Subject: one\r\n\r\nbody\r\n"))
	doneA := make(chan error, 1)
	go func() { doneA <- writerA.Close() }()
	select {
	case <-f.received:
	case <-time.After(time.Second):
		t.Fatal("first bounce was not forwarded")
	}
	begin(t, b)
	writerB, err := b.Data()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writerB.Write([]byte("Subject: two\r\n\r\nbody\r\n"))
	if err = writerB.Close(); smtpCode(err) != 451 {
		t.Fatalf("second listener was not held to the shared slot: %v", err)
	}
	f.release <- nil
	if err = <-doneA; err != nil {
		t.Fatal(err)
	}
	// Inbound mail on the second listener carries the primary name as receiver.
	if err = b.Mail("sender@example.org"); err != nil {
		t.Fatal(err)
	}
	if err = b.Rcpt("one@a.example"); err != nil {
		t.Fatal(err)
	}
	writer, err := b.Data()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte("Subject: hi\r\n\r\nbody\r\n"))
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-inbound.got:
		if m.Receiver != "smtp.mx.example" {
			t.Fatalf("receiver %q, want the primary name", m.Receiver)
		}
	case <-time.After(time.Second):
		t.Fatal("inbound message was not forwarded")
	}
}

func TestNewServersValidation(t *testing.T) {
	f := &fakeForwarder{}
	good := Listener{Hostname: "smtp.a.example", TLS: &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }}}
	base := Config{Hostname: "smtp.a.example", Domains: []string{"bounce.example"}, MaxBytes: 1024, MaxConcurrent: 1, ForwardTimeout: time.Second, Forwarder: f}
	if _, err := NewServers(base, nil); err == nil {
		t.Error("no listeners accepted")
	}
	if _, err := NewServers(base, []Listener{{Hostname: "bad name", TLS: good.TLS}}); err == nil {
		t.Error("invalid listener name accepted")
	}
	if _, err := NewServers(base, []Listener{{Hostname: "smtp.a.example"}}); err == nil {
		t.Error("listener without TLS accepted")
	}
	var domains []string
	for i := 0; i < MaxDomains+1; i++ {
		domains = append(domains, "d"+string(rune('a'+i%26))+string(rune('a'+i/26))+".example")
	}
	base.Domains = domains
	if _, err := NewServers(base, []Listener{good}); err == nil {
		t.Error("more than the domain cap accepted")
	}
	base.Domains = domains[:MaxDomains]
	if _, err := NewServers(base, []Listener{good}); err != nil {
		t.Errorf("exactly the domain cap rejected: %v", err)
	}
}
