package smtptransport

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func scriptedSMTP(conn net.Conn, greeting, hello, final time.Duration, stopReadingData bool) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(6 * time.Second))
	time.Sleep(greeting)
	if _, err := fmt.Fprint(conn, "220 ready\r\n"); err != nil {
		return
	}
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		return
	}
	time.Sleep(hello)
	if _, err := fmt.Fprint(conn, "250 mx.example\r\n"); err != nil {
		return
	}
	for _, response := range []string{"250 sender\r\n", "250 recipient\r\n", "354 send\r\n"} {
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		if _, err := fmt.Fprint(conn, response); err != nil {
			return
		}
	}
	if stopReadingData {
		time.Sleep(1500 * time.Millisecond)
		return
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if line == ".\r\n" {
			break
		}
	}
	time.Sleep(final)
	if _, err := fmt.Fprint(conn, "250 accepted\r\n"); err != nil {
		return
	}
	if _, err := reader.ReadString('\n'); err != nil {
		return
	}
	_, _ = fmt.Fprint(conn, "221 bye\r\n")
}

func TestStageTimeoutsAndAmbiguity(t *testing.T) {
	policy := Timeouts{Connect: time.Second, Greeting: time.Second, Command: time.Second, DataInit: time.Second, DataBlock: time.Second, DataFinal: time.Second}
	for _, test := range []struct {
		name                   string
		greeting, hello, final time.Duration
		block                  bool
		body                   string
		want                   Outcome
		wantError              bool
	}{
		{"each stage gets its own time", 600 * time.Millisecond, 600 * time.Millisecond, 100 * time.Millisecond, false, "body\r\n", Accepted, false},
		{"greeting timeout is safe to retry", 1200 * time.Millisecond, 0, 0, false, "body\r\n", NotAccepted, true},
		{"blocked body is safe to retry", 0, 0, 0, true, strings.Repeat("a", 128<<10), NotAccepted, true},
		{"final response timeout is uncertain", 0, 0, 1200 * time.Millisecond, false, "body\r\n", Uncertain, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			done := make(chan struct{})
			go func() { defer close(done); scriptedSMTP(server, test.greeting, test.hello, test.final, test.block) }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := Send(ctx, client, Request{MXName: "mx.example", HeloName: "smtp.example", EnvelopeFrom: "sender@example.com", Recipient: "recipient@example.net", MIME: strings.NewReader(test.body), Timeouts: &policy})
			if result != test.want || (err != nil) != test.wantError {
				t.Fatalf("outcome %v, error %v", result, err)
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("SMTP fixture did not exit")
			}
		})
	}
}

func TestStageDeadlineRespectsContextAndValidation(t *testing.T) {
	policy := DefaultTimeouts()
	policy.DataFinal = 0
	client, server := net.Pipe()
	defer server.Close()
	if result, err := Send(context.Background(), client, Request{MXName: "mx.example", HeloName: "smtp.example", EnvelopeFrom: "sender@example.com", Recipient: "recipient@example.net", MIME: strings.NewReader("body"), Timeouts: &policy}); err == nil || result != NotAccepted {
		t.Fatal("invalid stage timeout accepted", err)
	}
	client, server = net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if result, err := Send(ctx, client, Request{MXName: "mx.example", HeloName: "smtp.example", EnvelopeFrom: "sender@example.com", Recipient: "recipient@example.net", MIME: strings.NewReader("body")}); err == nil || result != NotAccepted || time.Since(started) > time.Second {
		t.Fatal("context deadline ignored", result, err)
	}
}
