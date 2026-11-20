package smtp

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

type limitBackend struct{ got chan limitResult }

type limitResult struct {
	body []byte
	err  error
}

type limitSession struct{ b *limitBackend }

func (b *limitBackend) NewSession(*Conn) (Session, error) { return &limitSession{b}, nil }
func (s *limitSession) Reset()                            {}
func (s *limitSession) Logout() error                     { return nil }
func (s *limitSession) Mail(string, *MailOptions) error   { return nil }
func (s *limitSession) Rcpt(string, *RcptOptions) error   { return nil }
func (s *limitSession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	s.b.got <- limitResult{body, err}
	return err
}

const testLimit = 64

func dataTest(t *testing.T, payload string, closeAfter bool) (int, limitResult, *pipeClient) {
	t.Helper()
	be := &limitBackend{got: make(chan limitResult, 1)}
	s := NewServer(be)
	s.AllowInsecureAuth = true
	s.MaxMessageBytes = testLimit
	p := dialPipe(t, s)
	p.expect(250, "EHLO client.example.net")
	p.expect(250, "MAIL FROM:<a@example.net>")
	p.expect(250, "RCPT TO:<b@example.org>")
	p.expect(354, "DATA")
	p.send(payload)
	if closeAfter {
		p.conn.Close()
		res := <-be.got
		<-p.done
		return 0, res, nil
	}
	code, _ := p.read()
	return code, <-be.got, p
}

func TestDataExactLimitAccepted(t *testing.T) {
	// 62 bytes + CRLF = 64 decoded bytes.
	body := strings.Repeat("a", testLimit-2) + "\r\n"
	code, res, p := dataTest(t, body+".\r\n", false)
	defer p.close()
	if code != 250 || res.err != nil || string(res.body) != body {
		t.Fatalf("code=%d err=%v len=%d", code, res.err, len(res.body))
	}
}

func TestDataExactLimitDotStuffed(t *testing.T) {
	// Wire form has a stuffed dot; decoded it is exactly 64 bytes.
	decoded := "." + strings.Repeat("b", testLimit-3) + "\r\n"
	wire := "." + decoded
	code, res, p := dataTest(t, wire+".\r\n", false)
	defer p.close()
	if code != 250 || res.err != nil || string(res.body) != decoded {
		t.Fatalf("code=%d err=%v len=%d", code, res.err, len(res.body))
	}
}

func TestDataOverLimitRejected(t *testing.T) {
	body := strings.Repeat("a", testLimit-1) + "\r\n" // 65 bytes
	code, res, p := dataTest(t, body+".\r\n", false)
	defer p.close()
	if code != 552 || !errors.Is(res.err, ErrDataTooLarge) {
		t.Fatalf("code=%d err=%v", code, res.err)
	}
}

func TestDataOverLimitByDotStuffedByte(t *testing.T) {
	// Decodes to 65 bytes only because the dot is not counted twice.
	decoded := "." + strings.Repeat("b", testLimit-2) + "\r\n"
	code, res, p := dataTest(t, "."+decoded+".\r\n", false)
	defer p.close()
	if code != 552 || !errors.Is(res.err, ErrDataTooLarge) {
		t.Fatalf("code=%d err=%v", code, res.err)
	}
}

func TestDataTruncatedAtLimit(t *testing.T) {
	// Exactly the limit of data arrives, then the client disconnects before the
	// terminator: still an unexpected EOF, not success and not "too large".
	body := strings.Repeat("a", testLimit-2) + "\r\n"
	_, res, _ := dataTest(t, body, true)
	if !errors.Is(res.err, io.ErrUnexpectedEOF) {
		t.Fatalf("err=%v", res.err)
	}
}

func TestDataTruncatedBelowLimit(t *testing.T) {
	_, res, _ := dataTest(t, "short\r\n", true)
	if !errors.Is(res.err, io.ErrUnexpectedEOF) {
		t.Fatalf("err=%v", res.err)
	}
}

func TestDataPartialTerminatorAtLimit(t *testing.T) {
	// At the limit, a partial terminator followed by EOF is a truncation.
	body := strings.Repeat("a", testLimit-2) + "\r\n.\r"
	_, res, _ := dataTest(t, body, true)
	if !errors.Is(res.err, io.ErrUnexpectedEOF) {
		t.Fatalf("err=%v", res.err)
	}
}

// TestDataTooLargeIsSticky reads past the limit one call at a time and keeps
// going after the error: every later Read must still fail, never reach a clean
// EOF that would let a retrying reader accept a truncated message.
func TestDataTooLargeIsSticky(t *testing.T) {
	r := &dataReader{
		r:       bufio.NewReader(strings.NewReader("abcdef\r\n.\r\n")),
		limited: true,
		n:       4,
	}
	buf := make([]byte, 16)
	n, err := r.Read(buf)
	if n != 4 || err != nil || string(buf[:n]) != "abcd" {
		t.Fatalf("first read = %d %q %v", n, buf[:n], err)
	}
	for i := 0; i < 20; i++ {
		n, err = r.Read(buf)
		if n != 0 || !errors.Is(err, ErrDataTooLarge) {
			t.Fatalf("read %d after limit = %d, %v; want 0, ErrDataTooLarge", i, n, err)
		}
	}
}
