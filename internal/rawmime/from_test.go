package rawmime

import (
	"errors"
	"net/mail"
	"strings"
	"testing"
)

func scanFrom(t *testing.T, from string, extra string) *Scanned {
	t.Helper()
	hdr := ""
	if from != "" {
		hdr = "From: " + from + "\r\n"
	}
	return mustScan(t, hdr+extra+"\r\nbody")
}

func TestDecideFromMatrix(t *testing.T) {
	ver := verifiedSet("example.com", "example.net")
	domainT := Policy{Mode: FromDomain, Domain: "example.net", OnlyUnauthorized: true}
	cases := []struct {
		name     string
		from     string // header value; "" for no From field
		mailFrom string
		policy   Policy
		extra    string
		wantErr  error
		addr     string // effective address
		rewrite  bool
		original string
		strip    bool
	}{
		{name: "off authorized", from: "Al <al@example.com>", policy: Policy{Mode: FromOff}, addr: "al@example.com"},
		{name: "off empty mode", from: "al@example.com", policy: Policy{}, addr: "al@example.com"},
		{name: "off unauthorized", from: "al@example.org", policy: Policy{Mode: FromOff}, wantErr: ErrFromNotPermitted},
		{name: "off two froms", from: "a@example.com\r\nFrom: b@example.com", policy: Policy{Mode: FromOff}, wantErr: ErrFromNotPermitted},
		{name: "off missing", mailFrom: "", policy: Policy{Mode: FromOff}, wantErr: ErrInvalid},
		{name: "off missing null path", mailFrom: "<>", policy: Policy{Mode: FromOff}, wantErr: ErrInvalid},
		{name: "off missing from uses mail from", mailFrom: "bounce@example.com", policy: Policy{Mode: FromOff}, addr: "bounce@example.com", rewrite: true, original: "bounce@example.com", strip: true},
		{name: "off missing from unauthorized mail from", mailFrom: "x@example.org", policy: Policy{Mode: FromOff}, wantErr: ErrFromNotPermitted},
		{name: "off preserved quoted local", from: `"team ops"@example.com`, policy: Policy{Mode: FromOff}, addr: "team ops@example.com"},
		{name: "off preserved quoted local with name", from: `Team <"team ops"@example.com>`, policy: Policy{Mode: FromOff}, addr: "team ops@example.com"},
		{name: "off domain case", from: "al@EXAMPLE.com", policy: Policy{Mode: FromOff}, addr: "al@EXAMPLE.com"},

		{name: "only_unauthorized keeps authorized", from: "al@example.com", policy: domainT, addr: "al@example.com"},
		{name: "only_unauthorized preserved quoted local", from: `"team ops"@example.com`, policy: domainT, addr: "team ops@example.com"},
		{name: "domain rewrites unauthorized", from: "Al <al@example.org>", policy: domainT, addr: "al@example.net", rewrite: true, original: "al@example.org", strip: true},
		{name: "domain all (only_unauthorized off)", from: "al@example.com", policy: Policy{Mode: FromDomain, Domain: "example.net"}, addr: "al@example.net", rewrite: true, original: "al@example.com", strip: true},
		{name: "domain keep origin", from: "al@example.org", policy: Policy{Mode: FromDomain, Domain: "example.net", KeepOrigin: true, OnlyUnauthorized: true}, addr: "al+example.org@example.net", rewrite: true, original: "al@example.org", strip: true},
		{name: "domain keep origin same domain not added", from: "al@example.com", policy: Policy{Mode: FromDomain, Domain: "example.com", KeepOrigin: true}, addr: "al@example.com", rewrite: true, original: "al@example.com", strip: true},
		{name: "domain keep origin too long drops origin", from: strings.Repeat("a", 55) + "@example.org", policy: Policy{Mode: FromDomain, Domain: "example.net", KeepOrigin: true, OnlyUnauthorized: true}, addr: strings.Repeat("a", 55) + "@example.net", rewrite: true, original: strings.Repeat("a", 55) + "@example.org", strip: true},
		{name: "domain local over 64 falls back", from: strings.Repeat("a", 65) + "@example.org", policy: Policy{Mode: FromDomain, Domain: "example.net", Address: "noreply@example.net", OnlyUnauthorized: true}, addr: "noreply@example.net", rewrite: true, original: strings.Repeat("a", 65) + "@example.org", strip: true},
		{name: "domain quoted local falls back", from: `"team ops"@example.org`, policy: Policy{Mode: FromDomain, Domain: "example.net", Address: "noreply@example.net", OnlyUnauthorized: true}, addr: "noreply@example.net", rewrite: true, original: "team ops@example.org", strip: true},
		{name: "domain quoted local no fallback", from: `"team ops"@example.org`, policy: domainT, wantErr: ErrFromNotPermitted},
		{name: "domain invalid from falls back", from: "a@example.org, b@example.org", policy: Policy{Mode: FromDomain, Domain: "example.net", Address: "noreply@example.net", OnlyUnauthorized: true}, addr: "noreply@example.net", rewrite: true, strip: true},
		{name: "domain invalid from no fallback", from: "a@example.org, b@example.org", policy: domainT, wantErr: ErrFromNotPermitted},
		{name: "domain unverified target", from: "al@example.org", policy: Policy{Mode: FromDomain, Domain: "example.org"}, wantErr: ErrMisconfigured},
		{name: "domain target not LDH", from: "al@example.org", policy: Policy{Mode: FromDomain, Domain: "Example.Net"}, wantErr: ErrMisconfigured},
		{name: "domain unverified fallback", from: `"x y"@example.org`, policy: Policy{Mode: FromDomain, Domain: "example.net", Address: "noreply@example.org"}, wantErr: ErrMisconfigured},
		{name: "domain keeps display name", from: "Al Smith <al@example.org>", policy: domainT, addr: "al@example.net", rewrite: true, original: "al@example.org", strip: true},

		{name: "address mode", from: "al@example.org", policy: Policy{Mode: FromAddress, Address: "noreply@example.net"}, addr: "noreply@example.net", rewrite: true, original: "al@example.org", strip: true},
		{name: "address mode many froms", from: "a@example.org\r\nFrom: b@example.org", policy: Policy{Mode: FromAddress, Address: "noreply@example.net"}, addr: "noreply@example.net", rewrite: true, strip: true},
		{name: "address mode missing from", mailFrom: "", policy: Policy{Mode: FromAddress, Address: "noreply@example.net"}, addr: "noreply@example.net", rewrite: true, strip: true},
		{name: "address mode only_unauthorized keeps authorized", from: "al@example.com", policy: Policy{Mode: FromAddress, Address: "noreply@example.net", OnlyUnauthorized: true}, addr: "al@example.com"},
		{name: "address non-ascii", from: "al@example.org", policy: Policy{Mode: FromAddress, Address: "nöreply@example.net"}, wantErr: ErrMisconfigured},
		{name: "address quoted local", from: "al@example.org", policy: Policy{Mode: FromAddress, Address: `"no reply"@example.net`}, wantErr: ErrMisconfigured},
		{name: "address uppercase domain", from: "al@example.org", policy: Policy{Mode: FromAddress, Address: "noreply@Example.net"}, wantErr: ErrMisconfigured},
		{name: "address unverified", from: "al@example.org", policy: Policy{Mode: FromAddress, Address: "noreply@example.org"}, wantErr: ErrMisconfigured},
		{name: "address empty", from: "al@example.org", policy: Policy{Mode: FromAddress}, wantErr: ErrMisconfigured},
		{name: "unknown mode", from: "al@example.com", policy: Policy{Mode: "weird"}, wantErr: ErrMisconfigured},

		{name: "sender on verified domain kept", from: "al@example.com", extra: "Sender: bot@example.net\r\n", policy: Policy{Mode: FromOff}, addr: "al@example.com"},
		{name: "sender on unverified domain stripped", from: "al@example.com", extra: "Sender: bot@example.org\r\n", policy: Policy{Mode: FromOff}, addr: "al@example.com", strip: true},
		{name: "unparsable sender stripped", from: "al@example.com", extra: "Sender: nonsense\r\n", policy: Policy{Mode: FromOff}, addr: "al@example.com", strip: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scanFrom(t, c.from, c.extra)
			d, err := DecideFrom(s, c.mailFrom, c.policy, ver)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("got %v (%+v), want %v", err, d, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Address != c.addr || d.Rewritten != c.rewrite || d.Original != c.original || d.StripSender != c.strip {
				t.Fatalf("got %+v", d)
			}
			if d.Domain != strings.ToLower(d.Domain) || d.Domain != domainOf(d.Address) {
				t.Fatalf("domain %q", d.Domain)
			}
			if strings.ContainsAny(d.Value, "\r\n") {
				t.Fatalf("Value is not one line: %q", d.Value)
			}
			if back, err := mail.ParseAddress(d.Value); err != nil || back.Address != d.Address {
				t.Fatalf("Value %q does not parse: %v", d.Value, err)
			}
			if (d.Field != "") != d.Rewritten {
				t.Fatalf("Field %q vs Rewritten %v", d.Field, d.Rewritten)
			}
			checkField(t, d)
		})
	}
}

