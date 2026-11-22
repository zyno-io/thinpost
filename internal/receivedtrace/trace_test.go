package receivedtrace

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

var at = time.Date(2026, 11, 21, 9, 5, 7, 0, time.FixedZone("x", 3600))

func TestFieldSMTP(t *testing.T) {
	got, err := Field("mx.example.net", WithSMTP, "0123456789abcdef0123456789abcdef", at)
	if err != nil {
		t.Fatal(err)
	}
	want := "Received: by mx.example.net (Thinpost)\r\n\twith ESMTPSA id 0123456789abcdef0123456789abcdef;\r\n\tSat, 21 Nov 2026 08:05:07 +0000\r\n"
	if string(got) != want {
		t.Fatalf("%q", got)
	}
}

func TestFieldHTTP(t *testing.T) {
	got, err := Field("mx.example.net", WithHTTP, "abc", at)
	if err != nil || !strings.Contains(string(got), "\r\n\twith HTTP id abc;\r\n") {
		t.Fatalf("%q %v", got, err)
	}
}

func TestFieldFoldingAndNoClientData(t *testing.T) {
	got, _ := Field(SizingHost(), WithSMTP, strings.Repeat("a", 32), at)
	for i, line := range strings.Split(strings.TrimSuffix(string(got), "\r\n"), "\r\n") {
		if len(line) > 998 || strings.ContainsAny(line, "\r\n") || i > 0 && line[0] != '\t' {
			t.Fatalf("line %d: %q", i, line)
		}
	}
	// Only the Thinpost host, the ID and the date: no address and no "from".
	if regexp.MustCompile(`\d+\.\d+\.\d+\.\d+|\[|from `).MatchString(strings.ReplaceAll(string(got), SizingHost(), "")) {
		t.Fatalf("client-looking data in %q", got)
	}
}

func TestFieldRejects(t *testing.T) {
	for _, c := range []struct{ host, with, id string }{
		{"", WithSMTP, "a"}, {"a b", WithSMTP, "a"}, {"a\r\nX: y", WithSMTP, "a"}, {"[192.0.2.1]", WithSMTP, "a"},
		{"mx.example.net", "ESMTP", "a"}, {"mx.example.net", WithSMTP, ""}, {"mx.example.net", WithSMTP, "a b"},
		{"mx.example.net", WithSMTP, "a\r\nb"}, {strings.Repeat("a", 254), WithSMTP, "a"}, {"-a.example", WithSMTP, "a"},
	} {
		if _, err := Field(c.host, c.with, c.id, at); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	if _, err := Field("mx.example.net", WithSMTP, "a", time.Time{}); err == nil {
		t.Fatal("zero time")
	}
	if len(SizingHost()) != MaxHostLen || !validHost(SizingHost()) {
		t.Fatal("sizing host")
	}
}
