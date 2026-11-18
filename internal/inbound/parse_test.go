package inbound

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeHelo(t *testing.T) {
	for in, want := range map[string]string{
		"mx.example.org": "mx.example.org",
		"[192.0.2.1]":    "[192.0.2.1]",
		"":               "",
		"mx.ex ample":    "mx.example",
		"mx\r\n.example": "mx.example",
		"mx\x00.example": "mx.example",
		"mäx.example":    "mx.example",
		"\xff\xfe":       "",
		"日本語":            "",
		"UPPER.Example":  "UPPER.Example",
	} {
		if got := SanitizeHelo(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	long := SanitizeHelo(strings.Repeat("a", 1000))
	if len(long) != 255 {
		t.Fatalf("not bounded: %d", len(long))
	}
}

func TestNormalizeEnvelope(t *testing.T) {
	good := Message{ID: strings.Repeat("a", 32), MailFrom: "Sender@Example.ORG", Rcpts: []string{"User@Mail.Test", "user@mail.test", "other@OTHER.test"}, ClientIP: "::ffff:192.0.2.1", Helo: "mx.example.org", TLS: TLSInfo{"TLS 1.3", "TLS_AES_128_GCM_SHA256"}, Receiver: "SMTP.MX.Example"}
	e, err := normalize(good)
	if err != nil {
		t.Fatal(err)
	}
	if e.mailFrom != "Sender@example.org" || len(e.rcpts) != 2 || e.rcpts[0] != "User@mail.test" || strings.Join(e.domains, ",") != "mail.test,other.test" || e.clientIP.String() != "192.0.2.1" || e.receiver != "smtp.mx.example" || e.tlsVer != "TLS 1.3" {
		t.Fatalf("%+v", e)
	}
	if e, err = normalize(Message{ID: good.ID, Rcpts: []string{"u@mail.test"}}); err != nil || e.mailFrom != "" || e.clientIP.IsValid() {
		t.Fatalf("null sender: %+v %v", e, err)
	}
	for name, mutate := range map[string]func(*Message){
		"id":      func(m *Message) { m.ID = "xyz" },
		"no rcpt": func(m *Message) { m.Rcpts = nil },
		"many rcpt": func(m *Message) {
			m.Rcpts = make([]string, 51)
			for i := range m.Rcpts {
				m.Rcpts[i] = "u@mail.test"
			}
		},
		"bad rcpt":   func(m *Message) { m.Rcpts = []string{"no-at-sign"} },
		"space rcpt": func(m *Message) { m.Rcpts = []string{"a b@mail.test"} },
		"utf8 rcpt":  func(m *Message) { m.Rcpts = []string{"ü@mail.test"} },
		"bad from":   func(m *Message) { m.MailFrom = "x" },
		"long from":  func(m *Message) { m.MailFrom = strings.Repeat("a", 320) + "@x.test" },
		"bad ip":     func(m *Message) { m.ClientIP = "999.1.1.1" },
	} {
		m := good
		m.Rcpts = append([]string(nil), good.Rcpts...)
		mutate(&m)
		if _, err := normalize(m); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Unprintable TLS facts and an unprintable HELO are dropped, not stored.
	e, err = normalize(Message{ID: good.ID, Rcpts: []string{"u@mail.test"}, Helo: "a\r\nb", TLS: TLSInfo{"TLS\r\n", strings.Repeat("c", 200)}})
	if err != nil || e.helo != "ab" || e.tlsVer != "" || e.tlsCiph != "" {
		t.Fatalf("%+v %v", e, err)
	}
}

func TestFingerprints(t *testing.T) {
	keys := [][]byte{[]byte("key-one-0123456789abcdef0123456789"), []byte("key-two-0123456789abcdef0123456789")}
	e1, _ := normalize(Message{ID: strings.Repeat("a", 32), MailFrom: "s@example.org", Rcpts: []string{"b@mail.test", "a@mail.test"}})
	e2, _ := normalize(Message{ID: strings.Repeat("b", 32), MailFrom: "S@Example.org", Rcpts: []string{"A@mail.test", "b@mail.test"}})
	base := Fingerprints(keys, "app1", e1, []byte("body"))
	if len(base) != 2 || base[0] == base[1] {
		t.Fatal("one digest per key expected")
	}
	// A retry of the same transaction has the same fingerprint regardless of ID,
	// recipient order and case.
	if got := Fingerprints(keys, "app1", e2, []byte("body")); got != nil && (got[0] != base[0] || got[1] != base[1]) {
		t.Fatal("a retry changed the fingerprint")
	}
	e3, _ := normalize(Message{ID: strings.Repeat("a", 32), MailFrom: "s@example.org", Rcpts: []string{"a@mail.test"}})
	for name, other := range map[string][][32]byte{
		"app":       Fingerprints(keys, "app2", e1, []byte("body")),
		"body":      Fingerprints(keys, "app1", e1, []byte("body2")),
		"recipient": Fingerprints(keys, "app1", e3, []byte("body")),
		"key":       Fingerprints([][]byte{[]byte("key-three-0123456789abcdef012345678")}, "app1", e1, []byte("body")),
	} {
		if other[0] == base[0] {
			t.Errorf("fingerprint does not cover the %s", name)
		}
	}
}

func TestFingerprintKeysAreDerivedAndOrdered(t *testing.T) {
	keys := map[string][]byte{"v1": []byte("0123456789abcdef0123456789abcdef"), "v2": []byte("fedcba9876543210fedcba9876543210")}
	got := FingerprintKeys("v2", keys)
	if len(got) != 2 || string(got[0]) == string(keys["v2"]) || string(got[0]) == string(got[1]) {
		t.Fatal("keys must be derived, active first")
	}
	if again := FingerprintKeys("v2", keys); string(again[0]) != string(got[0]) {
		t.Fatal("derivation is not stable")
	}
	if len(FingerprintKeys("missing", keys)) != 2 {
		t.Fatal("an inactive version dropped")
	}
}

func TestParseHeaders(t *testing.T) {
	raw := "From: =?UTF-8?Q?Zo=C3=AB?= <Zoe@Example.ORG>\r\nSubject: =?UTF-8?B?SGVsbG8g8J+MjQ==?=\r\nMessage-ID: <abc@example.org>\r\nIn-Reply-To: <r1@x>\r\nReferences: <r0@x>\r\n <r1@x>\r\nARC-Seal: i=1\r\n\r\nbody"
	p := Parse([]byte(raw))
	if p.FromAddress != "zoe@example.org" || p.FromCount != 1 || p.Subject != "Hello \U0001F30D" || p.MessageID != "<abc@example.org>" || p.InReplyTo != "<r1@x>" || !strings.Contains(p.References, "<r0@x>") || !p.ARCPresent || p.Unparsable {
		t.Fatalf("%+v", p)
	}
	if !utf8.ValidString(p.Subject) {
		t.Fatal("invalid subject")
	}
}

func TestParseKeepsDatabaseUnsafeValuesOut(t *testing.T) {
	raw := "From: a@example.org\r\nSubject: bad\x00sub\x01ject \xff\r\nMessage-ID: <" + strings.Repeat("a", 2000) + "@x>\r\n\r\nbody"
	p := Parse([]byte(raw))
	if strings.ContainsAny(p.Subject, "\x00\x01") || !utf8.ValidString(p.Subject) || len(p.MessageID) > 998 {
		t.Fatalf("%q %d", p.Subject, len(p.MessageID))
	}
	long := "From: a@example.org\r\nSubject: " + strings.Repeat("é", 2000) + "\r\n\r\nx"
	if p = Parse([]byte(long)); len(p.Subject) > 1024 || !utf8.ValidString(p.Subject) {
		t.Fatalf("subject not bounded on a rune boundary: %d", len(p.Subject))
	}
}

func TestParseBounds(t *testing.T) {
	// A header block that never ends within 64 KiB is not parsed.
	huge := "Subject: " + strings.Repeat("a", 70<<10) + "\r\n\r\nbody"
	if p := Parse([]byte(huge)); !p.Unparsable || p.Subject != "" {
		t.Fatalf("oversize header block parsed: %+v", p)
	}
	if p := Parse([]byte("no headers at all, just text")); p.FromAddress != "" {
		t.Fatal("headers invented")
	}
	if p := Parse(nil); !p.Unparsable {
		t.Fatal("empty message")
	}
	// Nesting deeper than five levels truncates the walk, not the message.
	var nested strings.Builder
	nested.WriteString("From: a@example.org\r\nContent-Type: multipart/mixed; boundary=b0\r\n\r\n")
	for i := 0; i < 9; i++ {
		nested.WriteString("--b" + string(rune('0'+i)) + "\r\nContent-Type: multipart/mixed; boundary=b" + string(rune('1'+i)) + "\r\n\r\n")
	}
	nested.WriteString("--b9\r\nContent-Type: text/plain\r\n\r\nleaf\r\n--b9--\r\n")
	for i := 8; i >= 0; i-- {
		nested.WriteString("--b" + string(rune('0'+i)) + "--\r\n")
	}
	p := Parse([]byte(nested.String()))
	if !p.Truncated || p.Depth > maxDepth+1 || p.FromAddress != "a@example.org" {
		t.Fatalf("depth: %+v", p)
	}
	// More than 100 parts truncates too.
	var many strings.Builder
	many.WriteString("From: a@example.org\r\nContent-Type: multipart/mixed; boundary=zz\r\n\r\n")
	for i := 0; i < 150; i++ {
		many.WriteString("--zz\r\nContent-Type: text/plain\r\n\r\nx\r\n")
	}
	many.WriteString("--zz--\r\n")
	if p = Parse([]byte(many.String())); !p.Truncated || p.Parts > maxParts+1 {
		t.Fatalf("parts: %+v", p)
	}
}

func TestParseClassification(t *testing.T) {
	dsn := "From: mailer-daemon@example.org\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=q\r\n\r\n--q\r\nContent-Type: text/plain\r\n\r\nx\r\n--q--\r\n"
	if p := Parse([]byte(dsn)); !p.DeliveryStatusReport || p.FeedbackReport {
		t.Fatalf("%+v", p)
	}
	arf := strings.Replace(dsn, "delivery-status", "feedback-report", 1)
	if p := Parse([]byte(arf)); !p.FeedbackReport || p.DeliveryStatusReport {
		t.Fatalf("%+v", p)
	}
}

func TestParseMultipleFromFields(t *testing.T) {
	if p := Parse([]byte("From: a@example.org\r\nFrom: b@example.org\r\n\r\nx")); p.FromCount != 2 || p.FromAddress != "" {
		t.Fatalf("an ambiguous From was trusted: %+v", p)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(plainMessage))
	f.Add([]byte("From: a@b.c\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: message/rfc822\r\n\r\nSubject: s\r\n\r\n--x--\r\n"))
	f.Add([]byte("Subject: =?UTF-8?B?////?=\r\nFrom: <>\r\nContent-Transfer-Encoding: base64\r\n\r\n!!!!"))
	f.Add([]byte("References: <a@b> <c@d>\r\n\r\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		p := Parse(raw)
		if !utf8.ValidString(p.Subject) || strings.ContainsRune(p.Subject, 0) || len(p.Subject) > 1024 || len(p.MessageID) > 998 || len(p.FromAddress) > 320 || strings.ContainsRune(p.FromAddress, 0) || strings.ContainsRune(p.MessageID, 0) {
			t.Fatalf("unsafe parse result %+v", p)
		}
		if p.Parts > maxParts+1 || p.Depth > maxDepth+1 {
			t.Fatalf("bounds exceeded %+v", p)
		}
		_ = scan(p)
	})
}
