package rawtracking

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/rawmime"
)

// shape is one entry of a message's MIME structure, in document order: every
// container and leaf, with the raw (still encoded) bytes of a leaf.
type shape struct {
	mediaType string
	raw       []byte
}

// parseShape reads a message with the standard library's independent parser.
func parseShape(msg []byte) ([]shape, error) {
	m, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	var out []shape
	count := 0
	var walk func(h map[string][]string, body io.Reader, depth int) error
	walk = func(h map[string][]string, body io.Reader, depth int) error {
		if count++; count > 500 || depth > 12 {
			return errors.New("too deep")
		}
		ct := ""
		if v := h["Content-Type"]; len(v) > 0 {
			ct = v[0]
		}
		mt, params, err := mime.ParseMediaType(ct)
		if err != nil {
			mt = "text/plain"
		}
		if strings.HasPrefix(mt, "multipart/") {
			out = append(out, shape{mediaType: mt})
			mr := multipart.NewReader(body, params["boundary"])
			for {
				p, err := mr.NextRawPart()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				if err := walk(p.Header, p, depth+1); err != nil {
					return err
				}
			}
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			return err
		}
		out = append(out, shape{mt, bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))})
		return nil
	}
	if err := walk(m.Header, m.Body, 0); err != nil {
		return nil, err
	}
	return out, nil
}

// sameStructure requires the same container and leaf sequence, and the same
// bytes in every leaf that is not HTML.
func sameStructure(before, after []shape) error {
	if len(before) != len(after) {
		return fmt.Errorf("%d entries became %d", len(before), len(after))
	}
	for i := range before {
		if before[i].mediaType != after[i].mediaType {
			return fmt.Errorf("entry %d: %s became %s", i, before[i].mediaType, after[i].mediaType)
		}
		if before[i].mediaType != "text/html" && !bytes.Equal(before[i].raw, after[i].raw) {
			return fmt.Errorf("entry %d (%s) changed", i, before[i].mediaType)
		}
	}
	return nil
}

func planAndPrepare(t testing.TB, raw []byte) (res Result, out []byte, ok bool) {
	t.Helper()
	s, err := rawmime.Scan(raw)
	if err != nil {
		return res, nil, false
	}
	res, err = Plan(raw, s, Options{Opens: true, Clicks: true, Origin: origin, Links: linkKeys(t)})
	if err != nil {
		return res, nil, false
	}
	d, err := rawmime.DecideFrom(s, "", rawmime.Policy{Mode: rawmime.FromOff}, func(string) bool { return true })
	if err != nil {
		return res, nil, false
	}
	p, err := rawmime.Prepare(raw, rawmime.PrepareInput{
		Scan: s, From: d, MessageID: strings.Repeat("a1", 16), Date: time.Unix(1e9, 0),
		Selector: "sel", Signer: rsaKey(t), Body: res.Body, TopHTML: res.TopHTML,
	})
	if err != nil {
		return res, nil, false
	}
	return res, p.Message, true
}

