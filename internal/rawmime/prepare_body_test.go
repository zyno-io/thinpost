package rawmime

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func bodyInput(t testing.TB, raw string) ([]byte, PrepareInput) {
	t.Helper()
	s := mustScan(t, raw)
	d, err := DecideFrom(s, "", Policy{Mode: FromOff}, verifiedSet("example.com"))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(raw), PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("a1", 16), Date: acceptedAt, Selector: "sel", Signer: testKey(t)}
}

const bodyRaw = "From: al@example.com\r\nTo: b@example.net\r\nMIME-Version: 1.0\r\nContent-Type: text/html;\r\n charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\nX-Last: 1\r\n\r\nline one\nline two\r\n"

func TestPrepareBodySplicesAndSizeParity(t *testing.T) {
	raw, in := bodyInput(t, bodyRaw)
	start := in.Scan.BodyStart
	in.Body = []Splice{{Start: start, End: start + 4, Data: []byte("LINE\r\n")}}
	p, err := Prepare(raw, in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(p.Message, []byte("\r\n\r\nLINE\r\n one\r\nline two\r\n")) {
		t.Fatalf("%q", p.Message[p.HeaderLen:])
	}
	size, err := PreparedSize(raw, in)
	if err != nil || size.Total != len(p.Message) {
		t.Fatalf("size %+v %v vs %d", size, err, len(p.Message))
	}
	// A splice over a span that holds a bare LF.
	in.Body = []Splice{{Start: start, End: start + len("line one\nline two"), Data: []byte("x\r\ny")}}
	p, err = Prepare(raw, in)
	if err != nil || !bytes.HasSuffix(p.Message, []byte("\r\n\r\nx\r\ny\r\n")) {
		t.Fatalf("%v %q", err, p.Message)
	}
	if size, err = PreparedSize(raw, in); err != nil || size.Total != len(p.Message) {
		t.Fatalf("size %+v %v vs %d", size, err, len(p.Message))
	}
}

func TestPrepareTopHTMLReplacesFieldsAndFoldedLines(t *testing.T) {
	raw, in := bodyInput(t, bodyRaw)
	in.TopHTML = &TopLevelHTML{HTMLContentType, HTMLTransferEncoding}
	in.Body = []Splice{{Start: in.Scan.BodyStart, End: len(raw), Data: []byte("new\r\n")}}
	p, err := Prepare(raw, in)
	if err != nil {
		t.Fatal(err)
	}
	head := string(p.Message[:p.HeaderLen])
	for _, want := range []string{"Content-Type: text/html; charset=utf-8\r\n", "Content-Transfer-Encoding: quoted-printable\r\n", "X-Last: 1\r\n"} {
		if strings.Count(head, want) != 1 {
			t.Fatalf("%q in %q", want, head)
		}
	}
	if strings.Contains(head, " charset=utf-8\r\nContent-Transfer-Encoding: 8bit") || strings.Contains(head, "8bit") || strings.Count(head, "\r\n charset") != 0 {
		t.Fatalf("old field remains: %q", head)
	}
	if size, err := PreparedSize(raw, in); err != nil || size.Total != len(p.Message) {
		t.Fatalf("%+v %v", size, err)
	}
	// No Content-Transfer-Encoding: the new one follows the new Content-Type.
	raw2, in2 := bodyInput(t, strings.Replace(bodyRaw, "Content-Transfer-Encoding: 8bit\r\n", "", 1))
	in2.TopHTML = in.TopHTML
	in2.Body = []Splice{{Start: in2.Scan.BodyStart, End: len(raw2), Data: []byte("new\r\n")}}
	p2, err := Prepare(raw2, in2)
	if err != nil || !strings.Contains(string(p2.Message[:p2.HeaderLen]), "Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\nX-Last") {
		t.Fatalf("%v %q", err, p2.Message[:p2.HeaderLen])
	}
}

func TestPrepareBodyRejectsBadSplices(t *testing.T) {
	raw, in := bodyInput(t, bodyRaw)
	start := in.Scan.BodyStart
	cr := start + strings.Index(string(raw[start:]), "\r\n") + 1 // between CR and LF
	for name, body := range map[string][]Splice{
		"before the body":  {{Start: start - 2, End: start, Data: []byte("x")}},
		"out of order":     {{Start: start + 5, End: start + 6}, {Start: start + 1, End: start + 2}},
		"overlap":          {{Start: start, End: start + 5}, {Start: start + 3, End: start + 6}},
		"past the end":     {{Start: start, End: len(raw) + 1}},
		"splits a CRLF":    {{Start: cr, End: cr + 1, Data: []byte("x\r\n")}},
		"bare LF in data":  {{Start: start, End: start + 1, Data: []byte("a\nb")}},
		"lone CR in data":  {{Start: start, End: start + 1, Data: []byte("a\rb")}},
		"too many splices": make([]Splice, 257),
	} {
		in.Body = body
		if _, err := Prepare(raw, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := PreparedSize(raw, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s size: %v", name, err)
		}
	}
	in.Body = nil
	for _, top := range []*TopLevelHTML{{"", "quoted-printable"}, {"text/html\r\nX: y", "quoted-printable"}, {"text/html", ""}} {
		in.TopHTML = top
		if _, err := Prepare(raw, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", top, err)
		}
	}
	// Two top-level Content-Type fields cannot be replaced.
	raw2, in2 := bodyInput(t, strings.Replace(bodyRaw, "X-Last: 1", "Content-Type: text/html", 1))
	in2.TopHTML = &TopLevelHTML{HTMLContentType, HTMLTransferEncoding}
	if _, err := Prepare(raw2, in2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%v", err)
	}
}
