package leaseproof

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"net/url"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/controltls"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"
)

func peerContext(t *testing.T, identity string, pub crypto.PublicKey) context.Context {
	t.Helper()
	uri, err := url.Parse(identity)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{URIs: []*url.URL{uri}, PublicKey: pub, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
}
func TestProofBindsLeaseToPeerAndExpires(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		key  crypto.Signer
	}{{"RSA", rsaKey}, {"ECDSA", ecKey}, {"Ed25519", edKey}} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			authority, err := NewAuthority(5 * time.Second)
			if err != nil {
				t.Fatal(err)
			}
			authority.now = func() time.Time { return now }
			identity := "spiffe://thinpost.test/staging/controller/one"
			lease := Lease{WorkerID: "worker", ControllerID: "controller", JournalID: "0123456789abcdef0123456789abcdef", Identity: identity, Epoch: 7, ExpiresUnixMilli: now.Add(time.Minute).UnixMilli()}
			payload, signature, err := Sign(lease, tc.key)
			if err != nil {
				t.Fatal(err)
			}
			open := &controlv1.Open{WorkerId: "worker", ControllerId: "controller", JournalId: "0123456789abcdef0123456789abcdef", Epoch: 7, LeasePayload: payload, LeaseSignature: signature}
			ctx := peerContext(t, identity, tc.key.Public())
			g, err := authority.Authorize(ctx, identity, open)
			if err != nil || !g.Valid() {
				t.Fatal("valid proof rejected")
			}
			other := proto.Clone(open).(*controlv1.Open)
			other.Epoch = 8
			if _, err = authority.Authorize(ctx, identity, other); err == nil {
				t.Fatal("accepted different epoch")
			}
			other = proto.Clone(open).(*controlv1.Open)
			other.JournalId = "ffffffffffffffffffffffffffffffff"
			if _, err = authority.Authorize(ctx, identity, other); err == nil {
				t.Fatal("accepted a different journal identity")
			}
			other = proto.Clone(open).(*controlv1.Open)
			other.LeaseSignature = append([]byte(nil), signature...)
			other.LeaseSignature[0] ^= 1
			if _, err = authority.Authorize(ctx, identity, other); err == nil {
				t.Fatal("accepted forged signature")
			}
			if _, err = authority.Authorize(ctx, "spiffe://thinpost.test/staging/controller/other", open); err == nil {
				t.Fatal("accepted wrong identity")
			}
			now = now.Add(56 * time.Second)
			if g.Valid() {
				t.Fatal("grant survived conservative expiry")
			}
		})
	}
}
func TestProofRejectsExcessiveLifetime(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	identity := "spiffe://thinpost.test/staging/controller/one"
	lease := Lease{WorkerID: "worker", ControllerID: "controller", JournalID: "0123456789abcdef0123456789abcdef", Identity: identity, Epoch: 1, ExpiresUnixMilli: now.Add(10 * time.Minute).UnixMilli()}
	payload, sig, err := Sign(lease, key)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAuthority(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Authorize(peerContext(t, identity, key.Public()), identity, &controlv1.Open{WorkerId: "worker", ControllerId: "controller", JournalId: "0123456789abcdef0123456789abcdef", Epoch: 1, LeasePayload: payload, LeaseSignature: sig}); err == nil {
		t.Fatal("accepted excessive lease lifetime")
	}
}

func TestGrantCannotOutliveVerifiedCertificateChain(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	identity := "spiffe://thinpost.test/staging/controller/one"
	lease := Lease{WorkerID: "worker", ControllerID: "controller", JournalID: "0123456789abcdef0123456789abcdef", Identity: identity, Epoch: 1, ExpiresUnixMilli: now.Add(time.Minute).UnixMilli()}
	payload, sig, err := Sign(lease, key)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := NewAuthority(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	authority.now = func() time.Time { return now }
	uri, err := url.Parse(identity)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{Raw: []byte("leaf"), URIs: []*url.URL{uri}, PublicKey: key.Public(), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(50 * time.Second)}
	root := &x509.Certificate{Raw: []byte("root"), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(30 * time.Second)}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf, root}}}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: state}})
	open := &controlv1.Open{WorkerId: "worker", ControllerId: "controller", JournalId: "0123456789abcdef0123456789abcdef", Epoch: 1, LeasePayload: payload, LeaseSignature: sig}
	g, err := authority.Authorize(ctx, identity, open)
	if err != nil || !g.Valid() {
		t.Fatal("valid certificate chain rejected", err)
	}
	if !g.Deadline().Equal(now.Add(25 * time.Second)) {
		t.Fatal("grant did not use earliest certificate expiry with skew", g.Deadline())
	}
	now = now.Add(25 * time.Second)
	if g.Valid() {
		t.Fatal("grant survived issuing certificate expiry margin")
	}
	if _, err = authority.Authorize(ctx, identity, open); err == nil {
		t.Fatal("renewed lease with expired issuing certificate")
	}
}

func TestCertificateChainValidityAndAlternatives(t *testing.T) {
	now := time.Now()
	leaf := &x509.Certificate{Raw: []byte("leaf"), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Minute)}
	short := &x509.Certificate{NotBefore: now.Add(-time.Hour), NotAfter: now.Add(20 * time.Second)}
	long := &x509.Certificate{NotBefore: now.Add(-time.Hour), NotAfter: now.Add(40 * time.Second)}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf, short}, {leaf, long}}}
	expiry, err := controltls.VerifiedCertificateExpiry(state, now, 5*time.Second)
	if err != nil || !expiry.Equal(long.NotAfter) {
		t.Fatal("valid alternative chain not selected", expiry, err)
	}
	long.NotBefore = now.Add(-time.Second)
	short.NotAfter = now.Add(5 * time.Second)
	if _, err = controltls.VerifiedCertificateExpiry(state, now, 5*time.Second); err == nil {
		t.Fatal("accepted chains outside conservative validity window")
	}
	state.VerifiedChains = [][]*x509.Certificate{{{Raw: []byte("other"), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}}}
	if _, err = controltls.VerifiedCertificateExpiry(state, now, 5*time.Second); err == nil {
		t.Fatal("accepted chain for a different leaf")
	}
}
