package rawmime

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	keyOnce sync.Once
	testRSA *rsa.PrivateKey
)

func testKey(t testing.TB) *rsa.PrivateKey {
	keyOnce.Do(func() {
		var err error
		if testRSA, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return testRSA
}

// txtRecord is the DKIM key record for the test key.
func txtRecord(t testing.TB) string {
	der, err := x509.MarshalPKIXPublicKey(&testKey(t).PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der)
}

func fixture(t testing.TB, name string) []byte {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func verifiedSet(domains ...string) func(string) bool {
	return func(d string) bool {
		for _, v := range domains {
			if d == v {
				return true
			}
		}
		return false
	}
}

var acceptedAt = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func mustScan(t testing.TB, raw string) *Scanned {
	t.Helper()
	s, err := Scan([]byte(raw))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return s
}

// prepared runs Scan, DecideFrom (off mode, example.com verified) and Prepare.
func prepared(t testing.TB, raw string, mailFrom string) ([]byte, PrepareInput) {
	t.Helper()
	s := mustScan(t, raw)
	d, err := DecideFrom(s, mailFrom, Policy{Mode: FromOff}, verifiedSet("example.com"))
	if err != nil {
		t.Fatalf("DecideFrom: %v", err)
	}
	in := PrepareInput{Scan: s, From: d, MessageID: strings.Repeat("a1", 16), Date: acceptedAt, Selector: "sel", Signer: testKey(t)}
	out, err := Prepare([]byte(raw), in)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return out, in
}

func readFixture(name string) ([]byte, error) { return os.ReadFile("testdata/" + name) }
