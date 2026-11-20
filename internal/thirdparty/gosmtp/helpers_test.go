package smtp

import (
	"bufio"
	"net"
	"net/textproto"
	"testing"
	"time"
)

// pipeClient drives a Server over an in-memory connection (reserved domains
// only in fixtures: example.com, example.net, example.org).
type pipeClient struct {
	t    *testing.T
	conn net.Conn
	r    *textproto.Reader
	done chan error
}

func dialPipe(t *testing.T, s *Server) *pipeClient {
	t.Helper()
	cc, sc := net.Pipe()
	p := &pipeClient{t: t, conn: cc, r: textproto.NewReader(bufio.NewReader(cc)), done: make(chan error, 1)}
	go func() { p.done <- s.handleConn(newConn(sc, s)) }()
	_ = cc.SetDeadline(time.Now().Add(30 * time.Second))
	if code, _ := p.read(); code != 220 {
		t.Fatalf("greeting: %d", code)
	}
	return p
}

func (p *pipeClient) read() (int, string) {
	p.t.Helper()
	code, msg, err := p.r.ReadResponse(0)
	if err != nil {
		p.t.Fatalf("read reply: %v", err)
	}
	return code, msg
}

func (p *pipeClient) send(s string) {
	p.t.Helper()
	if _, err := p.conn.Write([]byte(s)); err != nil {
		p.t.Fatalf("write: %v", err)
	}
}

// cmd sends one line and returns the reply code.
func (p *pipeClient) cmd(line string) int {
	p.t.Helper()
	p.send(line + "\r\n")
	code, _ := p.read()
	return code
}

func (p *pipeClient) expect(want int, line string) {
	p.t.Helper()
	if got := p.cmd(line); got != want {
		p.t.Fatalf("%q: got %d, want %d", line, got, want)
	}
}

func (p *pipeClient) close() {
	p.conn.Close()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		p.t.Fatal("server connection did not finish")
	}
}
