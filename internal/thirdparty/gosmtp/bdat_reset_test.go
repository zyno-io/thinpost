package smtp

import (
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

var errFirstTxn = &SMTPError{Code: 554, EnhancedCode: EnhancedCode{5, 0, 0}, Message: "first transaction result"}

// bdatBackend's first Data call per transaction chain is slowed and returns an
// error; later calls succeed. txn is deliberately unsynchronized, like the MX
// session: Reset and Data must never run concurrently.
type bdatBackend struct{ datas atomic.Int64 }

type bdatSession struct {
	b   *bdatBackend
	txn string
}

func (b *bdatBackend) NewSession(*Conn) (Session, error) { return &bdatSession{b: b}, nil }
func (s *bdatSession) Reset()                            { s.txn = "" }
func (s *bdatSession) Logout() error                     { return nil }
func (s *bdatSession) Mail(from string, _ *MailOptions) error {
	s.txn = from
	return nil
}
func (s *bdatSession) Rcpt(string, *RcptOptions) error { return nil }
func (s *bdatSession) Data(r io.Reader) error {
	_ = s.txn // read the unsynchronized field
	first := s.b.datas.Add(1)%2 == 1
	if first {
		time.Sleep(50 * time.Microsecond)
	}
	_, _ = io.Copy(io.Discard, r)
	_ = s.txn
	if first {
		return errFirstTxn
	}
	return nil
}

func settleGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutine leak: %d > %d\n%s", runtime.NumGoroutine(), base, buf[:n])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func runBDATReset(t *testing.T, zeroFirst bool) {
	be := &bdatBackend{}
	s := NewServer(be)
	s.Domain = "mx.example.com"
	s.AllowInsecureAuth = true
	base := runtime.NumGoroutine()

	const iterations = 1000
	const perConn = 100
	for done := 0; done < iterations; {
		p := dialPipe(t, s)
		p.expect(250, "EHLO client.example.net")
		for i := 0; i < perConn && done < iterations; i, done = i+1, done+1 {
			p.expect(250, "MAIL FROM:<a@example.net>")
			p.expect(250, "RCPT TO:<b@example.org>")
			if zeroFirst {
				p.expect(250, "BDAT 0")
			} else {
				p.send("BDAT 10\r\n0123456789")
				if code, _ := p.read(); code != 250 {
					t.Fatalf("iteration %d: BDAT 10: %d", done, code)
				}
			}
			p.expect(250, "RSET")

			p.expect(250, "MAIL FROM:<c@example.net>")
			p.expect(250, "RCPT TO:<d@example.org>")
			p.send("BDAT 5 LAST\r\nhello")
			code, msg := p.read()
			if code != 250 {
				t.Fatalf("iteration %d: second transaction's final reply is %d %q, want its own 250", done, code, msg)
			}
		}
		p.close()
	}
	settleGoroutines(t, base)
	if n := be.datas.Load(); n != 2*iterations {
		t.Fatalf("Data calls: %d, want %d", n, 2*iterations)
	}
}

func TestBDATResetIsolatesWorker(t *testing.T) {
	runBDATReset(t, false)
}

func TestBDATZeroLengthChunkThenReset(t *testing.T) {
	runBDATReset(t, true)
}
