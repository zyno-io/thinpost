package bounce

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingForwarder accepts everything and records the bodies. The delay only
// slows the final forward; stale BDAT workers exit on the pipe reset before
// reaching it. The reset-ordering bug this file guards against is caught by
// the race detector, so run it with -race (as CI does).
type recordingForwarder struct {
	mu     sync.Mutex
	bodies [][]byte
	delay  time.Duration
}

func (f *recordingForwarder) Forward(_ context.Context, _, _ string, body []byte) error {
	time.Sleep(f.delay)
	f.mu.Lock()
	f.bodies = append(f.bodies, append([]byte(nil), body...))
	f.mu.Unlock()
	return nil
}

func (f *recordingForwarder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

type rawClient struct {
	t    *testing.T
	conn net.Conn
	r    *textproto.Reader
}

func startMX(t *testing.T, maxBytes int64, f Forwarder) string {
	t.Helper()
	server, err := NewServer(Config{Hostname: "smtp.mx.example", Domains: []string{"bounce.example"}, TLS: &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return nil, errors.New("test certificate unavailable")
	}}, MaxBytes: maxBytes, MaxConcurrent: 4, ForwardTimeout: 5 * time.Second, Forwarder: f})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String()
}

func dialRaw(t *testing.T, addr string) *rawClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	c := &rawClient{t: t, conn: conn, r: textproto.NewReader(bufio.NewReader(conn))}
	if code := c.reply(); code != 220 {
		t.Fatalf("greeting %d", code)
	}
	c.expect(250, "EHLO client.example.net")
	return c
}

func (c *rawClient) reply() int {
	c.t.Helper()
	code, _, err := c.r.ReadResponse(0)
	if err != nil {
		c.t.Fatalf("read reply: %v", err)
	}
	return code
}

func (c *rawClient) send(b []byte) {
	c.t.Helper()
	if _, err := c.conn.Write(b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *rawClient) expect(want int, line string) {
	c.t.Helper()
	c.send([]byte(line + "\r\n"))
	if got := c.reply(); got != want {
		c.t.Fatalf("%q: got %d, want %d", line, got, want)
	}
}

const bounceRcpt = "RCPT TO:<b+0123456789abcdef0123456789abcdef@bounce.example>"

func TestMXDataExactMaxBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("sends 16 MiB twice")
	}
	const max = maxAllowedBytes
	line := strings.Repeat("x", 1022) + "\r\n"
	exact := strings.Repeat(line, max/len(line))
	if len(exact) != max {
		t.Fatalf("fixture is %d bytes", len(exact))
	}
	f := &recordingForwarder{}
	addr := startMX(t, max, f)

	c := dialRaw(t, addr)
	c.expect(250, "MAIL FROM:<>")
	c.expect(250, bounceRcpt)
	c.expect(354, "DATA")
	c.send([]byte(exact + ".\r\n"))
	if code := c.reply(); code != 250 {
		t.Fatalf("a DATA bounce of exactly MaxBytes: got %d, want 250", code)
	}
	f.mu.Lock()
	if len(f.bodies) != 1 || len(f.bodies[0]) != max || !bytes.Equal(f.bodies[0], []byte(exact)) {
		f.mu.Unlock()
		t.Fatal("exact-limit body not forwarded intact")
	}
	f.mu.Unlock()

	// One byte more is refused with 552 and never forwarded.
	over := exact[:len(exact)-len(line)] + strings.Repeat("x", 1023) + "\r\n"
	c2 := dialRaw(t, addr)
	c2.expect(250, "MAIL FROM:<>")
	c2.expect(250, bounceRcpt)
	c2.expect(354, "DATA")
	c2.send([]byte(over + ".\r\n"))
	if code := c2.reply(); code != 552 {
		t.Fatalf("a DATA bounce of MaxBytes+1: got %d, want 552", code)
	}
	if f.count() != 1 {
		t.Fatal("an oversized bounce was forwarded")
	}
}

// BDAT then RSET then a second BDAT transaction on the real MX listener: the
// second transaction's final reply must be its own, and the unsynchronized
// session must not race the stale worker (meaningful under -race).
func TestMXBDATResetThenSecondTransaction(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first string
	}{
		{"non-final chunk", "BDAT 10\r\n0123456789"},
		{"zero-length chunk", "BDAT 0\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &recordingForwarder{delay: 100 * time.Microsecond}
			addr := startMX(t, 1<<20, f)
			const iterations = 100
			var c *rawClient
			for i := 0; i < iterations; i++ {
				if i%8 == 0 { // the receiver closes a connection after 20 transactions
					c = dialRaw(t, addr)
				}
				c.expect(250, "MAIL FROM:<>")
				c.expect(250, bounceRcpt)
				c.send([]byte(tc.first))
				if code := c.reply(); code != 250 {
					t.Fatalf("iteration %d: first chunk: %d", i, code)
				}
				c.expect(250, "RSET")
				c.expect(250, "MAIL FROM:<>")
				c.expect(250, bounceRcpt)
				c.send([]byte("BDAT 7 LAST\r\nbody\r\n\n"))
				if code := c.reply(); code != 250 {
					t.Fatalf("iteration %d: the second transaction's final reply is %d, want its own 250", i, code)
				}
			}
			if f.count() != iterations {
				t.Fatalf("forwarded %d bounces, want %d", f.count(), iterations)
			}
		})
	}
}
