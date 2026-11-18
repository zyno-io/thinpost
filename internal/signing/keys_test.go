package signing

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"math/big"
	"testing"
)

type domains struct{ active bool }

func (d *domains) SigningDomainActive(context.Context, string, string) (bool, error) {
	return d.active, nil
}

func TestSigningKeySizeBound(t *testing.T) {
	for _, tc := range []struct {
		bits  int
		valid bool
	}{{2047, false}, {2048, true}, {8192, true}, {8193, false}} {
		key := &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: new(big.Int).Lsh(big.NewInt(1), uint(tc.bits-1)), E: 65537}}
		_, err := New(&domains{}, &dns{}, []Entry{{AppID: "0123456789abcdef0123456789abcdef", Domain: "example.com", Selector: "mail", Signer: key}})
		if (err == nil) != tc.valid {
			t.Fatalf("key with %d bits: %v", tc.bits, err)
		}
	}
}

type dns struct {
	records []string
	name    string
}

func (d *dns) LookupTXT(_ context.Context, name string) ([]string, error) {
	d.name = name
	return d.records, nil
}

func TestSigningRequiresAppDomainAndPublishedMatchingDKIMKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	value := base64.StdEncoding.EncodeToString(der)
	domains := &domains{active: true}
	dns := &dns{records: []string{"v=DKIM1; k=rsa; h=sha256; p=" + value}}
	app := "0123456789abcdef0123456789abcdef"
	resolver, err := New(domains, dns, []Entry{{AppID: app, Domain: "example.com", Selector: "mail1", Signer: key}})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.ForSender(context.Background(), app, "Test <sender@example.com>")
	if err != nil || resolved.Domain != "example.com" || dns.name != "mail1._domainkey.example.com" {
		t.Fatal("valid DKIM rejected", err)
	}
	if _, err = resolver.ForSender(context.Background(), "abcdef0123456789abcdef0123456789", "sender@example.com"); err == nil {
		t.Fatal("cross-app key allowed")
	}
	domains.active = false
	if _, err = resolver.ForSender(context.Background(), app, "sender@example.com"); err == nil {
		t.Fatal("inactive domain allowed")
	}
	domains.active = true
	for _, record := range []string{
		"v=DKIM1; k=rsa; p=",
		"v=DKIM1; k=rsa; p=" + value + "; t=y",
		"v=DKIM1; k=rsa; p=" + value + "; h=sha1",
		"v=DKIM1; k=rsa; p=" + value + "; s=other",
		"v=DKIM1; k=rsa; p=" + value + "; p=" + value,
		"k=rsa; v=DKIM1; p=" + value,
	} {
		dns.records = []string{record}
		if _, err = resolver.ForSender(context.Background(), app, "sender@example.com"); err == nil {
			t.Error("invalid DKIM record accepted")
		}
	}
	dns.records = []string{"v=DKIM1; p=" + value, "v=DKIM1; p=" + value}
	if _, err = resolver.ForSender(context.Background(), app, "sender@example.com"); err == nil {
		t.Fatal("multiple DKIM records accepted")
	}
}
