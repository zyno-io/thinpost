package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/zyno-io/thinpost/internal/bounce"
	"github.com/zyno-io/thinpost/internal/edgeapp"
	"github.com/zyno-io/thinpost/internal/edgeprotocol"
)

const (
	loadCredID     = "0123456789abcdef0123456789abcdef"
	loadCredSecret = "tps_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	loadMessageID  = "fedcba9876543210fedcba9876543210"
)

// loadSubmission is a trusted side that accepts one credential and tracks how
// many submissions are inside Submit at once.
type loadSubmission struct {
	inflight, maxInflight, accepted atomic.Int64
	delay                           time.Duration
}

func (l *loadSubmission) Available() bool { return true }
func (l *loadSubmission) Verify(_ context.Context, c bounce.Credential, _ netip.Addr) (bounce.AuthResult, error) {
	if c.ID == loadCredID && c.Secret == loadCredSecret {
		return bounce.AuthResult{OK: true, Reply: bounce.Reply{Code: 235}}, nil
	}
	return bounce.AuthResult{Invalid: true}, nil
}
func (l *loadSubmission) Submit(_ context.Context, m bounce.SubmissionMessage) (bounce.Reply, error) {
	n := l.inflight.Add(1)
	defer l.inflight.Add(-1)
	for {
		max := l.maxInflight.Load()
		if n <= max || l.maxInflight.CompareAndSwap(max, n) {
			break
		}
	}
	time.Sleep(l.delay)
	l.accepted.Add(1)
	return bounce.Reply{Code: 250, Enhanced: "2.0.0", Text: "OK: queued as " + loadMessageID}, nil
}

type testPKI struct {
	authority *x509.Certificate
	caKey     *ecdsa.PrivateKey
	roots     *x509.CertPool
	serial    int64
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return &testPKI{authority: ca, caKey: key, roots: roots, serial: 1}
}

// issue writes a certificate for name and returns its cert and key paths.
func (p *testPKI) issue(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	p.serial++
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(p.serial), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, p.authority, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	certPath, keyPath := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

type submissionFixture struct {
	rt      *submissionRuntime
	pki     *testPKI
	budget  *semaphore.Weighted
	submit  *loadSubmission
	starttl []string // 587-style addresses
	implict []string // 465-style addresses
	intern  string
	names   []string
}

func startFixture(t *testing.T, addresses []string, internal bool) *submissionFixture {
	t.Helper()
	pki := newTestPKI(t)
	dir := t.TempDir()
	controlCert, _ := pki.issue(t, dir, "control.internal")
	var listeners []edgeapp.Listener
	var bounceListeners []bounce.Listener
	f := &submissionFixture{pki: pki, budget: semaphore.NewWeighted(edgeprotocol.EdgeSubmissionBudget), submit: &loadSubmission{delay: 20 * time.Millisecond}}
	for i, address := range addresses {
		name := fmt.Sprintf("smtp%d.example.com", i)
		certPath, keyPath := pki.issue(t, dir, name)
		cfg, err := publicSMTPTLS(certPath, keyPath, name, controlCert, pki.roots)
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, edgeapp.Listener{Address: netip.MustParseAddr(address), Hostname: name, Cert: certPath, Key: keyPath})
		bounceListeners = append(bounceListeners, bounce.Listener{Hostname: name, TLS: cfg})
		f.names = append(f.names, name)
	}
	c := edgeapp.Config{Submission: true, ControlCert: controlCert}
	if internal {
		certPath, keyPath := pki.issue(t, dir, "submission.cluster.example")
		caPath := filepath.Join(dir, "ca.crt")
		if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pki.authority.Raw}), 0o600); err != nil {
			t.Fatal(err)
		}
		c.SubmissionInternal = edgeapp.SubmissionInternal{Listen: netip.MustParseAddrPort("127.0.0.1:0"), Hostname: "submission.cluster.example", Cert: certPath, Key: keyPath, CAFile: caPath}
	}
	rt, err := bindSubmission(c, f.submit, listeners, bounceListeners, submissionBind{starttls: "0", implicit: "0"}, f.budget)
	if err != nil {
		t.Fatal(err)
	}
	f.rt = rt
	for i := range rt.servers {
		go func() { _ = rt.servers[i].Serve(rt.serve[i]) }()
	}
	t.Cleanup(func() { rt.closeServers(); rt.closeListeners() })
	// Listener order: per IP 587 then 465, then the internal one.
	for i := range addresses {
		f.starttl = append(f.starttl, rt.raw[2*i].Addr().String())
		f.implict = append(f.implict, rt.raw[2*i+1].Addr().String())
	}
	if internal {
		f.intern = rt.raw[len(rt.raw)-1].Addr().String()
	}
	return f
}

