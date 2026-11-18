package messageid

import (
	"reflect"
	"strings"
	"testing"
)

const hexID = "0123456789abcdef0123456789abcdef"

func TestParseThinpost(t *testing.T) {
	id, domain, ok := ParseThinpost("<" + hexID + "@Mail.Example.COM>")
	if !ok || id != hexID || domain != "mail.example.com" {
		t.Fatalf("%q %q %v", id, domain, ok)
	}
	for _, bad := range []string{
		"", hexID + "@example.com", "<" + hexID + ">", "<" + strings.ToUpper(hexID) + "@example.com>",
		"<" + hexID[:31] + "@example.com>", "<" + hexID + "0@example.com>", "<" + hexID + "@>", "<" + hexID + "@exa mple.com>",
		"<" + hexID + "@example.com> <" + hexID + "@example.com>", "<" + hexID + "@a@b>", " <" + hexID + "@example.com>",
	} {
		if _, _, ok := ParseThinpost(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestExtract(t *testing.T) {
	got := Extract("<a@x> (comment <ignored>) junk <b@y>\r\n\t<c@z>", 0)
	want := []string{"<a@x>", "<ignored>", "<b@y>", "<c@z>"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
	if got = Extract("<unterminated <real@x>", 0); !reflect.DeepEqual(got, []string{"<real@x>"}) {
		t.Fatalf("%q", got)
	}
	if got = Extract("<a b@x> <ok@x>", 0); !reflect.DeepEqual(got, []string{"<ok@x>"}) {
		t.Fatalf("%q", got)
	}
	if got = Extract(strings.Repeat("<a@x>", 100), 0); len(got) != MaxScanned {
		t.Fatalf("not bounded: %d", len(got))
	}
	if got = Extract(strings.Repeat("<a@x>", 100), 3); len(got) != 3 {
		t.Fatalf("limit: %d", len(got))
	}
	if got = Extract("<"+strings.Repeat("a", 999)+">", 0); len(got) != 0 {
		t.Fatal("over-long identifier accepted")
	}
	if got = Extract("no brackets", 0); got != nil {
		t.Fatal("brackets invented")
	}
}
