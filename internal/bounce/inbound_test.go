package bounce

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"
)

type routeEntry struct {
	group uint32
	state RouteState
}

type fakeInbound struct {
	routes  map[string]routeEntry
	mu      sync.Mutex
	verdict Verdict
	err     error
	gate    chan struct{}
	got     chan Message
}

func (f *fakeInbound) Lookup(domain string) (uint32, RouteState) {
	if r, ok := f.routes[domain]; ok {
		return r.group, r.state
	}
	return 0, RouteUnknown
}
func (f *fakeInbound) Forward(ctx context.Context, m Message) (Verdict, error) {
	f.got <- m
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return f.verdict, f.err
}

type countStats struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *countStats) Count(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[name]++
}
func (c *countStats) get(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[name]
}

func setupInbound(t *testing.T, f *fakeInbound) (*smtp.Client, *fakeForwarder, *countStats) {
	t.Helper()
	fw := &fakeForwarder{received: make(chan string, 1), release: make(chan error, 1)}
	stats := &countStats{}
	server, err := NewServer(Config{Hostname: "smtp.mx.example", Domains: []string{"bounce.example"}, TLS: &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return nil, errors.New("test certificate unavailable")
	}}, MaxBytes: 16 << 20, MaxConcurrent: 1, ForwardTimeout: time.Second, Forwarder: fw, Inbound: f, InboundForwardTimeout: 2 * time.Second, Stats: stats})
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
	return client, fw, stats
}

func newFake() *fakeInbound {
	return &fakeInbound{
		routes: map[string]routeEntry{
			"a.example":    {1, RouteAccept},
			"a2.example":   {1, RouteAccept},
			"b.example":    {2, RouteAccept},
			"paused.test":  {3, RouteTempfailState},
			"nosession.te": {4, RouteTempfailNoSession},
		},
		verdict: VerdictAccepted,
		got:     make(chan Message, 8),
	}
}

func smtpReply(t *testing.T, err error) (int, string) {
	t.Helper()
	if err == nil {
		return 250, ""
	}
	return smtpCode(err), err.Error()
}

func TestInboundRoutingDecisionTable(t *testing.T) {
	for _, tc := range []struct {
		name, rcpt string
		code       int
		enhanced   string
	}{
		{"snapshot accept", "user@a.example", 250, ""},
		{"domain is case-insensitive", "User@A.Example", 250, ""},
		{"app paused or no endpoint", "user@paused.test", 451, "4.3.0"},
		{"no session", "user@nosession.te", 451, "4.3.0"},
		{"unknown domain", "user@elsewhere.example", 550, "5.7.1"},
		{"bounce domain with a bad token", "user@bounce.example", 550, "5.1.1"},
		{"bounce domain with a token", "b+0123456789abcdef0123456789abcdef@bounce.example", 250, ""},
		{"percent hack", "user%a.example@elsewhere.example", 550, "5.1.1"},
		{"source route is dropped, never relayed", "@a.example:user@elsewhere.example", 550, ""},
		{"non-ascii", "usér@a.example", 550, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _, _ := setupInbound(t, newFake())
			if err := client.Mail("sender@example.org"); err != nil {
				t.Fatal(err)
			}
			code, text := smtpReply(t, client.Rcpt(tc.rcpt))
			if code != tc.code || tc.enhanced != "" && !strings.Contains(text, tc.enhanced) {
				t.Fatalf("%s: got %d %q", tc.rcpt, code, text)
			}
		})
	}
}

