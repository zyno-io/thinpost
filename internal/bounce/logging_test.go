package bounce

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/smtp"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type panicForwarder struct{}

func (panicForwarder) Forward(context.Context, string, string, []byte) error {
	panic("victim@example.com")
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestPanicLoggingIsRedacted(t *testing.T) {
	var out lockedBuffer
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	server, err := NewServer(Config{Hostname: "smtp.mx.example", Domains: []string{"bounce.example"}, TLS: &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return nil, errors.New("test certificate unavailable")
	}}, MaxBytes: 1024, MaxConcurrent: 1, ForwardTimeout: time.Second, Forwarder: panicForwarder{}})
	if err != nil {
		t.Fatal(err)
	}
	if server.Debug != nil {
		t.Fatal("SMTP protocol debug output must stay disabled")
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
	defer client.Close()
	begin(t, client)
	writer, err := client.Data()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte("Subject: bounce\r\n\r\nbody\r\n"))
	_ = writer.Close()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "bounce SMTP") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	logged := out.String()
	if !strings.Contains(logged, "bounce SMTP handler panic") {
		t.Fatalf("panic was not logged: %q", logged)
	}
	for _, forbidden := range []string{"victim", "example.com", "127.0.0.1", "goroutine"} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("log leaked %q: %q", forbidden, logged)
		}
	}
}
