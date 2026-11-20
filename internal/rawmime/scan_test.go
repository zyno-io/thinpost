package rawmime

import (
	"errors"
	"strings"
	"testing"
)

func TestScanBounds(t *testing.T) {
	long := strings.Repeat("a", 998)
	fields := func(n int) string {
		return strings.Repeat("X-A: b\r\n", n) + "\r\nbody"
	}
	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"empty", "", ErrInvalid},
		{"crlf", "From: a@example.com\r\n\r\nbody\r\n", nil},
		{"lf only", "From: a@example.com\n\nbody\n", nil},
		{"mixed endings", "From: a@example.com\r\nTo: b@example.net\n\r\nbody", nil},
		{"all headers no blank line", "From: a@example.com\r\nSubject: x", nil},
		{"all headers trailing crlf", "From: a@example.com\r\n", nil},
		{"folded header", "Subject: one\r\n two\r\n\tthree\r\n\r\nx", nil},
		{"leading continuation", " Subject: one\r\n\r\nx", ErrInvalid},
		{"no colon", "Subject one\r\n\r\nx", ErrInvalid},
		{"empty name", ": value\r\n\r\nx", ErrInvalid},
		{"space in name", "Sub ject: x\r\n\r\nx", ErrInvalid},
		{"trailing ws before colon", "Subject : x\r\n\r\nx", nil},
		{"non-ascii header", "Subject: caf\xc3\xa9\r\n\r\nx", ErrContentNotSupported},
		{"nul header", "Subject: a\x00b\r\n\r\nx", ErrContentNotSupported},
		{"bare cr header", "Subject: a\rb\r\n\r\nx", ErrContentNotSupported},
		{"200 fields", fields(200), nil},
		{"201 fields", fields(201), ErrTooLarge},
		{"header 998 line", "X-A: " + long[:993] + "\r\n\r\nx", nil},
		{"header 999 line", "X-A: " + long[:994] + "\r\n\r\nx", ErrContentNotSupported},
		{"body 8bit", "A: b\r\n\r\ncaf\xc3\xa9 \xff\r\n", nil},
		{"body bare lf", "A: b\r\n\r\none\ntwo\r\n", nil},
		{"body bare cr", "A: b\r\n\r\none\rtwo\r\n", ErrContentNotSupported},
		{"body cr at end", "A: b\r\n\r\none\r", ErrContentNotSupported},
		{"body cr cr lf", "A: b\r\n\r\none\r\r\n", ErrContentNotSupported},
		{"body nul", "A: b\r\n\r\none\x00", ErrContentNotSupported},
		{"body 998 line", "A: b\r\n\r\n" + long + "\r\n", nil},
		{"body 999 line", "A: b\r\n\r\n" + long + "a\r\n", ErrContentNotSupported},
		{"body 999 line bare lf", "A: b\r\n\r\n" + long + "a\n", ErrContentNotSupported},
		{"body 999 line no ending", "A: b\r\n\r\n" + long + "a", ErrContentNotSupported},
		{"whitespace-only continuation", "Subject: a\r\n  \t\r\n b\r\n\r\nx", ErrInvalid},
		{"whitespace-only first line", "   \r\nFrom: a@example.com\r\n\r\nx", ErrInvalid},
		{"empty header block", "\r\nbody", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Scan([]byte(c.raw))
			if !errors.Is(err, c.want) && (c.want != nil || err != nil) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestScanSizeBounds(t *testing.T) {
	head := "From: a@example.com\r\n\r\n"
	body := func(n int) string {
		var b strings.Builder
		for b.Len() < n {
			b.WriteString(strings.Repeat("x", 76) + "\r\n")
		}
		return b.String()[:n]
	}
	if _, err := Scan([]byte(head + body(MaxRawBytes-len(head)))); err != nil {
		t.Fatalf("at limit: %v", err)
	}
	if _, err := Scan([]byte(head + body(MaxRawBytes-len(head)+1))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over limit: %v", err)
	}
}

func TestScanHeaderBlockLimit(t *testing.T) {
	line := " " + strings.Repeat("a", 70) + "\r\n" // a continuation line of 73 bytes
	var b strings.Builder
	b.WriteString("X-Filler: x\r\n")
	for b.Len()+len(line) <= MaxHeaderBlock {
		b.WriteString(line)
	}
	atLimit := b.String()
	if _, err := Scan([]byte(atLimit + "\r\nbody")); err != nil {
		t.Fatalf("under limit: %v", err)
	}
	if _, err := Scan([]byte(atLimit + line + "\r\nbody")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over limit: %v", err)
	}
	// No blank line at all and over the limit, with a large "body" behind it.
	if _, err := Scan([]byte(strings.Repeat(line, 2000))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("all headers over limit: %v", err)
	}
}

func TestScanFacts(t *testing.T) {
	s, err := Scan(fixture(t, "basic.eml"))
	if err != nil {
		t.Fatal(err)
	}
	if s.FromFields != 1 || s.From == nil || s.From.Address != "alice@example.com" || s.From.Name != "Alice Example" {
		t.Fatalf("from: %+v", s.From)
	}
	if s.Subject != "Hello world" {
		t.Fatalf("subject %q", s.Subject)
	}
	if !s.HasMessageID || s.MessageID != "<orig@example.com>" || !s.HasDate || !s.Enhanced || s.RequireTLS {
		t.Fatalf("facts: %+v", s)
	}
	for _, k := range []string{"bob@example.net", "carol@example.org"} {
		if _, ok := s.To[k]; !ok {
			t.Errorf("To missing %s: %v", k, s.To)
		}
	}
	if _, ok := s.Cc["dan@example.net"]; !ok {
		t.Errorf("Cc: %v", s.Cc)
	}
}

func TestScanFactEdges(t *testing.T) {
	cases := []struct {
		name  string
		hdr   string
		check func(*Scanned) bool
	}{
		{"no date", "From: a@example.com\r\n", func(s *Scanned) bool { return !s.HasDate }},
		{"date", "Date: Tue, 17 Nov 2026 10:00:00 +0000\r\n", func(s *Scanned) bool { return s.HasDate }},
		{"bad message-id", "Message-ID: not an id\r\n", func(s *Scanned) bool { return !s.HasMessageID }},
		{"message-id spaces trimmed", "Message-ID:   <a@example.com> \r\n", func(s *Scanned) bool { return s.HasMessageID && s.MessageID == "<a@example.com>" }},
		{"first message-id only", "Message-ID: bad\r\nMessage-ID: <a@example.com>\r\n", func(s *Scanned) bool { return !s.HasMessageID }},
		{"require tls", "X-Thinpost-Require-TLS: true\r\n", func(s *Scanned) bool { return s.RequireTLS }},
		{"enhanced false", "X-Thinpost-Enhanced-Protection: false\r\n", func(s *Scanned) bool { return !s.Enhanced }},
		{"enhanced case", "X-Thinpost-Enhanced-Protection: TRUE\r\n", func(s *Scanned) bool { return s.Enhanced }},
		{"no hold", "From: a@example.com\r\n", func(s *Scanned) bool { return !s.Hold }},
		{"hold", "x-thinpost-hold: True\r\n", func(s *Scanned) bool { return s.Hold }},
		{"hold false", "X-Thinpost-Hold: false\r\n", func(s *Scanned) bool { return !s.Hold }},
		{"two froms", "From: a@example.com\r\nFrom: b@example.com\r\n", func(s *Scanned) bool { return s.FromFields == 2 && s.From == nil }},
		{"two mailboxes", "From: a@example.com, b@example.com\r\n", func(s *Scanned) bool { return s.FromFields == 1 && s.From == nil }},
		{"group", "From: team: a@example.com;\r\n", func(s *Scanned) bool { return s.From == nil }},
		{"folded from", "From: Alice\r\n <alice@example.com>\r\n", func(s *Scanned) bool { return s.From != nil && s.From.Address == "alice@example.com" }},
		{"sender", "Sender: Bot <Bot@Example.COM>\r\n", func(s *Scanned) bool { return s.SenderPresent && s.SenderDomain == "example.com" }},
		{"bad sender", "Sender: nonsense\r\n", func(s *Scanned) bool { return s.SenderPresent && s.SenderDomain == "" }},
		{"to case", "To: Alice@Example.COM\r\n", func(s *Scanned) bool { _, ok := s.To["Alice@example.com"]; return ok }},
		{"2047 subject", "Subject: =?ISO-8859-1?Q?caf=E9?=\r\n", func(s *Scanned) bool { return s.Subject == "café" }},
		{"long subject cut", "Subject: " + strings.Repeat("a", 500) + "\r\n " + strings.Repeat("b", 500) + "\r\n", func(s *Scanned) bool { return len(s.Subject) == 998 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := mustScan(t, c.hdr+"\r\nbody")
			if !c.check(s) {
				t.Fatalf("%+v", s)
			}
		})
	}
	for _, bad := range []string{
		"X-Thinpost-Enhanced-Protection: maybe\r\n",
		"X-Thinpost-Enhanced-Protection: true\r\nX-Thinpost-Enhanced-Protection: true\r\n",
		"X-Thinpost-Hold: yes\r\n",
		"X-Thinpost-Hold: true\r\nX-Thinpost-Hold: false\r\n",
	} {
		if _, err := Scan([]byte(bad + "\r\n")); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestScanBareLFCount(t *testing.T) {
	s := mustScan(t, "A: b\r\n\r\none\ntwo\r\nthree\n\n")
	if s.BareLF != 3 {
		t.Fatalf("BareLF %d", s.BareLF)
	}
	if s.HeaderEnd != 6 || s.BodyStart != 8 {
		t.Fatalf("offsets %d %d", s.HeaderEnd, s.BodyStart)
	}
}

func TestErrorsDoNotQuoteInput(t *testing.T) {
	_, err := Scan([]byte("Subject: SECRET-\xff-VALUE\r\n\r\nx"))
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("%v", err)
	}
}