func TestBounceTokenAtAnyDomain(t *testing.T) {
	const tok = "b+0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name, rcpt string
		code       int
		enhanced   string
		counter    string // expected to be 1; empty for none
	}{
		{"own HELO name", tok + "@bounce.example", 250, "", ""},
		{"own HELO name, bad token stays strict", "b+XYZ@bounce.example", 550, "5.1.1", ""},
		{"inbound route accepts", "user@a.example", 250, "", "rcpt_accept"},
		{"inbound route tempfail", "user@paused.test", 451, "4.3.0", "rcpt_tempfail_state"},
		{"inbound route no session", "user@nosession.te", 451, "4.3.0", "rcpt_tempfail_no_session"},
		{"token at unknown domain", tok + "@custom.example", 250, "", "rcpt_bounce_other_domain"},
		{"token at unknown domain, case-insensitive domain", tok + "@Custom.Example", 250, "", "rcpt_bounce_other_domain"},
		{"inbound wins over a token", tok + "@a.example", 250, "", "rcpt_accept"},
		{"inbound tempfail wins over a token", tok + "@paused.test", 451, "4.3.0", "rcpt_tempfail_state"},
		{"plain local part at unknown domain", "user@custom.example", 550, "5.7.1", "rcpt_relay_denied"},
		{"uppercase hex is not a bounce", "b+0123456789ABCDEF0123456789abcdef@custom.example", 550, "5.7.1", "rcpt_relay_denied"},
		{"short token is not a bounce", "b+0123456789abcdef@custom.example", 550, "5.7.1", "rcpt_relay_denied"},
		{"long token is not a bounce", tok + "00@custom.example", 550, "5.7.1", "rcpt_relay_denied"},
		{"non-hex token is not a bounce", "b+0123456789abcdef0123456789abcdeg@custom.example", 550, "5.7.1", "rcpt_relay_denied"},
		{"missing b+ prefix", "0123456789abcdef0123456789abcdef@custom.example", 550, "5.7.1", "rcpt_relay_denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _, stats := setupInbound(t, newFake())
			if err := client.Mail("sender@example.org"); err != nil {
				t.Fatal(err)
			}
			code, text := smtpReply(t, client.Rcpt(tc.rcpt))
			if code != tc.code || tc.enhanced != "" && !strings.Contains(text, tc.enhanced) {
				t.Fatalf("%s: got %d %q", tc.rcpt, code, text)
			}
			if tc.counter != "" && stats.get(tc.counter) != 1 {
				t.Fatalf("%s: counter %s = %d", tc.rcpt, tc.counter, stats.get(tc.counter))
			}
			if tc.counter != "rcpt_bounce_other_domain" && stats.get("rcpt_bounce_other_domain") != 0 {
				t.Fatal("bounce-other-domain counted for a non-bounce recipient")
			}
		})
	}
}

// A bounce at a custom domain is one recipient, never mixed with inbound mail.
func TestBounceAtOtherDomainTransactionRules(t *testing.T) {
	const tok = "b+0123456789abcdef0123456789abcdef"
	client, _, _ := setupInbound(t, newFake())
	if err := client.Mail("sender@example.org"); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt(tok + "@custom.example"); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("user@a.example"); smtpCode(err) != 452 {
		t.Fatalf("inbound after bounce: %v", err)
	}
	if err := client.Rcpt(tok + "@other.example"); smtpCode(err) != 452 {
		t.Fatalf("second bounce: %v", err)
	}
}

// With no inbound feature, a token at another domain is still a bounce and
// anything else stays "unknown recipient".
func TestBounceAtOtherDomainWithoutInbound(t *testing.T) {
	client, _ := setup(t, 1<<20)
	if err := client.Mail("sender@example.org"); err != nil {
		t.Fatal(err)
	}
	if code, _ := smtpReply(t, client.Rcpt("user@custom.example")); code != 550 {
		t.Fatalf("plain: %d", code)
	}
	if err := client.Rcpt("b+0123456789abcdef0123456789abcdef@custom.example"); err != nil {
		t.Fatalf("token: %v", err)
	}
}

func TestInboundClassMixingAndRecipientCaps(t *testing.T) {
	client, _, _ := setupInbound(t, newFake())
	if err := client.Mail("sender@example.org"); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("one@a.example"); err != nil {
		t.Fatal(err)
	}
	// Same app, another domain: fine. Another app: later, in its own transaction.
	if err := client.Rcpt("two@a2.example"); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("three@b.example"); smtpCode(err) != 452 {
		t.Fatalf("another app: %v", err)
	}
	// A bounce recipient may not share a transaction with inbound recipients.
	if err := client.Rcpt("b+0123456789abcdef0123456789abcdef@bounce.example"); smtpCode(err) != 452 {
		t.Fatalf("bounce after inbound: %v", err)
	}
	// An unknown or paused domain still gets its own exact answer here.
	if err := client.Rcpt("x@paused.test"); smtpCode(err) != 451 {
		t.Fatalf("paused: %v", err)
	}
	// 50 recipients per transaction.
	for i := 2; i < 50; i++ {
		if err := client.Rcpt("r" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + "@a.example"); err != nil {
			t.Fatalf("recipient %d: %v", i, err)
		}
	}
	if err := client.Rcpt("overflow@a.example"); smtpCode(err) != 452 {
		t.Fatalf("recipient 51: %v", err)
	}

	// And the other way round: a bounce recipient first.
	client2, _, _ := setupInbound(t, newFake())
	if err := client2.Mail(""); err != nil {
		t.Fatal(err)
	}
	if err := client2.Rcpt("b+0123456789abcdef0123456789abcdef@bounce.example"); err != nil {
		t.Fatal(err)
	}
	if err := client2.Rcpt("user@a.example"); smtpCode(err) != 452 {
		t.Fatalf("inbound after bounce: %v", err)
	}
}