func checkField(t *testing.T, d FromDecision) {
	t.Helper()
	if d.Field == "" {
		return
	}
	if !strings.HasPrefix(d.Field, "From: ") || !strings.HasSuffix(d.Field, "\r\n") {
		t.Fatalf("field %q", d.Field)
	}
	for _, line := range strings.Split(strings.TrimSuffix(d.Field, "\r\n"), "\r\n") {
		if len(line) > MaxLine || strings.TrimSpace(line) == "" {
			t.Fatalf("bad line %q", line)
		}
	}
	unfolded := strings.ReplaceAll(strings.TrimSuffix(d.Field, "\r\n"), "\r\n ", " ")
	if unfolded != "From: "+d.Value {
		t.Fatalf("unfolds to %q, want %q", unfolded, "From: "+d.Value)
	}
}

func TestDecideFromPreservedQuotedLocalValue(t *testing.T) {
	s := scanFrom(t, `"team ops"@example.com`, "")
	d, err := DecideFrom(s, "", Policy{Mode: FromOff}, verifiedSet("example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Value != `<"team ops"@example.com>` || d.Field != "" || d.Rewritten {
		t.Fatalf("%+v", d)
	}
}

func TestDecideFromFoldedOriginalKeepsSingleLineValue(t *testing.T) {
	s := mustScan(t, "From: Alice Example\r\n <alice@example.com>\r\n\r\nbody")
	d, err := DecideFrom(s, "", Policy{Mode: FromOff}, verifiedSet("example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(d.Value, "\r\n") || d.Field != "" {
		t.Fatalf("%+v", d)
	}
}

func TestDecideFromLongNameFolding(t *testing.T) {
	name := strings.TrimSpace(strings.Repeat("Alexandria ", 30)) // 329 chars of words
	s := scanFrom(t, name+" <al@example.org>", "")
	d, err := DecideFrom(s, "", Policy{Mode: FromDomain, Domain: "example.net", OnlyUnauthorized: true}, verifiedSet("example.net"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != name || strings.Count(d.Field, "\r\n") < 5 {
		t.Fatalf("not folded: %q", d.Field)
	}
	for _, line := range strings.Split(strings.TrimSuffix(d.Field, "\r\n"), "\r\n") {
		if len(line) > foldTarget+len("Alexandria") {
			t.Fatalf("line too long (%d): %q", len(line), line)
		}
	}
	checkField(t, d)
}

func TestFoldFieldBounds(t *testing.T) {
	// "From: " is 6 octets, so 992 more is exactly 998.
	if _, err := FoldField("From", strings.Repeat("a", 992)); err != nil {
		t.Fatalf("exact: %v", err)
	}
	if _, err := FoldField("From", strings.Repeat("a", 993)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over: %v", err)
	}
	for _, bad := range []string{"", " a", "a ", "a\r\nb", "a\x00", "caf\xc3\xa9"} {
		if _, err := FoldField("From", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	got, err := FoldField("X", "a  b c")
	if err != nil || got != "X: a  b c\r\n" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestDecideFromNonASCIINameIsEncoded(t *testing.T) {
	s := scanFrom(t, "=?UTF-8?Q?Caf=C3=A9?= <al@example.org>", "")
	d, err := DecideFrom(s, "", Policy{Mode: FromAddress, Address: "noreply@example.net"}, verifiedSet("example.net"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "Café" || !isASCII(d.Value) || !isASCII(d.Field) {
		t.Fatalf("%+v", d)
	}
}

func TestDecideFromMultipleSenders(t *testing.T) {
	ver := verifiedSet("example.com")
	cases := []struct {
		name, extra string
		strip       bool
	}{
		{"one verified", "Sender: ok@example.com\r\n", false},
		{"verified then unverified", "Sender: ok@example.com\r\nSender: ceo@example.org\r\n", true},
		{"unverified then verified", "Sender: ceo@example.org\r\nSender: ok@example.com\r\n", true},
		{"two verified", "Sender: a@example.com\r\nSender: b@example.com\r\n", true},
		{"two mailboxes in one", "Sender: a@example.com, b@example.org\r\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := DecideFrom(scanFrom(t, "al@example.com", c.extra), "", Policy{Mode: FromOff}, ver)
			if err != nil || d.StripSender != c.strip {
				t.Fatalf("%+v %v", d, err)
			}
		})
	}
}

func TestDecideFromNameIsCleanedAndBounded(t *testing.T) {
	// RFC 2047 words decode to CR, LF, NUL and a very long run.
	long := strings.Repeat("=?UTF-8?Q?aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_?=\r\n ", 60)
	for _, from := range []string{
		"=?UTF-8?Q?a=0D=0Ab=00c?= <al@example.org>",
		long + "<al@example.org>",
	} {
		for _, p := range []Policy{{Mode: FromAddress, Address: "noreply@example.net"}, {Mode: FromOff}} {
			s := scanFrom(t, from, "")
			ver := verifiedSet("example.net", "example.org")
			d, err := DecideFrom(s, "", p, ver)
			if err != nil {
				t.Fatalf("%v", err)
			}
			if len(d.Name) > 998 || strings.ContainsAny(d.Name, "\x00\r\n") || strings.ContainsAny(d.Value, "\r\n\x00") {
				t.Fatalf("name %q", d.Name)
			}
			if d.Field != "" {
				checkField(t, d)
				if !strings.Contains(d.Field, d.Name[:1]) {
					t.Fatalf("field %q", d.Field)
				}
			}
		}
	}
	s := scanFrom(t, "=?UTF-8?Q?a=0D=0Ab=00c?= <al@example.org>", "")
	d, err := DecideFrom(s, "", Policy{Mode: FromAddress, Address: "noreply@example.net"}, verifiedSet("example.net"))
	if err != nil || d.Name != "abc" || d.Value != `"abc" <noreply@example.net>` {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestFoldFieldNeverEmitsBlankLine(t *testing.T) {
	for _, v := range []string{
		strings.Repeat("a", 90) + " \t " + strings.Repeat("b", 90),
		strings.Repeat("a", 90) + "\t " + strings.Repeat("b", 90),
		strings.Repeat("a b\t", 100) + "c",
	} {
		got, err := FoldField("X", v)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSuffix(got, "\r\n"), "\r\n") {
			if strings.TrimSpace(line) == "" {
				t.Fatalf("blank line in %q", got)
			}
		}
		if strings.ReplaceAll(got, "\r\n ", " ") != "X: "+v+"\r\n" {
			t.Fatalf("does not unfold: %q", got)
		}
	}
}
