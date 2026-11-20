package bounce

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/netip"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"

	smtp "github.com/zyno-io/thinpost/internal/thirdparty/gosmtp"
)

const (
	testCredID     = "0123456789abcdef0123456789abcdef"
	testCredSecret = "tps_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testMessageID  = "fedcba9876543210fedcba9876543210"
)

// fakeSubmission is the trusted side: it verifies one credential and records
// every submitted message.
type fakeSubmission struct {
	mu        sync.Mutex
	down      bool
	verifies  int
	verify    func(Credential) AuthResult
	submit    func(context.Context, SubmissionMessage) (Reply, error)
	messages  []SubmissionMessage
	submitted chan struct{}
}

func newFakeSubmission() *fakeSubmission { return &fakeSubmission{submitted: make(chan struct{}, 64)} }

func (f *fakeSubmission) setDown(v bool) {
	f.mu.Lock()
	f.down = v
	f.mu.Unlock()
}
func (f *fakeSubmission) Available() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.down
}
func (f *fakeSubmission) Verify(_ context.Context, c Credential, _ netip.Addr) (AuthResult, error) {
	f.mu.Lock()
	f.verifies++
	verify := f.verify
	f.mu.Unlock()
	if verify != nil {
		return verify(c), nil
	}
	if c.ID == testCredID && c.Secret == testCredSecret {
		return AuthResult{OK: true, Reply: Reply{Code: 235, Enhanced: "2.7.0", Text: "Authentication successful"}}, nil
	}
	return AuthResult{Invalid: true, Reply: Reply{Code: 535, Enhanced: "5.7.8", Text: "Authentication credentials invalid"}}, nil
}
func (f *fakeSubmission) Submit(ctx context.Context, m SubmissionMessage) (Reply, error) {
	f.mu.Lock()
	f.messages = append(f.messages, m)
	submit := f.submit
	f.mu.Unlock()
	f.submitted <- struct{}{}
	if submit != nil {
		return submit(ctx, m)
	}
	return Reply{Code: 250, Enhanced: "2.0.0", Text: "OK: queued as " + testMessageID}, nil
}
func (f *fakeSubmission) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}
func (f *fakeSubmission) last() SubmissionMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.messages[len(f.messages)-1]
}

type fakeLimiter struct {
	mu      sync.Mutex
	gate    AuthGate
	checks  int
	refunds int
}

func (l *fakeLimiter) Check(netip.Addr) AuthGate {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.checks++
	return l.gate
}
func (l *fakeLimiter) Succeeded(netip.Addr) {
	l.mu.Lock()
	l.refunds++
	l.mu.Unlock()
}
func (l *fakeLimiter) counts() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.checks, l.refunds
}

type subStats struct {
	mu sync.Mutex
	n  map[string]int
}

func (s *subStats) Count(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == nil {
		s.n = map[string]int{}
	}
	s.n[name]++
}
func (s *subStats) get(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n[name]
}

// subEnv is one running submission listener.
type subEnv struct {
	t        *testing.T
	addr     string
	implicit bool
	sub      *fakeSubmission
	cert     tls.Certificate
	hostname string
	server   *smtp.Server
}

type subOptions struct {
	implicit bool
	internal bool
	cfg      SubmissionConfig
	limits   *clampLimits
	hostname string
}