func TestInboundSizeLimits(t *testing.T) {
	f := newFake()
	client, _, _ := setupInbound(t, f)
	// SIZE above the 10 MiB inbound limit (but below the advertised maximum) is
	// refused at RCPT; the bounce class keeps the larger limit.
	if err := client.Hello("client.example.org"); err != nil {
		t.Fatal(err)
	}
	id, err := client.Text.Cmd("MAIL FROM:<s@example.org> SIZE=11000000")
	if err != nil {
		t.Fatal(err)
	}
	client.Text.StartResponse(id)
	_, _, err = client.Text.ReadResponse(250)
	client.Text.EndResponse(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("user@a.example"); smtpCode(err) != 552 {
		t.Fatalf("SIZE over the limit: %v", err)
	}
	if err := client.Rcpt("b+0123456789abcdef0123456789abcdef@bounce.example"); err != nil {
		t.Fatalf("the bounce class lost its larger limit: %v", err)
	}

	// DATA over the limit is refused without forwarding.
	client2, _, _ := setupInbound(t, f)
	if err := client2.Mail("s@example.org"); err != nil {
		t.Fatal(err)
	}
	if err := client2.Rcpt("user@a.example"); err != nil {
		t.Fatal(err)
	}
	writer, err := client2.Data()
	if err != nil {
		t.Fatal(err)
	}
	chunk := []byte(strings.Repeat("a", 1023) + "\n")
	for i := 0; i < 10*1024+1; i++ {
		if _, err = writer.Write(chunk); err != nil {
			break
		}
	}
	err = writer.Close()
	if smtpCode(err) != 552 {
		t.Fatalf("DATA over the limit: %v", err)
	}
	select {
	case <-f.got:
		t.Fatal("an oversized message was forwarded")
	default:
	}
}

func TestInboundVerdictMapping(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict Verdict
		err     error
		code    int
	}{
		{"accepted", VerdictAccepted, nil, 250},
		{"tempfail", VerdictTempfail, nil, 451},
		{"rejected", VerdictRejected, nil, 554},
		{"stream lost", 0, errors.New("inbound session closed"), 451},
		{"unknown verdict", 0, nil, 451},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.verdict, f.err = tc.verdict, tc.err
			client, _, stats := setupInbound(t, f)
			if err := client.Mail("sender@example.org"); err != nil {
				t.Fatal(err)
			}
			if err := client.Rcpt("one@a.example"); err != nil {
				t.Fatal(err)
			}
			if err := client.Rcpt("two@a.example"); err != nil {
				t.Fatal(err)
			}
			writer, err := client.Data()
			if err != nil {
				t.Fatal(err)
			}
			_, _ = writer.Write([]byte("Subject: hi\r\n\r\nbody\r\n"))
			err = writer.Close()
			code, text := smtpReply(t, err)
			if code != tc.code {
				t.Fatalf("got %d %q", code, text)
			}
			for _, secret := range []string{"inbound session closed", "one@a.example"} {
				if strings.Contains(text, secret) {
					t.Fatalf("reply leaks %q", secret)
				}
			}
			m := <-f.got
			if m.MailFrom != "sender@example.org" || len(m.Rcpts) != 2 || m.Rcpts[0] != "one@a.example" || m.Group != 1 || m.Receiver != "smtp.mx.example" || !m.ClientIP.IsValid() || !m.ClientIP.IsLoopback() || m.Helo == "" || string(m.Body) != "Subject: hi\r\n\r\nbody\r\n" || len(m.ID) != 32 || m.TLSVer != "" {
				t.Fatalf("message facts %+v", m)
			}
			if stats.get("rcpt_accept") != 2 {
				t.Fatal("recipient counter")
			}
		})
	}
}

