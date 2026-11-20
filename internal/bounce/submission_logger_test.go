package bounce

import (
	"strings"
	"testing"
	"time"
)

func TestSubmissionLoggerAggregatesConnectionErrors(t *testing.T) {
	var lines []string
	stats := &subStats{}
	now := time.Now()
	l := newSubmissionLogger(stats, func(format string, args ...any) { lines = append(lines, strings.TrimSpace(format)) })
	l.now = func() time.Time { return now }
	for i := 0; i < 50; i++ {
		l.Printf("error handling %v: %s", "198.51.100.7:1234", "tls: handshake failure")
	}
	if len(lines) != 1 || strings.Contains(lines[0], "198.51") {
		t.Fatalf("lines: %v", lines)
	}
	if stats.get("connection_error") != 50 {
		t.Fatal("errors must all be counted")
	}
	now = now.Add(2 * time.Minute)
	l.Println("x")
	if len(lines) != 2 || !strings.Contains(lines[1], "more since") {
		t.Fatalf("summary line: %v", lines)
	}
	l.Printf("panic serving %v: %v\n%s", "a", "victim@example.com", "stack")
	if lines[len(lines)-1] != "submission SMTP handler panic" {
		t.Fatal("a panic is logged at once")
	}
}