func startSubmission(t *testing.T, o subOptions) *subEnv {
	t.Helper()
	if o.hostname == "" {
		o.hostname = "smtp.submission.example"
	}
	if o.cfg.Submission == nil {
		o.cfg.Submission = newFakeSubmission()
	}
	cert := selfSigned(t, o.hostname)
	port := uint32(587)
	if o.implicit {
		port = 465
	}
	if o.internal {
		port = 0
	}
	servers, err := NewSubmissionServers(o.cfg, []SubmissionListener{{Hostname: o.hostname, TLS: &tls.Config{Certificates: []tls.Certificate{cert}}, Port: port, Internal: o.internal}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	limits := defaultClampLimits()
	if o.limits != nil {
		limits = *o.limits
	}
	var listener net.Listener = &clampListener{Listener: raw, limits: limits}
	if o.implicit {
		listener = tls.NewListener(listener, servers[0].TLSConfig)
	}
	go func() { _ = servers[0].Serve(listener) }()
	t.Cleanup(func() { _ = servers[0].Close() })
	sub, _ := o.cfg.Submission.(*fakeSubmission)
	return &subEnv{t: t, addr: raw.Addr().String(), implicit: o.implicit, sub: sub, cert: cert, hostname: o.hostname, server: servers[0]}
}

type subClient struct {
	t    *testing.T
	conn net.Conn
	r    *textproto.Reader
}

func (e *subEnv) dial() *subClient {
	e.t.Helper()
	var conn net.Conn
	var err error
	if e.implicit {
		conn, err = tls.Dial("tcp", e.addr, &tls.Config{InsecureSkipVerify: true, ServerName: e.hostname})
	} else {
		conn, err = net.Dial("tcp", e.addr)
	}
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	c := &subClient{t: e.t, conn: conn, r: textproto.NewReader(bufio.NewReader(conn))}
	if code, _ := c.reply(); code != 220 {
		e.t.Fatalf("greeting %d", code)
	}
	return c
}

// ready dials, says EHLO, upgrades with STARTTLS where needed and authenticates.
func (e *subEnv) ready() *subClient {
	e.t.Helper()
	c := e.dial()
	c.ehlo()
	if !e.implicit {
		c.startTLS()
		c.ehlo()
	}
	c.expect(235, "AUTH PLAIN "+plainIR("", testCredID, testCredSecret))
	return c
}

func (c *subClient) reply() (int, string) {
	c.t.Helper()
	code, msg, err := c.r.ReadResponse(0)
	if err != nil {
		return 0, err.Error()
	}
	return code, msg
}
func (c *subClient) send(s string) {
	c.t.Helper()
	if _, err := c.conn.Write([]byte(s)); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}
func (c *subClient) cmd(line string) (int, string) {
	c.t.Helper()
	c.send(line + "\r\n")
	return c.reply()
}
func (c *subClient) expect(want int, line string) string {
	c.t.Helper()
	code, msg := c.cmd(line)
	if code != want {
		c.t.Fatalf("%q: got %d %q, want %d", redact(line), code, msg, want)
	}
	return msg
}
func redact(line string) string {
	if strings.HasPrefix(strings.ToUpper(line), "AUTH") {
		return "AUTH ..."
	}
	return line
}
func (c *subClient) ehlo() string {
	c.t.Helper()
	return c.expect(250, "EHLO client.example.net")
}
func (c *subClient) startTLS() {
	c.t.Helper()
	c.expect(220, "STARTTLS")
	tc := tls.Client(c.conn, &tls.Config{InsecureSkipVerify: true, ServerName: "smtp.submission.example"})
	if err := tc.Handshake(); err != nil {
		c.t.Fatal(err)
	}
	c.conn = tc
	c.r = textproto.NewReader(bufio.NewReader(tc))
}

// envelope sends MAIL and RCPT and the DATA command up to the 354.
func (c *subClient) envelope(rcpts ...string) {
	c.t.Helper()
	c.expect(250, "MAIL FROM:<sender@example.com>")
	if len(rcpts) == 0 {
		rcpts = []string{"to@example.net"}
	}
	for _, r := range rcpts {
		c.expect(250, "RCPT TO:<"+r+">")
	}
	c.expect(354, "DATA")
}

// stuff dot-stuffs a decoded body for the wire and adds the terminator.
func stuff(decoded string) string {
	var b strings.Builder
	atLineStart := true
	for i := 0; i < len(decoded); i++ {
		if atLineStart && decoded[i] == '.' {
			b.WriteByte('.')
		}
		b.WriteByte(decoded[i])
		atLineStart = decoded[i] == '\n'
	}
	return b.String() + ".\r\n"
}

const simpleBody = "From: sender@example.com\r\nTo: to@example.net\r\nSubject: Hello\r\n\r\nBody.\r\n"

func (c *subClient) message(body string) (int, string) {
	c.t.Helper()
	c.envelope()
	c.send(stuff(body))
	return c.reply()
}

func plainIR(identity, user, pass string) string {
	return b64(identity + "\x00" + user + "\x00" + pass)
}

func (c *subClient) readEOF() bool {
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	for {
		if _, err := c.conn.Read(buf); err != nil {
			var ne net.Error
			return !(errors.As(err, &ne) && ne.Timeout())
		}
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func smallBudget(n int64) *semaphore.Weighted { return semaphore.NewWeighted(n) }

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