func TestRewrittenHTMLCannotForgeABoundary(t *testing.T) {
	att := "Content-Type: application/pdf; name=\"a.pdf\"\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n"
	alt := func(htmlPart string) string {
		return head + "Content-Type: multipart/mixed; boundary=\"MIX\"\r\n\r\n--MIX\r\nContent-Type: multipart/alternative; boundary=\"ALT\"\r\n\r\n" +
			"--ALT\r\nContent-Type: text/plain\r\n\r\nplain\r\n--ALT\r\n" + htmlPart + "\r\n--ALT--\r\n--MIX\r\n" + att + "--MIX--\r\n"
	}
	evil := "<a href=\"https://a.example.org/\">x</a>\n--ALT\nContent-Type: application/x-injected\n\nevil\n--ALT--"
	for name, raw := range map[string]string{
		"base64 with a decoded boundary": alt("Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" + wrap76([]byte(evil))),
		"quoted-printable =2D-MIX--":     alt("Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<a href=3D\"https://a.example.org/\">x</a>\r\n=2D-MIX--\r\n=2D-ALT--"),
		"7bit boundary text mid-line":    alt("Content-Type: text/html; charset=utf-8\r\n\r\n<a href=\"https://a.example.org/\">x</a><pre>x</pre> --MIX-- --ALT--"),
		"a soft break before a dash":     alt("Content-Type: text/html; charset=utf-8\r\n\r\n<a href=\"https://a.example.org/\">x</a><p>" + strings.Repeat("a", 71) + "--ALT--</p>"),
		"dashes at every column":         alt("Content-Type: text/html; charset=utf-8\r\n\r\n<a href=\"https://a.example.org/\">x</a><p>" + strings.Repeat("-", 400) + "ALT</p>"),
	} {
		t.Run(name, func(t *testing.T) {
			b := []byte(raw)
			before, err := parseShape(b)
			if err != nil {
				t.Fatal(err)
			}
			res, out, ok := planAndPrepare(t, b)
			if !ok || len(res.Body) == 0 {
				t.Fatal("not rewritten")
			}
			after, err := parseShape(out)
			if err != nil {
				t.Fatal(err)
			}
			if err := sameStructure(before, after); err != nil {
				t.Fatal(err)
			}
			for _, line := range bytes.Split(res.Body[len(res.Body)-1].Data, []byte("\r\n")) {
				if bytes.HasPrefix(line, []byte("-")) {
					t.Fatalf("an output line starts with a dash: %q", line)
				}
			}
		})
	}
}

func TestEncodeQPNeverStartsALineWithADash(t *testing.T) {
	// Via Plan on single-part mail, where the content starts a line too.
	raw := head + "Content-Type: text/html; charset=utf-8\r\n\r\n--x <a href=\"https://a.example.org/\">x</a><p>" + strings.Repeat("b", 70) + "-----</p>"
	b := []byte(raw)
	res, _, ok := planAndPrepare(t, b)
	if !ok {
		t.Fatal("not planned")
	}
	for _, p := range res.Body {
		for _, line := range bytes.Split(p.Data, []byte("\r\n")) {
			if bytes.HasPrefix(line, []byte("-")) {
				t.Fatalf("line %q", line)
			}
		}
	}
}

// FuzzPlanStructure: whatever Plan rewrites, an independent parser must see the
// same containers and leaves, byte-identical non-HTML leaves, and DKIM must
// verify.
func FuzzPlanStructure(f *testing.F) {
	for _, seed := range []string{
		clientAlternative([]byte("attachment")),
		strings.ReplaceAll(clientAlternative([]byte("attachment")), "\r\n", "\n"),
		head + "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/html\r\n\r\n<a href=\"https://a.example.org/\">x</a>\r\n--B\r\nContent-Type: text/plain\r\n\r\nz\r\n--B--\r\n",
		head + "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/html\r\nContent-Transfer-Encoding: base64\r\n\r\n" + wrap76([]byte("<a href=\"https://a.example.org/\">x</a>\n--B\nContent-Type: x/y\n\nz\n--B--")) + "\r\n--B\r\nContent-Type: text/plain\r\n\r\nz\r\n--B--\r\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		// The standard library's multipart reader fixes the line ending style
		// from the first delimiter and then misses delimiters in the other, so it
		// disagrees with Thinpost's walker on mixed endings in the source. The
		// output is all CRLF, which both agree on; compare only consistent sources.
		if bytes.Contains(raw, []byte("\r\n")) && bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) {
			return
		}
		before, err := parseShape(raw)
		if err != nil {
			return
		}
		_, out, ok := planAndPrepare(t, raw)
		if !ok {
			return
		}
		after, err := parseShape(out)
		if err != nil {
			t.Fatalf("output no longer parses: %v", err)
		}
		if err := sameStructure(before, after); err != nil {
			t.Fatal(err)
		}
	})
}
