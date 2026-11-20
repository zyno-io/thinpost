package smtp

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// chunkBackend counts sessions and commands so a test can tell whether bytes
// after a refused BDAT were ever interpreted as commands.
type chunkBackend struct{ sessions, mails, datas atomic.Int64 }

type chunkSession struct{ b *chunkBackend }

func (b *chunkBackend) NewSession(*Conn) (Session, error) {
	b.sessions.Add(1)
	return &chunkSession{b: b}, nil
}
func (s *chunkSession) Reset()        {}
func (s *chunkSession) Logout() error { return nil }
func (s *chunkSession) Mail(string, *MailOptions) error {
	s.b.mails.Add(1)
	return nil
}
func (s *chunkSession) Rcpt(string, *RcptOptions) error { return nil }
func (s *chunkSession) Data(r io.Reader) error {
	s.b.datas.Add(1)
	_, _ = io.Copy(io.Discard, r)
	return nil
}

func ehloCaps(t *testing.T, p *pipeClient) string {
	t.Helper()
	p.send("EHLO client.example.com\r\n")
	code, msg := p.read()
	if code != 250 {
		t.Fatalf("EHLO: %d", code)
	}
	return msg
}

func TestDisableCHUNKINGOmitsCapability(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		s := NewServer(&chunkBackend{})
		s.DisableCHUNKING = disabled
		p := dialPipe(t, s)
		caps := ehloCaps(t, p)
		if has := strings.Contains(caps, "CHUNKING"); has == disabled {
			t.Fatalf("DisableCHUNKING=%v but CHUNKING advertised=%v: %q", disabled, has, caps)
		}
		p.close()
	}
}

func TestDisableCHUNKINGPipelinedBDATGetsOne502ThenClose(t *testing.T) {
	be := &chunkBackend{}
	s := NewServer(be)
	s.DisableCHUNKING = true
	p := dialPipe(t, s)
	ehloCaps(t, p)
	p.expect(250, "MAIL FROM:<a@example.com>")
	be.mails.Store(0)
	// The second BDAT and the payloads contain commands that must never run.
	payload := "RSET\r\nMAIL FROM:<a@example.com>\r\n"
	p.send("BDAT 10\r\n" + payload[:10] + "BDAT 5 LAST\r\n" + payload[10:15])
	if code, msg := p.read(); code != 502 || !strings.Contains(msg, "BDAT not supported") {
		t.Fatalf("got %d %q, want 502", code, msg)
	}
	if _, _, err := p.r.ReadResponse(0); err == nil {
		t.Fatal("a second reply followed the 502")
	}
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("connection stayed open after the 502")
	}
	if got := be.mails.Load(); got != 0 {
		t.Fatalf("payload command ran: %d Mail calls", got)
	}
}

// Without the `if c.terminate { return nil }` loop exit, the buffered EHLO in
// the payload would be dispatched after Close cleared the session and create a
// second one. The payload arrives in the same Write as the BDAT command, so it
// is guaranteed to sit in the server's buffered reader.
func TestDisableCHUNKINGBufferedPayloadCreatesOneSession(t *testing.T) {
	be := &chunkBackend{}
	s := NewServer(be)
	s.DisableCHUNKING = true
	p := dialPipe(t, s)
	ehloCaps(t, p)
	p.send("BDAT 10\r\nEHLO x.example.com\r\nMAIL FROM:<a@example.com>\r\n")
	if code, _ := p.read(); code != 502 {
		t.Fatalf("got %d, want 502", code)
	}
	if _, _, err := p.r.ReadResponse(0); err == nil {
		t.Fatal("a reply followed the 502")
	}
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("handleConn did not return")
	}
	if got := be.sessions.Load(); got != 1 {
		t.Fatalf("NewSession called %d times, want exactly 1", got)
	}
	if got := be.mails.Load(); got != 0 {
		t.Fatalf("payload command ran: %d Mail calls", got)
	}
}

func TestChunkingEnabledStillAcceptsBDAT(t *testing.T) {
	be := &chunkBackend{}
	p := dialPipe(t, NewServer(be))
	ehloCaps(t, p)
	p.expect(250, "MAIL FROM:<a@example.com>")
	p.expect(250, "RCPT TO:<b@example.net>")
	p.send("BDAT 5 LAST\r\nhello")
	if code, _ := p.read(); code != 250 {
		t.Fatalf("BDAT: %d", code)
	}
	p.close()
	if be.datas.Load() != 1 {
		t.Fatal("BDAT did not reach the backend")
	}
}
