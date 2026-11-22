package rawmime

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"mime/quotedprintable"
	"strings"
	"testing"
	"unicode/utf8"

	preparedrecipe "github.com/zyno-io/thinpost/internal/prepared"
)

const topHdr = "From: a@example.com\nTo: b@example.net\nSubject: x\nMIME-Version: 1.0\n"

func toCRLF(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

func wrap64(b []byte) string {
	enc := base64.StdEncoding.EncodeToString(b)
	var sb strings.Builder
	for len(enc) > 76 {
		sb.WriteString(enc[:76] + "\n")
		enc = enc[76:]
	}
	return sb.String() + enc
}

func multi(sub, boundary string, parts ...string) string {
	var sb strings.Builder
	sb.WriteString(topHdr + "Content-Type: multipart/" + sub + `; boundary="` + boundary + "\"\n\npreamble\n")
	for _, p := range parts {
		sb.WriteString("--" + boundary + "\n" + p + "\n")
	}
	sb.WriteString("--" + boundary + "--\nepilogue\n")
	return sb.String()
}

func htmlPart(body string) string {
	return "Content-Type: text/html; charset=utf-8\nContent-Transfer-Encoding: quoted-printable\n\n" + body
}

const textPart = "Content-Type: text/plain; charset=utf-8\n\nhello"

// checkSplices verifies the invariants every Replace result must keep.
func checkSplices(t testing.TB, raw []byte, s *Scanned, sp []Splice) {
	t.Helper()
	prev := s.BodyStart
	for _, p := range sp {
		if p.Start < prev || p.End < p.Start || p.End > len(raw) {
			t.Fatalf("splice %d..%d out of order or bounds (prev %d, len %d)", p.Start, p.End, prev, len(raw))
		}
		for _, at := range []int{p.Start, p.End} {
			if at > 0 && at < len(raw) && raw[at-1] == '\r' && raw[at] == '\n' {
				t.Fatalf("splice boundary %d splits a CRLF", at)
			}
		}
		if bytes.Contains(bytes.ReplaceAll(p.Data, []byte("\r\n"), nil), []byte{'\n'}) || bytes.Contains(bytes.ReplaceAll(p.Data, []byte("\r\n"), nil), []byte{'\r'}) {
			t.Fatalf("splice data is not CRLF-only: %q", p.Data)
		}
		prev = p.End
	}
}

func qpLinesOK(t testing.TB, data []byte) {
	t.Helper()
	for _, l := range bytes.Split(data, []byte("\r\n")) {
		if len(l) > 76 {
			t.Fatalf("QP line of %d octets", len(l))
		}
	}
}

// replaceAll applies Replace(part.HTML) for every part and returns the new
// message (CRLF-normalized), doing the top-level header surgery by hand.
func replaceAll(t testing.TB, raw []byte, s *Scanned, parts []HTMLPart, htmls []string) []byte {
	t.Helper()
	var sp []Splice
	var top *TopLevelHTML
	for i, p := range parts {
		got, tl, err := p.Replace(htmls[i])
		if err != nil {
			t.Fatalf("Replace: %v", err)
		}
		sp = append(sp, got...)
		if tl != nil {
			top = tl
		}
	}
	for i := 1; i < len(sp); i++ { // merge parts' splices into source order
		for j := i; j > 0 && sp[j].Start < sp[j-1].Start; j-- {
			sp[j], sp[j-1] = sp[j-1], sp[j]
		}
	}
	checkSplices(t, raw, s, sp)
	prs := make([]preparedrecipe.Splice, len(sp))
	var data []byte
	for i, p := range sp {
		prs[i] = preparedrecipe.Splice{Start: p.Start, End: p.End, Len: len(p.Data)}
		data = append(data, p.Data...)
		qpLinesOK(t, p.Data)
	}
	body, err := preparedrecipe.AppendBody(nil, raw, s.BodyStart, prs, data)
	if err != nil {
		t.Fatalf("AppendBody: %v", err)
	}
	hdr := raw[:s.BodyStart]
	if top != nil {
		var cut []Span
		for _, p := range parts {
			cut = append(cut, p.ContentType, p.TransferEncoding)
		}
		if cut[0].Start > cut[1].Start {
			cut[0], cut[1] = cut[1], cut[0]
		}
		var nh []byte
		pos := 0
		for _, c := range cut {
			nh = append(nh, raw[pos:c.Start]...)
			pos = c.End
		}
		nh = append(nh, raw[pos:s.HeaderEnd]...)
		nh = append(nh, "Content-Type: "+top.ContentType+"\r\nContent-Transfer-Encoding: "+top.TransferEncoding+"\r\n"...)
		hdr = append(nh, raw[s.HeaderEnd:s.BodyStart]...)
	}
	head, err := preparedrecipe.AppendBody(nil, hdr, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return append(head, body...)
}

func TestHTMLParts(t *testing.T) {
	qp := "<p>caf=C3=A9 =3D ok</p>"
	qpHTML := "<p>café = ok</p>"
	b64 := wrap64([]byte("<p>café</p>\n<b>x</b>"))
	cases := []struct {
		name   string
		raw    string
		want   []string // decoded HTML of the eligible parts, in order
		reason Reason
		top    bool
	}{
		{"single qp", topHdr + "Content-Type: text/html; charset=UTF-8\nContent-Transfer-Encoding: quoted-printable\n\n" + qp + "\n", []string{qpHTML + "\r\n"}, "", true},
		{"single base64", topHdr + "Content-Type: text/html; charset=\"utf-8\"\nContent-Transfer-Encoding: BASE64\n\n" + b64 + "\n", []string{"<p>café</p>\r\n<b>x</b>"}, "", true},
		{"single 7bit no CTE no charset", topHdr + "Content-Type: text/html\n\n<p>hi</p>\n", []string{"<p>hi</p>\r\n"}, "", true},
		{"single 7bit us-ascii", topHdr + "Content-Type: text/html; charset=us-ascii\nContent-Transfer-Encoding: 7bit\n\n<p>hi</p>", []string{"<p>hi</p>"}, "", true},
		{"single 8bit", topHdr + "Content-Type: text/html; charset=utf-8\nContent-Transfer-Encoding: 8bit\n\n<p>café</p>\n", []string{"<p>café</p>\r\n"}, "", true},
		{"single empty body", topHdr + "Content-Type: text/html\n\n", []string{""}, "", true},
		{"single no separator", topHdr + "Content-Type: text/html", nil, ReasonMalformed, false},
		{"single text/plain", topHdr + "Content-Type: text/plain\n\nhi\n", nil, ReasonNoHTML, false},
		{"no content type", topHdr + "\nhi\n", nil, ReasonNoHTML, false},
		{"folded content type", topHdr + "Content-Type: text/html;\n charset=utf-8\n\n<p>x</p>\n", []string{"<p>x</p>\r\n"}, "", true},
		{"inline disposition", topHdr + "Content-Type: text/html\nContent-Disposition: inline\n\n<p>x</p>\n", []string{"<p>x</p>\r\n"}, "", true},
		{"attachment disposition", topHdr + "Content-Type: text/html\nContent-Disposition: attachment; filename=a.html\n\n<p>x</p>\n", nil, ReasonNoHTML, false},
		{"name param", topHdr + "Content-Type: text/html; name=a.html\n\n<p>x</p>\n", nil, ReasonNoHTML, false},
		{"Content-Length", topHdr + "Content-Type: text/html\nContent-Length: 6\n\n<p>x</p>\n", nil, ReasonNoHTML, false},
		{"no MIME-Version", "From: a@example.com\nContent-Type: text/html\n\n<p>x</p>\n", nil, ReasonNotMIME, false},
		{"two Content-Type", topHdr + "Content-Type: text/html\nContent-Type: text/html\n\n<p>x</p>\n", nil, ReasonMalformed, false},
		{"two CTE", topHdr + "Content-Type: text/html\nContent-Transfer-Encoding: 7bit\nContent-Transfer-Encoding: 7bit\n\n<p>x</p>\n", nil, ReasonMalformed, false},
		{"iso-8859-1", topHdr + "Content-Type: text/html; charset=iso-8859-1\n\n<p>x</p>\n", nil, ReasonUnsupportedCharset, false},
		{"invalid utf-8", topHdr + "Content-Type: text/html; charset=utf-8\nContent-Transfer-Encoding: 8bit\n\n<p>\xff</p>\n", nil, ReasonUnsupportedCharset, false},
		{"binary encoding", topHdr + "Content-Type: text/html\nContent-Transfer-Encoding: binary\n\n<p>x</p>\n", nil, ReasonUnsupportedEncoding, false},
		{"invalid qp", topHdr + "Content-Type: text/html\nContent-Transfer-Encoding: quoted-printable\n\n<p>x</p>==", nil, ReasonUnsupportedEncoding, false},
		{"invalid qp in part", multi("alternative", "b1", htmlPart("abc==")), nil, ReasonUnsupportedEncoding, false},
		{"lenient qp escape kept literal", topHdr + "Content-Type: text/html\nContent-Transfer-Encoding: quoted-printable\n\na=4G\n", []string{"a=4G\r\n"}, "", true},
		{"invalid base64", topHdr + "Content-Type: text/html\nContent-Transfer-Encoding: base64\n\n!!!!\n", nil, ReasonUnsupportedEncoding, false},
		{"unpadded base64", topHdr + "Content-Type: text/html\nContent-Transfer-Encoding: base64\n\nPGI+eDwvYj4\n", []string{"<b>x</b>"}, "", true},

		{"alternative", multi("alternative", "b1", textPart, htmlPart(qp)), []string{qpHTML}, "", false},
		{"related", multi("related", "b1", htmlPart(qp), "Content-Type: image/png\nContent-Transfer-Encoding: base64\nContent-ID: <a>\n\n"+wrap64(bytes.Repeat([]byte{1, 2, 3}, 100))), []string{qpHTML}, "", false},
		{"mixed attached html skipped, inline eligible", multi("mixed", "b1",
			"Content-Type: text/html; name=\"a.html\"\nContent-Disposition: attachment; filename=\"a.html\"\n\n<p>attached</p>",
			"Content-Type: text/html\nContent-Disposition: inline\n\n<p>inline</p>"), []string{"<p>inline</p>"}, "", false},
		{"mixed only attachment", multi("mixed", "b1", "Content-Type: text/html\nContent-Disposition: attachment\n\n<p>a</p>"), nil, ReasonNoHTML, false},
		{"two html parts", multi("alternative", "b1", htmlPart("one"), htmlPart("two")), []string{"one", "two"}, "", false},
		{"nested", multi("mixed", "outer", "Content-Type: multipart/alternative; boundary=inner\n\n--inner\n"+textPart+"\n--inner\n"+
			"Content-Type: multipart/related; boundary=\"in/ner:2\"\n\n--in/ner:2\n"+htmlPart(qp)+"\n--in/ner:2--\n\n--inner--\n",
			"Content-Type: application/pdf\n\n%PDF"), []string{qpHTML}, "", false},
		{"skip bad charset keep good", multi("alternative", "b1", "Content-Type: text/html; charset=iso-8859-1\n\n<p>x</p>", htmlPart("good")), []string{"good"}, "", false},
		{"only bad charset", multi("alternative", "b1", textPart, "Content-Type: text/html; charset=iso-8859-1\n\n<p>x</p>"), nil, ReasonUnsupportedCharset, false},
		{"first skip reason wins", multi("alternative", "b1", "Content-Type: text/html\nContent-Transfer-Encoding: binary\n\nx", "Content-Type: text/html; charset=koi8-r\n\nx"), nil, ReasonUnsupportedEncoding, false},
		{"signed", multi("signed", "b1", htmlPart(qp), "Content-Type: application/pgp-signature\n\nsig"), nil, ReasonSignedOrEncrypted, false},
		{"encrypted", multi("encrypted", "b1", "Content-Type: application/pgp-encrypted\n\nVersion: 1", "Content-Type: application/octet-stream\n\nx"), nil, ReasonSignedOrEncrypted, false},
		{"top pkcs7", topHdr + "Content-Type: application/pkcs7-mime; smime-type=enveloped-data\nContent-Transfer-Encoding: base64\n\nAAAA\n", nil, ReasonSignedOrEncrypted, false},
		{"nested x-pkcs7", multi("mixed", "b1", htmlPart(qp), "Content-Type: application/x-pkcs7-mime\n\nAAAA"), nil, ReasonSignedOrEncrypted, false},
		{"nested signed", multi("mixed", "b1", htmlPart(qp), "Content-Type: multipart/signed; boundary=s\n\n--s\n"+textPart+"\n--s--\n"), nil, ReasonSignedOrEncrypted, false},
		{"other multipart subtype", multi("report", "b1", htmlPart(qp)), nil, ReasonNoHTML, false},
		{"nested other subtype", multi("mixed", "b1", htmlPart(qp), "Content-Type: multipart/digest; boundary=d\n\n--d\n\nx\n--d--\n"), nil, ReasonNoHTML, false},
		{"inner message/rfc822 untouched", multi("mixed", "b1", htmlPart(qp),
			"Content-Type: message/rfc822\n\nMIME-Version: 1.0\nContent-Type: text/html\n\n<p>inner</p>"), []string{qpHTML}, "", false},
		{"only message/rfc822", multi("mixed", "b1", "Content-Type: message/rfc822\n\nMIME-Version: 1.0\nContent-Type: text/html\n\n<p>inner</p>"), nil, ReasonNoHTML, false},
		{"missing closing boundary", topHdr + "Content-Type: multipart/mixed; boundary=b1\n\n--b1\n" + htmlPart("x") + "\n", nil, ReasonMalformed, false},
		{"nested missing closing", multi("mixed", "b1", "Content-Type: multipart/alternative; boundary=in\n\n--in\n"+htmlPart("x")), nil, ReasonMalformed, false},
		{"no boundary param", topHdr + "Content-Type: multipart/mixed\n\n--b1\n" + htmlPart("x") + "\n--b1--\n", nil, ReasonMalformed, false},
		{"bad boundary char", topHdr + "Content-Type: multipart/mixed; boundary=\"a\\\"b\"\n\n--a\"b\n" + htmlPart("x") + "\n--a\"b--\n", nil, ReasonMalformed, false},
		{"unparsable part header", multi("mixed", "b1", "no colon here\n\nx"), nil, ReasonMalformed, false},
		{"duplicate part Content-Type", multi("mixed", "b1", "Content-Type: text/html\nContent-Type: text/html\n\nx"), nil, ReasonMalformed, false},
		{"transport padding", topHdr + "Content-Type: multipart/alternative; boundary=b1\n\n--b1 \t\n" + textPart + "\n--b1   \n" + htmlPart("x") + "\n--b1-- \t\n", []string{"x"}, "", false},
		{"quoted boundary with space", topHdr + "Content-Type: multipart/alternative; boundary=\"a b\"\n\n--a b\n" + htmlPart("x") + "\n--a b--\n", []string{"x"}, "", false},
		{"boundary-like lines in content", multi("alternative", "b1", "Content-Type: text/html; charset=utf-8\n\nline\n--b1x\n-- b1\n--b\n--B1\nend"), []string{"line\r\n--b1x\r\n-- b1\r\n--b\r\n--B1\r\nend"}, "", false},
		{"empty html leaf", multi("alternative", "b1", "Content-Type: text/html\n\n"), []string{""}, "", false},
		{"html leaf headers only", multi("alternative", "b1", "Content-Type: text/html"), nil, ReasonMalformed, false},
		{"empty part skipped", multi("alternative", "b1", "", htmlPart("x")), []string{"x"}, "", false},
		{"preamble delimiter-like text", topHdr + "Content-Type: multipart/alternative; boundary=b1\n\nnot --b1\n--b1\n" + htmlPart("x") + "\n--b1--", []string{"x"}, "", false},
		{"part CTE absent", multi("alternative", "b1", "Content-Type: text/html; charset=utf-8\n\n<p>x</p>"), []string{"<p>x</p>"}, "", false},
		{"part CTE before CT", multi("alternative", "b1", "Content-Transfer-Encoding: 7bit\nContent-Type: text/html\nX-Other: 1\n\n<p>x</p>"), []string{"<p>x</p>"}, "", false},
	}
	for _, c := range cases {
		for _, ending := range []string{"lf", "crlf", "mixed"} {
			t.Run(c.name+"/"+ending, func(t *testing.T) {
				src := c.raw
				switch ending {
				case "crlf":
					src = toCRLF(src)
				case "mixed":
					var sb strings.Builder
					for i, l := range strings.SplitAfter(src, "\n") {
						if i%2 == 0 {
							l = strings.Replace(l, "\n", "\r\n", 1)
						}
						sb.WriteString(l)
					}
					src = sb.String()
				}
				raw := []byte(src)
				s, err := Scan(raw)
				if err != nil {
					t.Skipf("scan: %v", err) // invalid byte policy (e.g. 8-bit header) is not this test's subject
				}
				parts, reason := HTMLParts(raw, s)
				if reason != c.reason {
					t.Fatalf("reason %q, want %q (parts %d)", reason, c.reason, len(parts))
				}
				if len(parts) != len(c.want) {
					t.Fatalf("%d parts, want %d", len(parts), len(c.want))
				}
				var htmls []string
				for i, p := range parts {
					if p.HTML != c.want[i] {
						t.Errorf("part %d html %q, want %q", i, p.HTML, c.want[i])
					}
					if p.TopLevel != c.top {
						t.Errorf("TopLevel %v", p.TopLevel)
					}
					htmls = append(htmls, p.HTML)
				}
				if len(parts) == 0 {
					return
				}
				// Round trip.
				out := replaceAll(t, raw, s, parts, htmls)
				s2, err := Scan(out)
				if err != nil {
					t.Fatalf("Scan of result: %v", err)
				}
				again, reason := HTMLParts(out, s2)
				if reason != "" || len(again) != len(parts) {
					t.Fatalf("result: %d parts, reason %q", len(again), reason)
				}
				for i, p := range again {
					if p.HTML != c.want[i] {
						t.Errorf("round trip html %q, want %q", p.HTML, c.want[i])
					}
					if !p.HasCTE {
						t.Error("result part has no CTE")
					}
					ct := string(out[p.ContentType.Start:p.ContentType.End])
					cte := string(out[p.TransferEncoding.Start:p.TransferEncoding.End])
					if ct != "Content-Type: text/html; charset=utf-8\r\n" || cte != "Content-Transfer-Encoding: quoted-printable\r\n" {
						t.Errorf("headers %q %q", ct, cte)
					}
				}
			})
		}
	}
}

func TestHTMLPartsOtherPartsByteIdentical(t *testing.T) {
	pdf := wrap64(bytes.Repeat([]byte("pdfdata"), 200))
	raw := []byte(toCRLF(multi("mixed", "b1", textPart, "Content-Type: multipart/alternative; boundary=in\n\n--in\n"+htmlPart("=C3=A9")+"\n--in--\n",
		"Content-Type: application/pdf\nContent-Transfer-Encoding: base64\nContent-Disposition: attachment; filename=a.pdf\n\n"+pdf)))
	s := mustScan(t, string(raw))
	parts, _ := HTMLParts(raw, s)
	if len(parts) != 1 {
		t.Fatal(len(parts))
	}
	out := replaceAll(t, raw, s, parts, []string{"<p>new ✓ =</p>\r\n"})
	for _, keep := range []string{toCRLF(pdf), "hello", "preamble", "epilogue", "--in--", "--b1--"} {
		if !bytes.Contains(out, []byte(keep)) {
			t.Errorf("lost %q", keep[:min(len(keep), 20)])
		}
	}
	if !bytes.Contains(out, []byte("<p>new =E2=9C=93 =3D</p>\r\n\r\n--in--")) {
		t.Errorf("content not replaced as expected: %q", out[len(out)-400:])
	}
}

func TestReplaceTopLevelDetails(t *testing.T) {
	raw := []byte(toCRLF(topHdr + "Content-Type: text/html\n\n<p>x</p>"))
	s := mustScan(t, string(raw))
	parts, _ := HTMLParts(raw, s)
	sp, top, err := parts[0].Replace("<p>y</p>")
	if err != nil || top == nil || top.ContentType != "text/html; charset=utf-8" || top.TransferEncoding != "quoted-printable" {
		t.Fatalf("%v %+v", err, top)
	}
	if len(sp) != 1 || sp[0].Start != s.BodyStart || sp[0].End != len(raw) || string(sp[0].Data) != "<p>y</p>=\r\n" {
		t.Fatalf("%+v", sp)
	}
	sp, _, _ = parts[0].Replace("<p>y</p>\r\n")
	if string(sp[0].Data) != "<p>y</p>\r\n" {
		t.Fatalf("%q", sp[0].Data)
	}
}

func TestReplaceLeafInsertsCTE(t *testing.T) {
	raw := []byte(toCRLF(multi("alternative", "b1", "Content-Type: text/html\n\n<p>x</p>")))
	s := mustScan(t, string(raw))
	parts, _ := HTMLParts(raw, s)
	p := parts[0]
	if p.HasCTE || p.TransferEncoding.Start != p.TransferEncoding.End || p.TransferEncoding.Start != p.ContentType.End {
		t.Fatalf("%+v", p)
	}
	sp, top, _ := p.Replace("x\r\n")
	if top != nil || len(sp) != 3 || sp[1].Start != sp[1].End || string(sp[2].Data) != "x\r\n" {
		t.Fatalf("%+v", sp)
	}
}

func TestHTMLPartsLimits(t *testing.T) {
	nest := func(levels int) string {
		inner := htmlPart("x")
		for i := levels; i >= 1; i-- {
			b := fmt.Sprintf("b%d", i)
			inner = "Content-Type: multipart/mixed; boundary=" + b + "\n\n--" + b + "\n" + inner + "\n--" + b + "--"
		}
		return topHdr + inner + "\n"
	}
	// strip the first part-level header copy: the outermost "Content-Type" is the message's.
	for _, c := range []struct {
		levels int
		reason Reason
	}{{1, ""}, {8, ""}, {9, ReasonTooManyParts}, {30, ReasonTooManyParts}} {
		raw := []byte(nest(c.levels))
		s := mustScan(t, string(raw))
		parts, reason := HTMLParts(raw, s)
		if reason != c.reason || (reason == "" && len(parts) != 1) {
			t.Errorf("levels %d: reason %q, %d parts", c.levels, reason, len(parts))
		}
	}
	many := func(n int) string {
		ps := make([]string, n)
		for i := range ps {
			ps[i] = "Content-Type: text/plain\n\nx"
		}
		return multi("mixed", "b1", append(ps, htmlPart("x"))...)
	}
	for _, c := range []struct {
		leaves int
		reason Reason
	}{{126, ""}, {127, ReasonTooManyParts}, {300, ReasonTooManyParts}} {
		raw := []byte(many(c.leaves))
		s := mustScan(t, string(raw))
		_, reason := HTMLParts(raw, s)
		if reason != c.reason {
			t.Errorf("%d leaves: reason %q", c.leaves, reason)
		}
	}
}

func TestHTMLPartsTooLarge(t *testing.T) {
	big := func(n int) string {
		return topHdr + "Content-Type: text/html\nContent-Transfer-Encoding: base64\n\n" + wrap64(bytes.Repeat([]byte("a"), n)) + "\n"
	}
	for _, c := range []struct {
		n      int
		reason Reason
	}{{MaxHTMLBytes, ""}, {MaxHTMLBytes + 1, ReasonTooLarge}} {
		raw := []byte(big(c.n))
		s := mustScan(t, string(raw))
		parts, reason := HTMLParts(raw, s)
		if reason != c.reason || (reason == "" && len(parts) != 1) {
			t.Errorf("%d bytes: reason %q", c.n, reason)
		}
	}
	// 7bit content over the bound.
	line := strings.Repeat("a", 76) + "\n"
	raw := []byte(topHdr + "Content-Type: text/html\n\n" + strings.Repeat(line, MaxHTMLBytes/77+10))
	if parts, reason := HTMLParts(raw, mustScan(t, string(raw))); reason != ReasonTooLarge || parts != nil {
		t.Errorf("7bit: %q", reason)
	}
}

func TestHTMLPartsScanMismatch(t *testing.T) {
	raw := []byte(topHdr + "Content-Type: text/html\n\nx")
	s := mustScan(t, string(raw))
	if p, r := HTMLParts(raw[:len(raw)-1], s); p != nil || r != ReasonMalformed {
		t.Errorf("%v %q", p, r)
	}
	if p, r := HTMLParts(raw, nil); p != nil || r != ReasonMalformed {
		t.Errorf("%v %q", p, r)
	}
}

func TestEncodeQP(t *testing.T) {
	cases := []string{
		"", "a", "plain text", "a=b", "tab\there", "trailing space ", " leading", "\t", " \r\n \n \t\r\n",
		"é✓\x00\x01", "a\rb", "a\r", "x\r\n", "x\n", "\n\n\n", "line\r\nline2\nline3",
		strings.Repeat("a", 74), strings.Repeat("a", 75), strings.Repeat("a", 76), strings.Repeat("a", 77), strings.Repeat("a", 500),
		strings.Repeat("é", 100), strings.Repeat("= ", 100), strings.Repeat("a b ", 60) + "\r\n" + strings.Repeat("\t", 90),
		strings.Repeat("a", 73) + "=", strings.Repeat("a", 74) + "é", strings.Repeat("a", 72) + " x",
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 300; i++ {
		b := make([]byte, rng.Intn(400))
		for j := range b {
			b[j] = "ab =\t\r\n\x00\xc3\xa9."[rng.Intn(11)]
		}
		cases = append(cases, string(b))
	}
	for _, s := range cases {
		for _, final := range []bool{false, true} {
			enc := encodeQP(nil, s, final)
			qpLinesOK(t, enc)
			if bytes.Contains(bytes.ReplaceAll(enc, []byte("\r\n"), nil), []byte{'\n'}) || bytes.Contains(bytes.ReplaceAll(enc, []byte("\r\n"), nil), []byte{'\r'}) {
				t.Fatalf("not CRLF-only: %q", enc)
			}
			if final && !bytes.HasSuffix(enc, []byte("\r\n")) {
				t.Fatalf("final does not end in CRLF: %q", enc)
			}
			dec, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(enc)))
			if err != nil {
				t.Fatalf("decode %q: %v", enc, err)
			}
			want := string(widenLF([]byte(s)))
			if string(dec) != want {
				t.Fatalf("round trip of %q: got %q (enc %q)", s, dec, enc)
			}
		}
	}
}

func FuzzHTMLParts(f *testing.F) {
	seeds := []string{
		topHdr + "Content-Type: text/html\n\n<p>x</p>\n",
		topHdr + "Content-Type: text/html; charset=utf-8\nContent-Transfer-Encoding: base64\n\nPHA+eDwvcD4=\n",
		topHdr + "Content-Type: text/html;\n charset=utf-8\nContent-Transfer-Encoding: quoted-printable\n\ncaf=C3=A9 =\n",
		multi("alternative", "b1", textPart, htmlPart("caf=C3=A9 \n\n tail ")),
		multi("mixed", "b1", "Content-Type: multipart/related; boundary=r\n\n--r\nContent-Type: text/html\n\n<p>x</p>\n--r--\n", "Content-Type: message/rfc822\n\nContent-Type: text/html\n\nx"),
		multi("mixed", "b1", "Content-Transfer-Encoding: 7bit\nContent-Type: text/html\n\nx", "Content-Type: text/html\n\n\n"),
		topHdr + "Content-Type: multipart/mixed; boundary=b1\n\n--b1 \t\n\n--b1--",
	}
	for _, sd := range seeds {
		f.Add([]byte(sd), false)
		f.Add([]byte(toCRLF(sd)), true)
	}
	f.Fuzz(func(t *testing.T, raw []byte, _ bool) {
		s, err := Scan(raw)
		if err != nil {
			return
		}
		parts, reason := HTMLParts(raw, s)
		if len(parts) > 0 && reason != "" || len(parts) == 0 && reason == "" {
			t.Fatalf("%d parts with reason %q", len(parts), reason)
		}
		if len(parts) > MaxMIMEParts {
			t.Fatalf("%d parts", len(parts))
		}
		prevEnd := 0
		var htmls []string
		for _, p := range parts {
			if p.Start < s.BodyStart || p.End < p.Start || p.End > len(raw) || p.Start < prevEnd {
				t.Fatalf("content %d..%d (body %d, prev %d, len %d)", p.Start, p.End, s.BodyStart, prevEnd, len(raw))
			}
			for _, sp := range []Span{p.ContentType, p.TransferEncoding} {
				if sp.Start > sp.End || sp.End > p.Start {
					t.Fatalf("header span %v after content %d", sp, p.Start)
				}
				if !p.TopLevel && sp.Start < s.BodyStart {
					t.Fatalf("part header span %v before body", sp)
				}
				if p.TopLevel && sp.End > s.HeaderEnd {
					t.Fatalf("top-level header span %v past header end %d", sp, s.HeaderEnd)
				}
			}
			if len(p.HTML) > MaxHTMLBytes+MaxHTMLBytes/2 || !utf8.ValidString(p.HTML) {
				t.Fatalf("bad html")
			}
			prevEnd = p.End
			htmls = append(htmls, p.HTML)
		}
		if len(parts) == 0 {
			return
		}
		out := replaceAll(t, raw, s, parts, htmls) // checks splices, CRLF, QP lines
		s2, err := Scan(out)
		if err != nil {
			t.Fatalf("Scan of result: %v", err)
		}
		again, reason := HTMLParts(out, s2)
		if reason != "" || len(again) != len(parts) {
			t.Fatalf("result: %d parts (want %d), reason %q", len(again), len(parts), reason)
		}
		for i := range again {
			if again[i].HTML != parts[i].HTML {
				t.Fatalf("part %d html changed", i)
			}
		}
	})
}

// A Content-Length on a container (or the top level) would be kept over a
// rewritten body, so the whole message stays untracked.
func TestHTMLPartsContentLengthOnAContainer(t *testing.T) {
	inner := "Content-Type: multipart/alternative; boundary=\"I\"\nContent-Length: 10\n\n--I\n" + htmlPart("<p>x</p>") + "\n--I--"
	for name, raw := range map[string]string{
		"top level":   strings.Replace(multi("mixed", "B", htmlPart("<p>x</p>")), "Content-Type: multipart", "Content-Length: 99\nContent-Type: multipart", 1),
		"nested":      multi("mixed", "B", inner),
		"leaf (skip)": multi("mixed", "B", "Content-Length: 5\n"+htmlPart("<p>x</p>")),
	} {
		raw = toCRLF(raw)
		s := mustScan(t, raw)
		parts, reason := HTMLParts([]byte(raw), s)
		if len(parts) != 0 {
			t.Errorf("%s: %d parts", name, len(parts))
		}
		want := ReasonMalformed
		if name == "leaf (skip)" {
			want = ReasonNoHTML
			if reason != ReasonNoHTML && reason != ReasonNone {
				t.Errorf("%s: %q", name, reason)
			}
			continue
		}
		if reason != want {
			t.Errorf("%s: reason %q", name, reason)
		}
	}
}
