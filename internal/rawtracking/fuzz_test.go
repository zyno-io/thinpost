package rawtracking

import (
	"bytes"
	"strings"
	"testing"
	"time"

	preparedrecipe "github.com/zyno-io/thinpost/internal/prepared"
	"github.com/zyno-io/thinpost/internal/rawmime"
)

// FuzzPlanPrepare runs Scan, Plan and Prepare on arbitrary bytes. Whenever all
// succeed, the sized length equals the built length, the output has no bare LF
// or CR, and the recipe assembles to exactly the signed message.
func FuzzPlanPrepare(f *testing.F) {
	for _, seed := range []string{
		clientAlternative([]byte("attachment")),
		strings.ReplaceAll(clientAlternative([]byte("attachment")), "\r\n", "\n"),
		head + "Content-Type: text/html\r\n\r\n<a href=\"https://a.example.org/\">x</a>",
		head + "Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\nPGEgaHJlZj0iaHR0cHM6Ly9hLmV4YW1wbGUub3JnLyI+eDwvYT4=\r\n",
		head + "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/html\r\n\r\n<a href=\"https://a.example.org/\">x</a>\n--B--\n",
	} {
		f.Add([]byte(seed))
	}
	keys := linkKeys(f)
	key := rsaKey(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		s, err := rawmime.Scan(raw)
		if err != nil {
			return
		}
		res, err := Plan(raw, s, Options{Opens: true, Clicks: true, Origin: origin, Links: keys})
		if err != nil {
			return
		}
		d, err := rawmime.DecideFrom(s, "", rawmime.Policy{Mode: rawmime.FromOff}, func(string) bool { return true })
		if err != nil {
			return
		}
		in := rawmime.PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("a1", 16), Date: time.Unix(1e9, 0), Selector: "sel", Signer: key, Body: res.Body, TopHTML: res.TopHTML}
		p, err := rawmime.Prepare(raw, in)
		if err != nil {
			return
		}
		if size, err := rawmime.PreparedSize(raw, in); err != nil || size.Total != len(p.Message) {
			t.Fatalf("sized %+v %v, built %d", size, err, len(p.Message))
		}
		if bytes.Contains(bytes.ReplaceAll(p.Message, []byte("\r\n"), nil), []byte("\n")) || bytes.Contains(bytes.ReplaceAll(p.Message, []byte("\r\n"), nil), []byte("\r")) {
			t.Fatal("bare CR or LF in the output")
		}
		base := strings.Repeat("cd", 32)
		recipe, err := preparedrecipe.New(strings.Repeat("ab", 16), base, s.BodyStart, p.Message, p.HeaderLen, res.Body)
		if err != nil {
			return // e.g. a header block over the recipe bound
		}
		obj, err := recipe.Encode()
		if err != nil {
			return
		}
		again, err := preparedrecipe.Decode(obj)
		if err != nil {
			t.Fatal(err)
		}
		out, err := again.Assemble(raw, base)
		if err != nil || !bytes.Equal(out, p.Message) {
			t.Fatalf("recipe does not assemble to the signed message: %v", err)
		}
	})
}
