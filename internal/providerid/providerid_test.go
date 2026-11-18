package providerid

import "testing"

const hexID = "0123456789abcdef0123456789abcdef"
const uuidID = "01234567-89ab-cdef-0123-456789abcdef"

func TestRenderings(t *testing.T) {
	for name, tc := range map[string][2]string{
		"postmark":        {Postmark(hexID), uuidID},
		"resend":          {Resend(hexID), uuidID},
		"mailgun header":  {MailgunHeader(hexID, "example.com"), hexID + "@example.com"},
		"mailgun reply":   {MailgunResponse(hexID, "example.com"), "<" + hexID + "@example.com>"},
		"mailgun nodom":   {MailgunHeader(hexID, ""), hexID},
		"postmark opaque": {Postmark("fixture-1"), "fixture-1"},
	} {
		if tc[0] != tc[1] {
			t.Errorf("%s: got %q want %q", name, tc[0], tc[1])
		}
	}
}

func TestMessageIDHeader(t *testing.T) {
	if got := MessageIDHeader(hexID, "Example.COM"); got != "<"+hexID+"@example.com>" {
		t.Fatalf("got %q", got)
	}
	if MailgunResponse(hexID, "example.com") != MessageIDHeader(hexID, "example.com") {
		t.Fatal("the Mailgun response id is not the Message-ID header")
	}
	if MailgunResponse(hexID, "") != "<"+hexID+">" {
		t.Fatal("no-domain Mailgun response changed")
	}
}

func TestNormalizeAcceptsEveryForm(t *testing.T) {
	for _, in := range []string{hexID, uuidID, "01234567-89AB-CDEF-0123-456789ABCDEF", MailgunHeader(hexID, "example.com"), MailgunResponse(hexID, "Example.COM"), "<" + hexID + ">"} {
		if got, ok := Normalize(in); !ok || got != hexID {
			t.Errorf("%q -> %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "abc", hexID + "0", uuidID[:35], "01234567x89ab-cdef-0123-456789abcdef", "@example.com", hexID + "@", hexID + "@a@b", "<" + hexID, "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"} {
		if got, ok := Normalize(in); ok {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	for _, s := range []string{Postmark(hexID), Resend(hexID), MailgunHeader(hexID, "a.example"), MailgunResponse(hexID, "a.example")} {
		if got, ok := Normalize(s); !ok || got != hexID {
			t.Errorf("%q", s)
		}
	}
}
