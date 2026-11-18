package controllease

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/leaseproof"
	"github.com/zyno-io/thinpost/internal/postgres"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

type fakeRepo struct {
	lease postgres.ControllerLease
	err   error
}

func (r fakeRepo) AcquireControllerLease(context.Context, string, string, time.Duration) (postgres.ControllerLease, error) {
	return r.lease, r.err
}
func (r fakeRepo) RenewControllerLease(context.Context, postgres.ControllerLease, time.Duration) (postgres.ControllerLease, error) {
	return r.lease, r.err
}
func (r fakeRepo) ReleaseControllerLease(context.Context, postgres.ControllerLease) (bool, error) {
	return r.err == nil, r.err
}

type releaseRecorder struct {
	fakeRepo
	released   postgres.ControllerLease
	cleanupErr error
}

func (r *releaseRecorder) ReleaseControllerLease(ctx context.Context, lease postgres.ControllerLease) (bool, error) {
	r.released = lease
	r.cleanupErr = ctx.Err()
	return true, nil
}
func TestIssueOnlyAfterDatabaseLease(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := "spiffe://thinpost.test/staging/controller/one"
	lease := postgres.ControllerLease{WorkerID: "worker", ControllerID: "controller", Epoch: 2, ExpiresAt: time.Now().Add(time.Minute)}
	issuer := &Issuer{Repository: fakeRepo{lease: lease}, Signer: key, JournalID: "0123456789abcdef0123456789abcdef", Identity: identity}
	got, payload, sig, err := issuer.Acquire(context.Background(), "worker", "controller", time.Minute)
	if err != nil || got.Epoch != 2 {
		t.Fatal("lease not issued")
	}
	uri, _ := url.Parse(identity)
	cert := &x509.Certificate{URIs: []*url.URL{uri}, PublicKey: key.Public(), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
	authority, err := leaseproof.NewAuthority(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := authority.Authorize(ctx, identity, &controlv1.Open{WorkerId: "worker", ControllerId: "controller", JournalId: "0123456789abcdef0123456789abcdef", Epoch: 2, LeasePayload: payload, LeaseSignature: sig})
	if err != nil || !grant.Valid() {
		t.Fatal("edge rejected database-backed proof")
	}
	issuer.Repository = fakeRepo{err: errors.New("lease held")}
	if _, payload, _, err = issuer.Acquire(context.Background(), "worker", "controller", time.Minute); err == nil || payload != nil {
		t.Fatal("proof issued without database lease")
	}
}

func TestAcquireReleasesUnsignedLease(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	lease := postgres.ControllerLease{WorkerID: "worker", ControllerID: "controller", Epoch: 2, ExpiresAt: time.Now().Add(time.Minute)}
	repository := &releaseRecorder{fakeRepo: fakeRepo{lease: lease}}
	issuer := &Issuer{Repository: repository, Signer: key, JournalID: "0123456789abcdef0123456789abcdef", Identity: strings.Repeat("x", 513)}
	if _, _, _, err = issuer.Acquire(context.Background(), "worker", "controller", time.Minute); err == nil {
		t.Fatal("invalid proof signed")
	}
	if repository.released.Epoch != lease.Epoch || repository.released.WorkerID != lease.WorkerID || repository.cleanupErr != nil {
		t.Fatal("unsigned lease was not released with a live cleanup context")
	}
}