// 250 is sent only after Forward returns, not when the data ends.
func TestInboundAcknowledgedOnlyAfterCommit(t *testing.T) {
	f := newFake()
	f.gate = make(chan struct{})
	client, _, _ := setupInbound(t, f)
	if err := client.Mail("sender@example.org"); err != nil {
		t.Fatal(err)
	}
	if err := client.Rcpt("one@a.example"); err != nil {
		t.Fatal(err)
	}
	writer, err := client.Data()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte("body\r\n"))
	done := make(chan error, 1)
	go func() { done <- writer.Close() }()
	<-f.got
	select {
	case err = <-done:
		t.Fatalf("SMTP acknowledged before the commit: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(f.gate)
	if err = <-done; err != nil {
		t.Fatalf("committed message refused: %v", err)
	}
}

func TestInboundInvalidRecipientBudgetClosesConnection(t *testing.T) {
	client, _, _ := setupInbound(t, newFake())
	if err := client.Mail("sender@example.org"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := client.Rcpt("user@elsewhere.example"); smtpCode(err) != 550 {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if err := client.Rcpt("user@elsewhere.example"); smtpCode(err) != 421 {
		t.Fatalf("budget not enforced: %v", err)
	}
	// The connection is closed shortly after the 421.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := client.Noop(); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("connection stayed open")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestInboundTransactionsPerConnection(t *testing.T) {
	client, _, _ := setupInbound(t, newFake())
	for i := 0; i < 20; i++ {
		if err := client.Mail("sender@example.org"); err != nil {
			t.Fatalf("transaction %d: %v", i, err)
		}
		if err := client.Reset(); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.Mail("sender@example.org"); smtpCode(err) != 421 {
		t.Fatalf("transaction 21: %v", err)
	}
}

func TestInboundDataSlotsAreSeparatePools(t *testing.T) {
	f := newFake()
	f.gate = make(chan struct{})
	fw := &fakeForwarder{received: make(chan string, 1), release: make(chan error, 1)}
	server, err := NewServer(Config{Hostname: "smtp.mx.example", Domains: []string{"bounce.example"}, TLS: &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return nil, errors.New("unavailable")
	}}, MaxBytes: 16 << 20, MaxConcurrent: 1, ForwardTimeout: 5 * time.Second, Forwarder: fw, Inbound: f, InboundConcurrent: 1, InboundForwardTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	dial := func() *smtp.Client {
		c, err := smtp.Dial(listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	first := dial()
	_ = first.Mail("s@example.org")
	_ = first.Rcpt("u@a.example")
	w1, _ := first.Data()
	_, _ = w1.Write([]byte("one\r\n"))
	firstDone := make(chan error, 1)
	go func() { firstDone <- w1.Close() }()
	<-f.got
	// The only inbound slot is busy: a second inbound DATA is a tempfail...
	second := dial()
	_ = second.Mail("s@example.org")
	_ = second.Rcpt("u@a.example")
	w2, _ := second.Data()
	_, _ = w2.Write([]byte("two\r\n"))
	if err = w2.Close(); smtpCode(err) != 451 {
		t.Fatalf("second inbound: %v", err)
	}
	// ...while a bounce still has its own slot.
	third := dial()
	_ = third.Mail("")
	_ = third.Rcpt("b+0123456789abcdef0123456789abcdef@bounce.example")
	w3, _ := third.Data()
	_, _ = w3.Write([]byte("bounce\r\n"))
	thirdDone := make(chan error, 1)
	go func() { thirdDone <- w3.Close() }()
	<-fw.received
	fw.release <- nil
	if err = <-thirdDone; err != nil {
		t.Fatalf("bounce blocked by inbound: %v", err)
	}
	close(f.gate)
	if err = <-firstDone; err != nil {
		t.Fatal(err)
	}
}

// A client cannot put anything on the control stream that would end it: an
// oversized or non-ASCII reverse path is refused at MAIL, and the EHLO name is
// bounded to printable ASCII before it is forwarded.
func TestInboundSenderAndHeloAreBoundedBeforeForwarding(t *testing.T) {
	f := newFake()
	client, _, _ := setupInbound(t, f)
	if err := client.Hello(strings.Repeat("h", 300) + "\xff" + strings.Repeat("\u00e9", 150)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Repeat("a", 405) + "\xfe@example.org", strings.Repeat("a", 411), "caf\xc3\xa9@example.org", "a b@example.org"} {
		id, err := client.Text.Cmd("MAIL FROM:<%s>", bad)
		if err != nil {
			t.Fatal(err)
		}
		client.Text.StartResponse(id)
		code, _, _ := client.Text.ReadResponse(0)
		client.Text.EndResponse(id)
		if code != 501 {
			t.Fatalf("%d-byte reverse path accepted: %d", len(bad), code)
		}
	}
	if err := client.Mail("ok@example.org"); err != nil {
		t.Fatalf("the connection is unusable after a refused reverse path: %v", err)
	}
	if err := client.Rcpt("user@a.example"); err != nil {
		t.Fatal(err)
	}
	w, err := client.Data()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("x\r\n"))
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	m := <-f.got
	if len(m.Helo) == 0 || len(m.Helo) > 255 || !printableASCII(m.Helo) || len(m.MailFrom) > 320 {
		t.Fatalf("forwarded %d-byte HELO %q", len(m.Helo), m.Helo)
	}
}
