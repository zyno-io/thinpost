package preparation

import (
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/rawmime"
	"github.com/zyno-io/thinpost/internal/submission"
)

// decided runs real rawmime.DecideFrom output through the codec.
func decided(t *testing.T, raw, mailFrom string, p rawmime.Policy) (submission.RawHeader, error) {
	t.Helper()
	s, err := rawmime.Scan([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	d, err := rawmime.DecideFrom(s, mailFrom, p, func(d string) bool { return d == "example.com" })
	if err != nil {
		t.Fatalf("DecideFrom: %v", err)
	}
	h := submission.RawHeader{
		Recipients: []string{"b@example.net"}, Kinds: []string{"to"}, SenderDomain: d.Domain, FromRewritten: d.Rewritten,
		FromField: d.Field, OriginalFrom: d.Original, StripSender: d.StripSender,
	}
	if d.Field != "" {
		h.From = d.Value
	}
	data, err := submission.EncodeRawSource(h, []byte(raw))
	if err != nil {
		return h, err
	}
	src, err := submission.DecodeSource(data, false)
	if err != nil || src.RawHeader.FromField != h.FromField || src.RawHeader.OriginalFrom != h.OriginalFrom {
		t.Fatalf("decode: %v", err)
	}
	return h, nil
}

func TestCodecAcceptsRealDecideFromOutput(t *testing.T) {
	const body = "To: b@example.net\r\nSubject: hi\r\n\r\nbody\r\n"
	cases := map[string]struct {
		raw, mailFrom string
		policy        rawmime.Policy
	}{
		"rewrite quoted source":    {"From: \"team ops\"@example.org\r\n" + body, "", rawmime.Policy{Mode: rawmime.FromAddress, Address: "noreply@example.com"}},
		"rewrite at in local part": {"From: \"a@b\"@example.org\r\n" + body, "", rawmime.Policy{Mode: rawmime.FromAddress, Address: "noreply@example.com"}},
		"insert quoted mail from":  {body, "\"team ops\"@example.com", rawmime.Policy{Mode: rawmime.FromOff}},
		"insert plain mail from":   {body, "a@example.com", rawmime.Policy{Mode: rawmime.FromOff}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h, err := decided(t, c.raw, c.mailFrom, c.policy)
			if err != nil {
				t.Fatalf("rejected %q: %v", h.OriginalFrom, err)
			}
		})
	}
}

func TestCodecAcceptsTheLongestFoldedFromField(t *testing.T) {
	// 450 non-ASCII characters (900 bytes decoded) as RFC 2047 words folded across lines.
	word := "=?UTF-8?q?" + strings.Repeat("=C3=A9", 15) + "?="
	name := strings.TrimSuffix(strings.Repeat(word+"\r\n ", 30), "\r\n ")
	raw := "From: " + name + " <a@example.org>\r\nTo: b@example.net\r\nSubject: hi\r\n\r\nbody\r\n"
	h, err := decided(t, raw, "", rawmime.Policy{Mode: rawmime.FromAddress, Address: "noreply@example.com"})
	if err != nil {
		t.Fatalf("len %d: %v", len(h.FromField), err)
	}
	if len(h.FromField) <= 2048 {
		t.Fatalf("field only %d bytes: the test no longer covers the old limit", len(h.FromField))
	}
}
