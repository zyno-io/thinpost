package submission

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeInReplyTo(t *testing.T) {
	good := map[string]string{
		"<a@b.example>":                   "<a@b.example>",
		" \t<a.b+c@[192.0.2.1]>\t ":       "<a.b+c@[192.0.2.1]>",
		"<20260101.abc_def@mail.example>": "<20260101.abc_def@mail.example>",
	}
	for in, want := range good {
		if got, err := NormalizeInReplyTo(in); err != nil || got != want {
			t.Fatalf("%q: %q %v", in, got, err)
		}
	}
	bad := []string{"", "a@b", "<a@b> <c@d>", "<a@b>\r\n", "<a@b>\n", "<.a@b>", "<a.@b>", "<a..b@c>", "<a@b.>", "<@b>", "<a@>", "<a b@c>",
		"<a@b> (c)", "(c)<a@b>", "<\"q\"@b>", "<a@[x y]>", "<a@[x[y]>", "<a@[x\\y]>", "<a@[]]>", "<a@b@c>", "<é@b>", "<a@é>", "<a@b\x00>",
		"<" + strings.Repeat("a", 990) + "@b>", "<a@b", "a@b>"}
	for _, in := range bad {
		if got, err := NormalizeInReplyTo(in); err == nil {
			t.Fatalf("accepted %q as %q", in, got)
		}
	}
	// The line "In-Reply-To: <id>" must fit 998 bytes.
	max := "<" + strings.Repeat("a", 998-len("In-Reply-To: ")-len("@b>")-1) + "@b>"
	if len(max)+len("In-Reply-To: ") != 998 {
		t.Fatal("test arithmetic", len(max))
	}
	if _, err := NormalizeInReplyTo(max); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeInReplyTo(strings.Replace(max, "@b", "a@b", 1)); err == nil {
		t.Fatal("accepted a line over 998 bytes")
	}
	if err := ValidateInReplyTo(" <a@b>"); err == nil {
		t.Fatal("unnormalized stored value accepted")
	}
}

func TestNormalizeReferences(t *testing.T) {
	if got, err := NormalizeReferences("  <a@b>\t<c@d>   <e@[192.0.2.4]> "); err != nil || got != "<a@b> <c@d> <e@[192.0.2.4]>" {
		t.Fatal(got, err)
	}
	ids := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = "<id" + strings.Repeat("x", i%7) + "@example.com>"
		}
		return strings.Join(parts, " ")
	}
	if _, err := NormalizeReferences(ids(100)); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeReferences(ids(101)); err == nil {
		t.Fatal("101 ids accepted")
	}
	long := "<" + strings.Repeat("a", 980) + "@b.example>"
	if _, err := NormalizeReferences(long + " " + long); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeReferences(strings.Join([]string{long, long, long, long, long, long, long, long, long}, " ")); err == nil {
		t.Fatal("over 8192 bytes accepted")
	}
	// A folded continuation line is " " + id, so an id may be 997 bytes, not 998.
	idOfLen := func(n int) string { return "<" + strings.Repeat("a", n-len("<@b>")) + "@b>" }
	if _, err := NormalizeReferences(idOfLen(997)); err != nil {
		t.Fatal("997-byte id rejected", err)
	}
	if _, err := NormalizeReferences(idOfLen(998)); err == nil {
		t.Fatal("998-byte id accepted; its folded line would exceed 998 bytes")
	}
	for _, in := range []string{"", "   ", "<a@b>\r\n<c@d>", "<a@b>\n<c@d>", "<a@b>,<c@d>", "<a@b><c@d>", "<a@b> junk", "<a@b> (c) <c@d>", "<a@b>\v<c@d>"} {
		if got, err := NormalizeReferences(in); err == nil {
			t.Fatalf("accepted %q as %q", in, got)
		}
	}
	for _, in := range []string{"<a@b>  <c@d>", " <a@b>", "<a@b>\t<c@d>"} {
		if ValidateReferences(in) == nil {
			t.Fatalf("unnormalized stored value %q accepted", in)
		}
	}
}

func TestThreadingFieldsAreOmittedFromLegacyJSON(t *testing.T) {
	data, err := json.Marshal(Message{From: "a@example.com", To: []string{"b@example.net"}})
	if err != nil || strings.Contains(string(data), "InReplyTo") || strings.Contains(string(data), "References") {
		t.Fatal(string(data), err)
	}
	m := Message{From: "a@example.com", To: []string{"b@example.net"}, Subject: "s", Text: "t", InReplyTo: "<a@b>", References: "<x@y> <a@b>"}
	if err = m.Validate(); err != nil {
		t.Fatal(err)
	}
	m.References = "<x@y>  <a@b>"
	if m.Validate() == nil {
		t.Fatal("Validate accepted an unnormalized References")
	}
}