// send submits a message of size decoded bytes with the standard client.
func sendMessage(addr, serverName string, implicit bool, size int, roots *x509.CertPool) error {
	tlsConfig := &tls.Config{RootCAs: roots, ServerName: serverName}
	var client *smtp.Client
	if implicit {
		conn, err := tls.Dial("tcp", addr, tlsConfig)
		if err != nil {
			return err
		}
		if client, err = smtp.NewClient(conn, serverName); err != nil {
			return err
		}
	} else {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return err
		}
		if client, err = smtp.NewClient(conn, serverName); err != nil {
			return err
		}
		if err = client.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	defer client.Close()
	if err := client.Auth(smtp.PlainAuth("", loadCredID, loadCredSecret, serverName)); err != nil {
		return err
	}
	if err := client.Mail("sender@example.com"); err != nil {
		return err
	}
	if err := client.Rcpt("to@example.net"); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	line := strings.Repeat("x", 70) + "\r\n"
	for written := 0; written < size; written += len(line) {
		if _, err = io.WriteString(w, line); err != nil {
			return err
		}
	}
	if err = w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func TestSubmissionListenersUseEachIPsCertificateAndOmitChunking(t *testing.T) {
	f := startFixture(t, []string{"127.0.0.1"}, true)
	if len(f.rt.servers) != 3 {
		t.Fatalf("servers: %d", len(f.rt.servers))
	}
	// 465: implicit TLS with the IP's own certificate, AUTH at once.
	conn, err := tls.Dial("tcp", f.implict[0], &tls.Config{RootCAs: f.pki.roots, ServerName: f.names[0]})
	if err != nil {
		t.Fatal(err)
	}
	client, err := smtp.NewClient(conn, f.names[0])
	if err != nil {
		t.Fatal(err)
	}
	if ok, auth := client.Extension("AUTH"); !ok || !strings.Contains(auth, "PLAIN") {
		t.Fatal("AUTH not offered on 465")
	}
	if ok, _ := client.Extension("CHUNKING"); ok {
		t.Fatal("CHUNKING offered on 465")
	}
	_ = client.Close()
	// 587 and the internal listener: STARTTLS, AUTH only afterwards.
	for name, check := range map[string]struct{ addr, host string }{"587": {f.starttl[0], f.names[0]}, "internal": {f.intern, "submission.cluster.example"}} {
		raw, err := net.Dial("tcp", check.addr)
		if err != nil {
			t.Fatal(err)
		}
		c, err := smtp.NewClient(raw, check.host)
		if err != nil {
			t.Fatal(err)
		}
		if ok, _ := c.Extension("AUTH"); ok {
			t.Fatalf("%s: AUTH offered before STARTTLS", name)
		}
		if err = c.StartTLS(&tls.Config{RootCAs: f.pki.roots, ServerName: check.host}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ok, _ := c.Extension("AUTH"); !ok {
			t.Fatalf("%s: AUTH not offered after STARTTLS", name)
		}
		if ok, _ := c.Extension("CHUNKING"); ok {
			t.Fatalf("%s: CHUNKING offered", name)
		}
		_ = c.Close()
	}
	if err := sendMessage(f.intern, "submission.cluster.example", false, 2048, f.pki.roots); err != nil {
		t.Fatal(err)
	}
	if f.submit.accepted.Load() != 1 {
		t.Fatal("the internal listener did not deliver")
	}
}

func canBindLoopback(address string) bool {
	l, err := net.Listen("tcp", net.JoinHostPort(address, "0"))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// TestSubmissionLoad runs concurrent 587 and 465 submissions while MX traffic
// on port 25's own admission keeps flowing. The edge byte budget is never
// exceeded (at most four 7.5 MiB reservations at once) and is returned in full.
func TestSubmissionLoad(t *testing.T) {
	f := startFixture(t, []string{"127.0.0.1"}, false)
	// An MX server with its own admission and rate limiter, as in main.
	mxStats := &smtpAdmissionStats{}
	admission, err := newSMTPAdmission(64, 8, mxStats)
	if err != nil {
		t.Fatal(err)
	}
	mxCert, mxKey := f.pki.issue(t, t.TempDir(), "mx.example.com")
	controlCert, _ := f.pki.issue(t, t.TempDir(), "control2.internal")
	mxTLS, err := publicSMTPTLS(mxCert, mxKey, "mx.example.com", controlCert, f.pki.roots)
	if err != nil {
		t.Fatal(err)
	}
	forwarder := &countingForwarder{}
	servers, err := bounce.NewServers(bounce.Config{Hostname: "mx.example.com", Domains: []string{"bounce.example.com"}, MaxBytes: 16 << 20, MaxConcurrent: 1, ForwardTimeout: 5 * time.Second, Forwarder: forwarder}, []bounce.Listener{{Hostname: "mx.example.com", TLS: mxTLS}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = servers[0].Serve(admission.Wrap(raw)) }()
	t.Cleanup(func() { _ = servers[0].Close() })

	const clients = 12
	var wg sync.WaitGroup
	errs := make(chan error, clients+10)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			implicit := i%2 == 1
			addr := f.starttl[0]
			if implicit {
				addr = f.implict[0]
			}
			// 5 MiB keeps the full 7.5 MiB reservation, so at most four run at once.
			if err := sendMessage(addr, f.names[0], implicit, 5<<20, f.pki.roots); err != nil {
				errs <- fmt.Errorf("submission %d: %w", i, err)
			}
		}()
	}
	// Bounce mail on port 25 keeps being admitted and committed meanwhile.
	// The MX takes one bounce at a time, as in main, so they run in sequence.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			if err := sendBounce(raw.Addr().String(), i); err != nil {
				errs <- fmt.Errorf("bounce %d: %w", i, err)
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := f.submit.accepted.Load(); got != clients {
		t.Fatalf("accepted %d submissions, want %d", got, clients)
	}
	if got := f.submit.maxInflight.Load(); got > 4 {
		t.Fatalf("%d large submissions buffered at once: the 32 MiB edge budget allows 4", got)
	}
	if forwarder.count.Load() != 10 {
		t.Fatalf("MX committed %d bounces, want 10", forwarder.count.Load())
	}
	if !f.budget.TryAcquire(edgeprotocol.EdgeSubmissionBudget) {
		t.Fatal("the edge byte budget was not returned in full")
	}
}

type countingForwarder struct{ count atomic.Int64 }

func (f *countingForwarder) Forward(context.Context, string, string, []byte) error {
	f.count.Add(1)
	return nil
}

func sendBounce(addr string, n int) error {
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if err = c.Mail("mailer-daemon@example.net"); err != nil {
		return err
	}
	token := make([]byte, 16)
	token[0] = byte(n + 1)
	if err = c.Rcpt("b+" + hex.EncodeToString(token) + "@bounce.example.com"); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err = io.WriteString(w, "Subject: bounce\r\n\r\nbody\r\n"); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// One source cannot take more than its share of the submission admission, and
// another source is unaffected.
func TestSubmissionAdmissionIsPerSource(t *testing.T) {
	f := startFixture(t, []string{"127.0.0.1"}, false)
	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})
	greeted := 0
	for i := 0; i < 20; i++ {
		conn, err := net.Dial("tcp", f.starttl[0])
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if line, err := bufio.NewReader(conn).ReadString('\n'); err == nil && strings.HasPrefix(line, "220 ") {
			greeted++
		}
	}
	if greeted != 16 {
		t.Fatalf("%d connections from one source were admitted, want 16", greeted)
	}
	if !canBindLoopback("127.0.0.2") {
		t.Skip("127.0.0.2 is not available, cannot test a second source")
	}
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}}
	other, err := dialer.Dial("tcp", f.starttl[0])
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	_ = other.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := bufio.NewReader(other).ReadString('\n'); err != nil || !strings.HasPrefix(line, "220 ") {
		t.Fatalf("a second source was not admitted: %q %v", line, err)
	}
}
