package rawmime

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
)

const oneClickLink = "<https://tracking.example.com/t/u/v3/k1/abcdef0123456789>"

func prepareWithLink(t testing.TB, raw, link string) ([]byte, PrepareInput) {
	t.Helper()
	s := mustScan(t, raw)
	d, err := DecideFrom(s, "", Policy{Mode: FromOff}, verifiedSet("example.com"))
	if err != nil {
		t.Fatal(err)
	}
	in := PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("a1", 16), Date: acceptedAt, Selector: "sel", Signer: testKey(t), ListUnsubscribe: link}
	out, err := prepareBytes([]byte(raw), in)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return out, in
}

func TestScanRecordsTheClientsUnsubscribeFields(t *testing.T) {
	for name, tc := range map[string]struct {
		raw                string
		present            bool
		fields, postFields int
		value, postValue   string
	}{
		"none":         {"From: a@example.com\r\n\r\nb", false, 0, 0, "", ""},
		"both":         {"From: a@example.com\r\nList-Unsubscribe: <https://x.example/u>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n\r\nb", true, 1, 1, "<https://x.example/u>", "List-Unsubscribe=One-Click"},
		"folded":       {"From: a@example.com\r\nlist-unsubscribe: <mailto:a@x.example>,\r\n <https://x.example/u>\r\n\r\nb", true, 1, 0, "<mailto:a@x.example>, <https://x.example/u>", ""},
		"post only":    {"From: a@example.com\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n\r\nb", true, 0, 1, "", "List-Unsubscribe=One-Click"},
		"repeated":     {"From: a@example.com\r\nList-Unsubscribe: <https://x.example/1>\r\nList-Unsubscribe: <https://x.example/2>\r\n\r\nb", true, 2, 0, "", ""},
		"malformed ok": {"From: a@example.com\r\nList-Unsubscribe: garbage\r\n\r\nb", true, 1, 0, "garbage", ""},
	} {
		t.Run(name, func(t *testing.T) {
			s := mustScan(t, tc.raw)
			if s.ListUnsubscribe != tc.present || s.UnsubscribeFields != tc.fields || s.UnsubscribePostFields != tc.postFields || s.UnsubscribeValue != tc.value || s.UnsubscribePostValue != tc.postValue {
				t.Fatalf("%+v", s)
			}
		})
	}
}

// Thinpost's two fields replace the client's: the client's are removed, ours
// are the last two fields, and the DKIM signature covers them.
func TestPrepareReplacesTheClientsUnsubscribeFields(t *testing.T) {
	raw := "From: al@example.com\r\nTo: b@example.net\r\nList-Unsubscribe: <https://client.example/u>,\r\n <mailto:c@client.example>\r\nSubject: x\r\nlist-unsubscribe-post: List-Unsubscribe=One-Click\r\nList-Id: <l.example.com>\r\n\r\nbody\r\n"
	out, _ := prepareWithLink(t, raw, oneClickLink)
	sig, rest, body := splitPrepared(t, out)
	names := fieldNames(rest)
	if count(names, "list-unsubscribe") != 1 || count(names, "list-unsubscribe-post") != 1 || strings.Contains(string(out), "client.example") {
		t.Fatalf("client fields survived: %v\n%s", names, rest)
	}
	if got := names[len(names)-2:]; got[0] != "list-unsubscribe" || got[1] != "list-unsubscribe-post" {
		t.Fatalf("the fields are not last: %v", names)
	}
	if !strings.HasSuffix(rest, "List-Unsubscribe: "+oneClickLink+"\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n") || body != "body\r\n" {
		t.Fatalf("rendering: %q", rest)
	}
	lower := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(sig, "\r\n", ""), "\t", ""))
	if !strings.Contains(lower, "list-unsubscribe:list-unsubscribe-post") && !(strings.Contains(lower, "list-unsubscribe") && strings.Contains(lower, "list-unsubscribe-post")) {
		t.Fatalf("DKIM does not cover them: %s", sig)
	}
}

func TestPrepareWithoutALinkLeavesTheClientsFields(t *testing.T) {
	raw := "From: al@example.com\r\nTo: b@example.net\r\nList-Unsubscribe: <https://client.example/u>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n\r\nbody\r\n"
	out, _ := prepareWithLink(t, raw, "")
	if !strings.Contains(string(out), "List-Unsubscribe: <https://client.example/u>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n") {
		t.Fatalf("client fields changed:\n%s", out)
	}
}

func TestPrepareAddsTheFieldsToAMessageWithoutThem(t *testing.T) {
	out, _ := prepareWithLink(t, "From: al@example.com\r\nTo: b@example.net\r\n\r\nbody\r\n", oneClickLink)
	_, rest, _ := splitPrepared(t, out)
	names := fieldNames(rest)
	if count(names, "list-unsubscribe") != 1 || count(names, "list-unsubscribe-post") != 1 {
		t.Fatal(names)
	}
}

func TestPrepareWithTheLinkVerifiesAndSizeMatches(t *testing.T) {
	for _, name := range []string{"basic.eml", "signed.eml", "barelf_quoted_from.eml"} {
		t.Run(name, func(t *testing.T) {
			raw := fixture(t, name)
			out, in := prepareWithLink(t, string(raw), oneClickLink)
			vs, err := dkim.VerifyWithOptions(bytes.NewReader(out), &dkim.VerifyOptions{LookupTXT: func(domain string) ([]string, error) {
				if domain != "sel._domainkey.example.com" {
					return nil, errors.New("unexpected lookup")
				}
				return []string{txtRecord(t)}, nil
			}})
			if err != nil || len(vs) == 0 || vs[0].Err != nil || vs[0].Domain != "example.com" {
				t.Fatalf("%v %+v", err, vs)
			}
			size, err := PreparedSize(raw, in)
			if err != nil || size.Total != len(out) {
				t.Fatalf("PreparedSize %+v want %d: %v", size, len(out), err)
			}
			sizing := in
			sizing.Signer, _ = SizingSigner(2048)
			if again, err := PreparedSize(raw, sizing); err != nil || again != size {
				t.Fatalf("sizing signer %+v %v", again, err)
			}
		})
	}
}

