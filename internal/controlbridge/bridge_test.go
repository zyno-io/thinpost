package controlbridge

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"math/big"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/postgres"
)

type objects struct{}
type allowRecipients struct{}

func (allowRecipients) Allows(string) bool { return true }

func (objects) Get(context.Context, string, int64) ([]byte, error) { return nil, nil }
func (objects) PutMIME(context.Context, string, []byte) error      { return nil }

func TestBridgeRejectsPublicTargetsAndWrongControlIdentity(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := url.Parse("spiffe://thinpost.test/controller/a")
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), URIs: []*url.URL{identity}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{WorkerID: "worker", ControllerID: "controller", JournalID: "0123456789abcdef0123456789abcdef", ControllerIdentity: identity.String(), Target: "10.0.0.1:7443", WorkerDNS: "edge.internal", WorkerIdentity: "spiffe://thinpost.test/worker/a", PrivateCA: x509.NewCertPool(), ClientCertificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, Source: netip.MustParseAddr("45.0.1.22"), Infrastructure: []netip.Prefix{netip.MustParsePrefix("45.0.1.0/24")}, BounceDomain: "bounce.example.com", ActiveBounceKey: "v1", BounceKeys: map[string][]byte{"v1": []byte("0123456789abcdef0123456789abcdef")}, LeaseTTL: 30 * time.Second, ClockMargin: time.Second, PollInterval: time.Second}
	c.Recipients = allowRecipients{}
	repo := &postgres.Repository{DB: &sql.DB{}}
	bridge, err := New(c, repo, objects{})
	if err != nil {
		t.Fatal(err)
	}
	withoutPolicy := c
	withoutPolicy.Recipients = nil
	if bridge, err := New(withoutPolicy, repo, objects{}); err == nil {
		_ = bridge.Close()
		t.Fatal("accepted a bridge without a recipient policy")
	}
	badJournal := c
	badJournal.JournalID = "not-a-journal-id"
	if bridge, err := New(badJournal, repo, objects{}); err == nil {
		_ = bridge.Close()
		t.Fatal("accepted invalid journal identity")
	}
	if err = bridge.Close(); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"45.0.1.22:7443", "edge.internal:7443", "10.0.0.1:invalid", "0.0.0.0:7443"} {
		bad := c
		bad.Target = target
		if bridge, err = New(bad, repo, objects{}); err == nil {
			_ = bridge.Close()
			t.Errorf("accepted target %s", target)
		}
	}
	bad := c
	bad.ControllerIdentity = "spiffe://thinpost.test/controller/other"
	if bridge, err = New(bad, repo, objects{}); err == nil {
		_ = bridge.Close()
		t.Fatal("accepted mismatched controller identity")
	}
	bad = c
	bad.BounceKeys = map[string][]byte{"v1": []byte("short")}
	if bridge, err = New(bad, repo, objects{}); err == nil {
		_ = bridge.Close()
		t.Fatal("accepted weak bounce key")
	}
}
