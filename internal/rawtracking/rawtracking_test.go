package rawtracking

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/zyno-io/thinpost/internal/oplog"
	preparedrecipe "github.com/zyno-io/thinpost/internal/prepared"
	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/tracking"
)

const origin = "https://t.example.net"

var (
	keyOnce sync.Once
	testRSA *rsa.PrivateKey
)

func rsaKey(t testing.TB) *rsa.PrivateKey {
	keyOnce.Do(func() {
		var err error
		if testRSA, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return testRSA
}

func linkKeys(t testing.TB) *tracking.LinkKeys {
	t.Helper()
	k, err := tracking.NewLinkKeys([]byte("0123456789abcdef0123456789abcdef"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func scan(t testing.TB, raw string) (*rawmime.Scanned, []byte) {
	t.Helper()
	s, err := rawmime.Scan([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return s, []byte(raw)
}

func wrap76(b []byte) string {
	enc := base64.StdEncoding.EncodeToString(b)
	var sb strings.Builder
	for len(enc) > 76 {
		sb.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	return sb.String() + enc
}

const head = "From: Al <al@example.com>\r\nTo: b@example.net\r\nSubject: hi\r\nDate: Mon, 02 Nov 2026 10:00:00 +0000\r\nMIME-Version: 1.0\r\n"

// signedClient is a message the client DKIM-signed before submitting.
func clientAlternative(attachment []byte) string {
	return head + "Content-Type: multipart/mixed; boundary=\"MIX\"\r\n\r\n--MIX\r\n" +
		"Content-Type: multipart/alternative; boundary=\"ALT\"\r\n\r\n" +
		"--ALT\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nplain https://plain.example.org/x\r\n" +
		"--ALT\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"<html><body><p>Hi <a href=3D\"https://shop.example.org/a?x=3D1\">one</a> <a href=3D\"https://shop.example.org/b\">two</a> caf=C3=A9</p></body></html>\r\n" +
		"--ALT--\r\n" +
		"--MIX\r\nContent-Type: application/pdf; name=\"a.pdf\"\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		wrap76(attachment) + "\r\n--MIX--\r\n"
}

func prepare(t testing.TB, raw []byte, s *rawmime.Scanned, res Result, extra ...func(*rawmime.PrepareInput)) []byte {
	t.Helper()
	d, err := rawmime.DecideFrom(s, "", rawmime.Policy{Mode: rawmime.FromOff}, func(d string) bool { return d == "example.com" })
	if err != nil {
		t.Fatal(err)
	}
	in := rawmime.PrepareInput{
		Scan: s, From: d, MessageID: strings.Repeat("a1", 16), Date: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
		Selector: "sel", Signer: rsaKey(t), Body: res.Body, TopHTML: res.TopHTML,
	}
	for _, f := range extra {
		f(&in)
	}
	p, err := rawmime.Prepare(raw, in)
	if err != nil {
		t.Fatal(err)
	}
	return p.Message
}

func parts(t testing.TB, msg []byte) (top mail.Header, leaves []leaf) {
	t.Helper()
	m, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	var walk func(h textproto.MIMEHeader, body io.Reader)
	walk = func(h textproto.MIMEHeader, body io.Reader) {
		mt, params, err := mime.ParseMediaType(h.Get("Content-Type"))
		if err != nil {
			mt = "text/plain"
		}
		if strings.HasPrefix(mt, "multipart/") {
			mr := multipart.NewReader(body, params["boundary"])
			for {
				p, err := mr.NextRawPart()
				if err != nil {
					return
				}
				walk(p.Header, p)
			}
		}
		raw, _ := io.ReadAll(body)
		decoded := raw
		switch strings.ToLower(h.Get("Content-Transfer-Encoding")) {
		case "quoted-printable":
			decoded, _ = io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
		case "base64":
			decoded, _ = base64.StdEncoding.DecodeString(strings.NewReplacer("\r", "", "\n", "").Replace(string(raw)))
		}
		leaves = append(leaves, leaf{mt, h, raw, decoded})
	}
	walk(textproto.MIMEHeader(m.Header), m.Body)
	return m.Header, leaves
}

type leaf struct {
	mediaType string
	header    textproto.MIMEHeader
	raw       []byte // as encoded
	decoded   []byte
}

func TestPlanOnePixelClicksEverywhereDestinationsHidden(t *testing.T) {
	attachment := make([]byte, 5000)
	_, _ = rand.Read(attachment)
	s, raw := scan(t, clientAlternative(attachment))
	res, err := Plan(raw, s, Options{Opens: true, Clicks: true, Origin: origin, Links: linkKeys(t)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Opens || !res.Clicks || res.TopHTML != nil || len(res.Body) != 3 {
		t.Fatalf("%+v", res)
	}
	clicks, opens := 0, 0
	for _, m := range res.Mappings {
		switch m.Kind {
		case "click":
			clicks++
		case "open":
			opens++
		}
	}
	if clicks != 2 || opens != 1 {
		t.Fatalf("%d clicks %d opens", clicks, opens)
	}
	msg := prepare(t, raw, s, res)
	_, leaves := parts(t, msg)
	if len(leaves) != 3 {
		t.Fatalf("%d leaves", len(leaves))
	}
	if leaves[0].mediaType != "text/plain" || string(leaves[0].decoded) != "plain https://plain.example.org/x" {
		t.Fatalf("text/plain changed: %q", leaves[0].decoded)
	}
	html := string(leaves[1].decoded)
	if strings.Count(html, origin+"/t/") != 3 || strings.Contains(html, "shop.example.org") || !strings.Contains(html, "café") {
		t.Fatalf("html: %s", html)
	}
	if leaves[1].header.Get("Content-Type") != "text/html; charset=utf-8" || leaves[1].header.Get("Content-Transfer-Encoding") != "quoted-printable" {
		t.Fatalf("part headers: %v", leaves[1].header)
	}
	// The attachment is byte-identical: its encoded text is in the output unchanged.
	if !bytes.Equal(leaves[2].decoded, attachment) || !bytes.Contains(msg, []byte(wrap76(attachment)+"\r\n--MIX--")) {
		t.Fatal("attachment changed")
	}
	for _, l := range bytes.Split(leaves[1].raw, []byte("\r\n")) {
		if len(l) > 76 {
			t.Fatalf("QP line of %d octets", len(l))
		}
	}
	if bytes.Contains(msg, []byte("\n")) && bytes.Contains(bytes.ReplaceAll(msg, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatal("bare LF in the output")
	}
}

func TestPlanPixelOnlyInTheFirstEligibleHTMLPart(t *testing.T) {
	raw := head + "Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p><a href=\"https://a.example.org/1\">1</a></p>\r\n" +
		"--B\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p><a href=\"https://a.example.org/2\">2</a></p>\r\n" +
		"--B\r\nContent-Type: text/html; name=\"f.html\"\r\nContent-Disposition: attachment; filename=\"f.html\"\r\n\r\n<a href=\"https://a.example.org/att\">att</a>\r\n--B--\r\n"
	s, b := scan(t, raw)
	res, err := Plan(b, s, Options{Opens: true, Clicks: true, Origin: origin, Links: linkKeys(t)})
	if err != nil {
		t.Fatal(err)
	}
	msg := prepare(t, b, s, res)
	_, leaves := parts(t, msg)
	if len(leaves) != 3 {
		t.Fatal(len(leaves))
	}
	pixels := 0
	for _, l := range leaves[:2] {
		if bytes.Contains(l.decoded, []byte("<img")) {
			pixels++
		}
		if bytes.Contains(l.decoded, []byte("a.example.org")) {
			t.Fatal("destination left in a part")
		}
	}
	if pixels != 1 || !bytes.Contains(leaves[0].decoded, []byte("<img")) {
		t.Fatalf("%d pixels", pixels)
	}
	if string(leaves[2].decoded) != "<a href=\"https://a.example.org/att\">att</a>" {
		t.Fatalf("attached HTML changed: %q", leaves[2].decoded)
	}
}

func TestPlanTopLevelHTMLGetsNewContentFields(t *testing.T) {
	for name, raw := range map[string]string{
		"no encoding field": head + "Content-Type: text/html; charset=utf-8\r\n\r\n<p><a href=\"https://a.example.org/\">x</a></p>\r\n",
		"folded fields":     head + "Content-Type: text/html;\r\n charset=\"utf-8\"\r\nContent-Transfer-Encoding:\r\n 8bit\r\nX-Last: y\r\n\r\n<p><a href=\"https://a.example.org/\">x</a> caf\xc3\xa9</p>\r\n",
		"base64":            head + "Content-Type: text/html\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte("<p><a href=\"https://a.example.org/\">x</a></p>")) + "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			s, b := scan(t, raw)
			res, err := Plan(b, s, Options{Opens: true, Clicks: true, Origin: origin, Links: linkKeys(t)})
			if err != nil || res.TopHTML == nil {
				t.Fatalf("%v %+v", err, res)
			}
			msg := prepare(t, b, s, res)
			top, leaves := parts(t, msg)
			if len(leaves) != 1 || top.Get("Content-Type") != "text/html; charset=utf-8" || top.Get("Content-Transfer-Encoding") != "quoted-printable" {
				t.Fatalf("%v", top)
			}
			if len(top["Content-Type"]) != 1 || len(top["Content-Transfer-Encoding"]) != 1 {
				t.Fatal("duplicate content fields")
			}
			if bytes.Contains(msg, []byte("\r\n charset")) || bytes.Contains(msg, []byte("\r\n 8bit")) {
				t.Fatal("folded lines of the old field remain")
			}
			if strings.Contains(string(leaves[0].decoded), "a.example.org") || !strings.Contains(string(leaves[0].decoded), origin) {
				t.Fatalf("%s", leaves[0].decoded)
			}
			if name == "folded fields" && !bytes.Contains(msg, []byte("X-Last: y")) {
				t.Fatal("neighbouring field lost")
			}
		})
	}
}

func TestPlanUntouchedMail(t *testing.T) {
	for name, tc := range map[string]struct {
		raw    string
		reason Reason
	}{
		"plain text": {head + "Content-Type: text/plain\r\n\r\nhttps://a.example.org/\r\n", ReasonNoHTML},
		"not mime":   {"From: Al <al@example.com>\r\nTo: b@example.net\r\nSubject: x\r\n\r\n<a href=\"https://a.example.org/\">x</a>\r\n", ReasonNotMIME},
		"signed":     {head + "Content-Type: multipart/signed; boundary=S; protocol=\"application/pkcs7-signature\"\r\n\r\n--S\r\nContent-Type: text/html\r\n\r\n<a href=\"https://a.example.org/\">x</a>\r\n--S\r\nContent-Type: application/pkcs7-signature\r\n\r\nxx\r\n--S--\r\n", ReasonSignedOrEncrypted},
		"smime":      {head + "Content-Type: application/pkcs7-mime; smime-type=enveloped-data\r\nContent-Transfer-Encoding: base64\r\n\r\nAAAA\r\n", ReasonSignedOrEncrypted},
		"latin1":     {head + "Content-Type: text/html; charset=iso-8859-1\r\n\r\n<a href=\"https://a.example.org/\">x</a>\r\n", ReasonUnsupportedCharset},
	} {
		t.Run(name, func(t *testing.T) {
			s, b := scan(t, tc.raw)
			_, err := Plan(b, s, Options{Opens: true, Clicks: true, Origin: origin, Links: linkKeys(t)})
			var pe *Error
			if !errors.As(err, &pe) || pe.Reason != tc.reason {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestPlanOneClickStillAppliesToSignedMail(t *testing.T) {
	raw := head + "Content-Type: multipart/signed; boundary=S\r\n\r\n--S\r\nContent-Type: text/plain\r\n\r\nx\r\n--S\r\nContent-Type: application/pkcs7-signature\r\n\r\nxx\r\n--S--\r\n"
	s, b := scan(t, raw)
	if _, err := Plan(b, s, Options{Opens: true, Origin: origin, Links: linkKeys(t)}); err == nil {
		t.Fatal("signed mail was planned")
	}
	msg := prepare(t, b, s, Result{}, func(in *rawmime.PrepareInput) {
		in.ListUnsubscribe = "<https://t.example.net/t/u/v3/k/abc>"
	})
	m, _ := mail.ReadMessage(bytes.NewReader(msg))
	if m.Header.Get("List-Unsubscribe") == "" || !bytes.HasSuffix(msg, []byte(raw[strings.Index(raw, "\r\n\r\n")+4:])) {
		t.Fatal("one-click missing or body touched")
	}
}

func TestPlanLinkCapAcrossParts(t *testing.T) {
	link := func(n, from int) string {
		var sb strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&sb, "<a href=\"https://a.example.org/%d\">x</a>\r\n", from+i)
		}
		return sb.String()
	}
	mk := func(a, b int) string {
		return head + "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/html\r\n\r\n" + link(a, 0) + "\r\n--B\r\nContent-Type: text/html\r\n\r\n" + link(b, 1000) + "\r\n--B--\r\n"
	}
	for name, tc := range map[string]struct {
		a, b  int
		clean bool
	}{"100 total": {60, 40, true}, "101 total": {60, 41, false}, "101 in one": {101, 0, false}} {
		t.Run(name, func(t *testing.T) {
			s, b := scan(t, mk(tc.a, tc.b))
			res, err := Plan(b, s, Options{Clicks: true, Origin: origin, Links: linkKeys(t)})
			var pe *Error
			switch {
			case tc.clean && err != nil, !tc.clean && (!errors.As(err, &pe) || pe.Reason != ReasonTooManyLinks && pe.Reason != ReasonRewriteFailed):
				t.Fatalf("%v", err)
			}
			if tc.clean && len(res.Mappings) != 100 {
				t.Fatalf("%d mappings", len(res.Mappings))
			}
			// Choose drops clicks and keeps the pixel.
			got, reason, err := Choose(b, s, Options{Opens: true, Clicks: true, Origin: origin, Links: linkKeys(t)}, nil)
			if err != nil || got.Opens != true || tc.clean == (reason != ReasonNone) {
				t.Fatalf("%v %v %+v", err, reason, got)
			}
			if !tc.clean && (got.Clicks || len(got.Mappings) != 1) {
				t.Fatalf("clicks not dropped: %+v", got)
			}
		})
	}
}

func TestChooseDropsClicksThenOpensForSize(t *testing.T) {
	raw := head + "Content-Type: text/html\r\n\r\n<a href=\"https://a.example.org/1\">1</a>\r\n"
	s, b := scan(t, raw)
	opts := Options{Opens: true, Clicks: true, Origin: origin, Links: linkKeys(t)}
	var tried []string
	tryFits := func(limit func(Result) bool) func(Result) (bool, error) {
		return func(r Result) (bool, error) {
			tried = append(tried, fmt.Sprintf("%v/%v", r.Opens, r.Clicks))
			return limit(r), nil
		}
	}
	res, reason, err := Choose(b, s, opts, tryFits(func(r Result) bool { return !r.Clicks }))
	if err != nil || !res.Opens || res.Clicks || reason != ReasonMessageTooLarge || strings.Join(tried, ",") != "true/true,true/false" {
		t.Fatalf("%+v %q %v %v", res, reason, err, tried)
	}
	res, reason, err = Choose(b, s, opts, tryFits(func(Result) bool { return false }))
	if err != nil || res.Opens || res.Clicks || len(res.Mappings) != 0 || reason != ReasonMessageTooLarge {
		t.Fatalf("%+v %q %v", res, reason, err)
	}
	res, reason, err = Choose(b, s, opts, nil)
	if err != nil || !res.Opens || !res.Clicks || reason != ReasonNone || len(res.Mappings) != 2 {
		t.Fatalf("%+v %q %v", res, reason, err)
	}
	// Nothing requested, nothing planned.
	res, _, _ = Choose(b, s, Options{Origin: origin, Links: linkKeys(t)}, nil)
	if res.Opens || res.Clicks || len(res.Body) != 0 {
		t.Fatal("planned nothing-requested")
	}
}

func TestPlanClicksOnlyWithoutLinksLeavesTheMessageAlone(t *testing.T) {
	raw := head + "Content-Type: text/html\r\n\r\n<p>no links <a href=\"mailto:x@example.org\">m</a></p>\r\n"
	s, b := scan(t, raw)
	res, err := Plan(b, s, Options{Clicks: true, Origin: origin, Links: linkKeys(t)})
	if err != nil || len(res.Body) != 0 || res.TopHTML != nil || len(res.Mappings) != 0 {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestPlanTokensAreFreshPerCall(t *testing.T) {
	s, b := scan(t, clientAlternative([]byte("x")))
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		res, err := Plan(b, s, Options{Opens: true, Clicks: true, Origin: origin, Links: linkKeys(t)})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range res.Mappings {
			if seen[m.Token] {
				t.Fatal("token reused")
			}
			seen[m.Token] = true
		}
	}
	if len(seen) != 9 {
		t.Fatal(len(seen))
	}
}

func TestPlanTooManySplices(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(head + "Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i := 0; i < 90; i++ {
		fmt.Fprintf(&sb, "--B\r\nContent-Type: text/html\r\nContent-Transfer-Encoding: 7bit\r\n\r\n<a href=\"https://a.example.org/%d\">x</a>\r\n", i)
	}
	sb.WriteString("--B--\r\n")
	s, b := scan(t, sb.String())
	_, err := Plan(b, s, Options{Clicks: true, Origin: origin, Links: linkKeys(t)})
	var pe *Error
	if !errors.As(err, &pe) || pe.Reason != ReasonTooManyParts {
		t.Fatalf("%v", err)
	}
	got, reason, err := Choose(b, s, Options{Opens: true, Clicks: true, Origin: origin, Links: linkKeys(t)}, nil)
	if err != nil || !got.Opens || got.Clicks || reason != ReasonTooManyParts || len(got.Body) > preparedrecipe.MaxSplices {
		t.Fatalf("%v %q %+v", err, reason, got)
	}
}

// Every client DKIM signature goes when the body changes, and the fresh
// signature verifies over the assembled message.
func TestRewrittenBodyDropsClientSignaturesAndVerifies(t *testing.T) {
	raw := "DKIM-Signature: v=1; a=rsa-sha256; d=example.com; s=c; h=from:to:subject; bh=AAAA; b=BBBB\r\n" + clientAlternative([]byte("zz"))
	s, b := scan(t, raw)
	res, err := Plan(b, s, Options{Opens: true, Clicks: true, Origin: origin, Links: linkKeys(t)})
	if err != nil {
		t.Fatal(err)
	}
	msg := prepare(t, b, s, res)
	if bytes.Count(msg, []byte("DKIM-Signature:")) != 1 || bytes.Contains(msg, []byte("s=c;")) {
		t.Fatal("client signature kept")
	}
	// The same raw with no body change keeps a signature naming only unchanged fields.
	untracked := prepare(t, b, s, Result{})
	if bytes.Count(untracked, []byte("DKIM-Signature:")) != 2 {
		t.Fatal("client signature was dropped without a body change")
	}
	der, _ := x509.MarshalPKIXPublicKey(&rsaKey(t).PublicKey)
	record := "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der)
	vs, err := dkim.VerifyWithOptions(bytes.NewReader(msg), &dkim.VerifyOptions{LookupTXT: func(d string) ([]string, error) {
		if d != "sel._domainkey.example.com" {
			return nil, errors.New("lookup")
		}
		return []string{record}, nil
	}})
	if err != nil || len(vs) != 1 || vs[0].Err != nil {
		t.Fatalf("%v %+v", err, vs)
	}
}

// The recipe built from the plan assembles to the signed message, bare LFs in
// the source included.
func TestRecipeAssemblesPlanForBareLFSource(t *testing.T) {
	raw := strings.ReplaceAll(clientAlternative([]byte("abc")), "\r\n", "\n")
	s, b := scan(t, raw)
	res, err := Plan(b, s, Options{Opens: true, Clicks: true, Origin: origin, Links: linkKeys(t)})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := rawmime.DecideFrom(s, "", rawmime.Policy{Mode: rawmime.FromOff}, func(d string) bool { return d == "example.com" })
	in := rawmime.PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("a1", 16), Date: time.Unix(1e9, 0), Selector: "sel", Signer: rsaKey(t), Body: res.Body, TopHTML: res.TopHTML}
	p, err := rawmime.Prepare(b, in)
	if err != nil {
		t.Fatal(err)
	}
	size, err := rawmime.PreparedSize(b, in)
	if err != nil || size.Total != len(p.Message) {
		t.Fatalf("sized %d (%v), built %d", size.Total, err, len(p.Message))
	}
	recipe, err := preparedrecipe.New(strings.Repeat("ab", 16), strings.Repeat("cd", 32), s.BodyStart, p.Message, p.HeaderLen, res.Body)
	if err != nil {
		t.Fatal(err)
	}
	obj, _ := recipe.Encode()
	again, err := preparedrecipe.Decode(obj)
	if err != nil {
		t.Fatal(err)
	}
	out, err := again.Assemble(b, strings.Repeat("cd", 32))
	if err != nil || !bytes.Equal(out, p.Message) {
		t.Fatalf("assemble: %v", err)
	}
	if bytes.Contains(bytes.ReplaceAll(out, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatal("bare LF")
	}
}

func TestReasonsAreLoggable(t *testing.T) {
	for _, r := range []Reason{ReasonNotMIME, ReasonSignedOrEncrypted, ReasonMalformed, ReasonNoHTML, ReasonUnsupportedEncoding, ReasonUnsupportedCharset,
		ReasonTooLarge, ReasonTooManyParts, ReasonTooManyLinks, ReasonRewriteFailed, ReasonMessageTooLarge, ReasonNotConfigured} {
		if !oplog.TrackingSkipReason(string(r)) {
			t.Errorf("oplog does not know %q", r)
		}
	}
}