// A client signature is dropped exactly when its h= names one of the fields
// Thinpost replaced, even if the message had none (oversigned): the new fields
// would break it. Other signatures stay, and verify.
func TestPrepareDropsClientSignaturesCoveringTheUnsubscribeFields(t *testing.T) {
	sig := func(h string) string {
		return "DKIM-Signature: v=1; a=rsa-sha256; d=example.com; s=k; h=" + h + "; bh=AA; b=BB\r\n"
	}
	for name, tc := range map[string]struct {
		sig  string
		keep bool
	}{
		"unrelated":      {sig("from:to:subject"), true},
		"list":           {sig("from:list-unsubscribe"), false},
		"post":           {sig("from:list-unsubscribe-post"), false},
		"folded and mix": {sig("from:To:\r\n List-Unsubscribe"), false},
		"list-id only":   {sig("from:list-id"), true},
	} {
		t.Run(name, func(t *testing.T) {
			raw := "From: al@example.com\r\nTo: b@example.net\r\n" + tc.sig + "\r\nbody"
			out, _ := prepareWithLink(t, raw, oneClickLink)
			_, rest, _ := splitPrepared(t, out)
			if kept := count(fieldNames(rest), "dkim-signature") == 1; kept != tc.keep {
				t.Fatalf("kept=%v\n%s", kept, rest)
			}
		})
	}
	// Without a link the same signature naming the fields stays.
	raw := "From: al@example.com\r\nList-Unsubscribe: <mailto:a@b.example>\r\n" + sig("from:list-unsubscribe") + "\r\nbody"
	out, _ := prepareWithLink(t, raw, "")
	if _, rest, _ := splitPrepared(t, out); count(fieldNames(rest), "dkim-signature") != 1 {
		t.Fatal("signature dropped without a replacement")
	}
}

// A real client signature that covers the fields is dropped, one that does not is
// kept and still verifies next to Thinpost's.
func TestPrepareKeepsAValidClientSignatureThatDoesNotCoverTheFields(t *testing.T) {
	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := "From: Alice <alice@example.com>\r\nTo: bob@example.net\r\nSubject: signed\r\nDate: Mon, 16 Nov 2026 08:00:00 +0000\r\nList-Unsubscribe: <https://client.example/u>\r\n\r\nbody\r\n"
	sign := func(keys ...string) string {
		var signed bytes.Buffer
		err := dkim.Sign(&signed, strings.NewReader(unsigned), &dkim.SignOptions{Domain: "example.com", Selector: "client", Signer: clientKey, Hash: crypto.SHA256,
			HeaderCanonicalization: dkim.CanonicalizationRelaxed, BodyCanonicalization: dkim.CanonicalizationRelaxed, HeaderKeys: keys})
		if err != nil {
			t.Fatal(err)
		}
		return signed.String()
	}
	clientDER, _ := x509.MarshalPKIXPublicKey(&clientKey.PublicKey)
	clientRecord := "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(clientDER)
	verify := func(out []byte) []*dkim.Verification {
		vs, err := dkim.VerifyWithOptions(bytes.NewReader(out), &dkim.VerifyOptions{LookupTXT: func(name string) ([]string, error) {
			if name == "client._domainkey.example.com" {
				return []string{clientRecord}, nil
			}
			return []string{txtRecord(t)}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		return vs
	}
	out, _ := prepareWithLink(t, sign("From", "To", "Subject", "Date"), oneClickLink)
	vs := verify(out)
	if len(vs) != 2 || vs[0].Err != nil || vs[1].Err != nil || strings.Contains(string(out), "client.example") {
		t.Fatalf("%+v", vs)
	}
	out, _ = prepareWithLink(t, sign("From", "To", "List-Unsubscribe"), oneClickLink)
	if vs = verify(out); len(vs) != 1 || vs[0].Err != nil || vs[0].Domain != "example.com" {
		t.Fatalf("a signature covering the replaced field survived: %+v", vs)
	}
}

func TestPrepareRejectsAnInvalidLink(t *testing.T) {
	raw := "From: al@example.com\r\nTo: b@example.net\r\n\r\nb"
	s := mustScan(t, raw)
	d, _ := DecideFrom(s, "", Policy{Mode: FromOff}, verifiedSet("example.com"))
	long := "<https://tracking.example.com/" + strings.Repeat("a", MaxLine) + ">"
	for name, bad := range map[string]string{
		"no brackets": "https://tracking.example.com/u", "http": "<http://tracking.example.com/u>", "mailto": "<mailto:a@b.example>",
		"space": "<https://tracking.example.com/a b>", "two items": "<https://a.example/u>,<https://b.example/u>",
		"nested bracket": "<https://a.example/<u>", "non-ASCII": "<https://träck.example/u>", "control": "<https://a.example/\x01>",
		"empty host": "<https://>", "over a line": long, "newline": "<https://a.example/u>\r\nX: y",
	} {
		in := PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("a1", 16), Date: acceptedAt, Selector: "sel", Signer: testKey(t), ListUnsubscribe: bad}
		if _, err := Prepare([]byte(raw), in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
		if _, err := PreparedSize([]byte(raw), in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s sized: %v", name, err)
		}
	}
}
