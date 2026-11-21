package dkimkey

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zyno-io/thinpost/internal/objectstore"
)

func testRing(t *testing.T) *objectstore.Keyring {
	t.Helper()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := os.WriteFile(path, []byte(`{"active":"k1","keys":{"k1":"`+key+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ring, err := objectstore.LoadKeyring(path)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func TestCheckRecord(t *testing.T) {
	spki := []byte("0123456789abcdef-public-key-bytes-0123456789")
	p := base64.StdEncoding.EncodeToString(spki)
	other := base64.StdEncoding.EncodeToString([]byte("a different key entirely"))
	// A record that DNS returned split over several 255-character strings is
	// already joined by the resolver; whitespace inside p= can remain.
	split := p[:10] + " " + p[10:20] + "\t" + p[20:]
	for name, tc := range map[string]struct {
		records []string
		want    Reason
	}{
		"exact":                           {[]string{"v=DKIM1; k=rsa; p=" + p}, ReasonOK},
		"only p":                          {[]string{"p=" + p}, ReasonOK},
		"whitespace in p":                 {[]string{"v=DKIM1; k=rsa; p=" + split}, ReasonOK},
		"extra tags":                      {[]string{"v=DKIM1; k=rsa; h=sha256; s=email; n=notes; p=" + p}, ReasonOK},
		"trailing semicolon":              {[]string{"v=DKIM1; k=rsa; p=" + p + ";"}, ReasonOK},
		"testing mode":                    {[]string{"v=DKIM1; k=rsa; t=y; p=" + p}, ReasonTestingMode},
		"strict flag is fine":             {[]string{"v=DKIM1; k=rsa; t=s; p=" + p}, ReasonOK},
		"no record":                       {nil, ReasonNotFound},
		"two records":                     {[]string{"v=DKIM1; p=" + p, "v=DKIM1; p=" + p}, ReasonMultipleRecords},
		"two records, one other":          {[]string{"v=DKIM1; p=" + p, "unrelated"}, ReasonMultipleRecords},
		"different key":                   {[]string{"v=DKIM1; k=rsa; p=" + other}, ReasonDifferentKey},
		"revoked key":                     {[]string{"v=DKIM1; k=rsa; p="}, ReasonDifferentKey},
		"not base64":                      {[]string{"v=DKIM1; k=rsa; p=%%%"}, ReasonDifferentKey},
		"different key and t=y":           {[]string{"v=DKIM1; t=y; p=" + other}, ReasonDifferentKey},
		"unrelated text":                  {[]string{"hello world"}, ReasonUnsupported},
		"ed25519":                         {[]string{"v=DKIM1; k=ed25519; p=" + p}, ReasonUnsupported},
		"sha1 only":                       {[]string{"v=DKIM1; h=sha1; p=" + p}, ReasonUnsupported},
		"other service":                   {[]string{"v=DKIM1; s=other; p=" + p}, ReasonUnsupported},
		"duplicate p":                     {[]string{"v=DKIM1; p=" + p + "; p=" + p}, ReasonUnsupported},
		"v not first":                     {[]string{"k=rsa; v=DKIM1; p=" + p}, ReasonUnsupported},
		"wrong version":                   {[]string{"v=DKIM2; p=" + p}, ReasonUnsupported},
		"upper-case P is not p":           {[]string{"v=DKIM1; k=rsa; P=" + p}, ReasonDifferentKey},
		"upper-case P only":               {[]string{"P=" + p}, ReasonDifferentKey},
		"upper-case V is no version":      {[]string{"V=DKIM2; k=rsa; p=" + p}, ReasonOK},
		"upper-case K is no key type":     {[]string{"v=DKIM1; K=ed25519; p=" + p}, ReasonOK},
		"upper-case T is no testing flag": {[]string{"v=DKIM1; T=y; p=" + p}, ReasonOK},
		"oversized":                       {[]string{"v=DKIM1; p=" + p + "; n=" + strings.Repeat("x", 9000)}, ReasonUnsupported},
	} {
		if got := CheckRecord(tc.records, spki); got != tc.want {
			t.Errorf("%s: got %s want %s", name, got, tc.want)
		}
	}
	if CheckRecord([]string{"v=DKIM1; p="}, nil) == ReasonOK {
		t.Error("empty key matched an empty record")
	}
}

func TestGenerateSealOpenBindsTheRow(t *testing.T) {
	ring := testRing(t)
	material, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	id, app, domain := strings.Repeat("1", 32), strings.Repeat("a", 32), "example.com"
	sealed, err := Seal(ring, id, app, domain, "thinpost1", material.PKCS8)
	if err != nil {
		t.Fatal(err)
	}
	stored := Stored{ID: id, AppID: app, Domain: domain, Selector: "thinpost1", PublicKey: material.SPKI, Sealed: sealed}
	key, err := Open(ring, stored)
	if err != nil || key.N.BitLen() != Bits {
		t.Fatal("open", err)
	}
	if !strings.HasPrefix(RecordValue(material.SPKI), "v=DKIM1; k=rsa; p=") || RecordName("thinpost1", domain) != "thinpost1._domainkey.example.com" {
		t.Fatal("record")
	}
	// Moving the ciphertext to another row (id, app, domain or selector) fails.
	for name, moved := range map[string]Stored{
		"id":       {ID: strings.Repeat("2", 32), AppID: app, Domain: domain, Selector: "thinpost1"},
		"app":      {ID: id, AppID: strings.Repeat("b", 32), Domain: domain, Selector: "thinpost1"},
		"domain":   {ID: id, AppID: app, Domain: "example.org", Selector: "thinpost1"},
		"selector": {ID: id, AppID: app, Domain: domain, Selector: "thinpost2"},
	} {
		moved.PublicKey, moved.Sealed = material.SPKI, sealed
		if _, err := Open(ring, moved); err == nil {
			t.Errorf("ciphertext moved to another %s opened", name)
		}
	}
	// A different public key on the row is refused too.
	other, _ := Generate()
	stored.PublicKey = other.SPKI
	if _, err := Open(ring, stored); err == nil {
		t.Error("key opened against another public key")
	}
	stored.PublicKey, stored.Sealed = material.SPKI, nil
	if _, err := Open(ring, stored); err == nil {
		t.Error("a shredded key opened")
	}
	if _, err := Seal(nil, id, app, domain, "thinpost1", material.PKCS8); err == nil {
		t.Error("sealed without a keyring")
	}
}

func TestValidSelector(t *testing.T) {
	for value, want := range map[string]bool{"thinpost1": true, "a-b": true, "": false, "-a": false, "a-": false, "A": false, "a_b": false, "a.b": false, strings.Repeat("a", 64): false} {
		if ValidSelector(value) != want {
			t.Errorf("%q", value)
		}
	}
}
