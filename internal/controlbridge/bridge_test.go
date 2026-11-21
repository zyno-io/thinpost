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
	"github.com/zyno-io/thinpost/internal/smtpintake"
	"github.com/zyno-io/thinpost/internal/submission"
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
	c := Config{WorkerID: "worker", ControllerID: "controller", JournalID: "0123456789abcdef0123456789abcdef", ControllerIdentity: identity.String(), Target: "10.0.0.1:7443", WorkerDNS: "edge.internal", WorkerIdentity: "spiffe://thinpost.test/worker/a", PrivateCA: x509.NewCertPool(), ClientCertificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, Infrastructure: []netip.Prefix{netip.MustParsePrefix("45.0.1.0/24")}, ActiveBounceKey: "v1", BounceKeys: map[string][]byte{"v1": []byte("0123456789abcdef0123456789abcdef")}, LeaseTTL: 30 * time.Second, ClockMargin: time.Second, PollInterval: time.Second}
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

type fakeSubmission struct{}

func (fakeSubmission) Verify(context.Context, string, string, netip.Addr) smtpintake.AuthResult {
	return smtpintake.AuthTempfail
}
func (fakeSubmission) Submit(context.Context, submission.Fence, smtpintake.Message) smtpintake.Result {
	return smtpintake.Result{Outcome: smtpintake.Tempfail}
}

// The submission intake reaches the session runner, which advertises the
// capability only when it is set.
func TestBridgePassesSubmissionToTheRunner(t *testing.T) {
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
	c := Config{WorkerID: "worker", ControllerID: "controller", JournalID: "0123456789abcdef0123456789abcdef", ControllerIdentity: identity.String(), Target: "10.0.0.1:7443", WorkerDNS: "edge.internal", WorkerIdentity: "spiffe://thinpost.test/worker/a", PrivateCA: x509.NewCertPool(), ClientCertificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, Infrastructure: []netip.Prefix{netip.MustParsePrefix("45.0.1.0/24")}, ActiveBounceKey: "v1", BounceKeys: map[string][]byte{"v1": []byte("0123456789abcdef0123456789abcdef")}, LeaseTTL: 30 * time.Second, ClockMargin: time.Second, PollInterval: time.Second, Recipients: allowRecipients{}}
	repo := &postgres.Repository{DB: &sql.DB{}}
	without, err := New(c, repo, objects{})
	if err != nil {
		t.Fatal(err)
	}
	defer without.Close()
	if without.runner.Submission != nil {
		t.Fatal("submission set without a configured intake")
	}
	c.Submission = fakeSubmission{}
	with, err := New(c, repo, objects{})
	if err != nil {
		t.Fatal(err)
	}
	defer with.Close()
	if with.runner.Submission == nil {
		t.Fatal("the intake did not reach the runner")
	}
}
