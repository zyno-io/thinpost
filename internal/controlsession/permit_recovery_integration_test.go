package controlsession

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/controllease"
	"github.com/zyno-io/thinpost/internal/controltls"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/edgesession"
	"github.com/zyno-io/thinpost/internal/edgeworker"
	"github.com/zyno-io/thinpost/internal/journal"
	"github.com/zyno-io/thinpost/internal/leaseproof"
	"github.com/zyno-io/thinpost/internal/postgres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
)

type withheldCommittedPermit struct {
	repo      *postgres.Repository
	committed chan struct{}
}

func (p *withheldCommittedPermit) AuthorizeDeliveryStart(ctx context.Context, lease postgres.ControllerLease, attempt, deliveryID, session, journalID string, descriptor [32]byte, cutoff int64) error {
	if err := p.repo.AuthorizeDeliveryStart(ctx, lease, attempt, deliveryID, session, journalID, descriptor, cutoff); err != nil {
		return err
	}
	close(p.committed)
	<-ctx.Done()
	return ctx.Err()
}

type oneClaimWork struct {
	repo    *postgres.Repository
	claimed atomic.Bool
}

func (w *oneClaimWork) Next(ctx context.Context, lease postgres.ControllerLease, caps delivery.EdgeCapabilities) (*controlv1.DeliveryStart, []byte, error) {
	if w.claimed.Swap(true) {
		return nil, nil, delivery.ErrNoWork
	}
	claim, err := w.repo.ClaimDelivery(ctx, lease, "00000000000000000000000000000003", delivery.EdgeCapabilities{Sources: []delivery.Source{{Address: netip.MustParseAddr("45.0.1.22"), Hostname: "bounce.example.com"}}})
	if err != nil {
		return nil, nil, err
	}
	mime := []byte("Subject: synthetic\r\n\r\nbody\r\n")
	digest := sha256.Sum256(mime)
	return &controlv1.DeliveryStart{AttemptId: claim.AttemptID, DeliveryId: claim.DeliveryID, Epoch: lease.Epoch, MxName: "mx.example", DestinationIp: "8.8.8.8", EnvelopeFrom: "bounce@example.com", Recipient: "to@example.net", ExpectedSize: uint64(len(mime)), Sha256: digest[:]}, mime, nil
}

type neverSMTP struct{ calls atomic.Int32 }

func (d *neverSMTP) Dial(context.Context, netip.Addr, netip.Addr, string, string) (net.Conn, error) {
	d.calls.Add(1)
	return nil, errors.New("unexpected SMTP dial")
}

type allowFixtureRecipient struct{}

func (allowFixtureRecipient) Allows(string) bool { return true }

// Uses the production 30-second wait, an actual PostgreSQL permit commit,
// real control and Edge session implementations, mTLS, and a real journal.
// The S3 store and public SMTP path are deliberately not involved.
func TestCommittedPermitLostBeforeEdgeTimeout(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "permit_recovery_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = postgres.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	const generation = "11111111111111111111111111111111"
	const app = "00000000000000000000000000000001"
	const message = "00000000000000000000000000000002"
	const deliveryID = "00000000000000000000000000000004"
	if _, err = db.ExecContext(ctx, `UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, generation); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'fixture')`, app); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'source/fixture',false,'prepared')`, message, app); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,'final/fixture')`, deliveryID, message); err != nil {
		t.Fatal(err)
	}
	repo := &postgres.Repository{DB: db, Generation: generation}
	roots, clientCert, serverCert := certificates(t)
	const controllerID = "spiffe://thinpost.test/staging/controller/service"
	const workerID = "spiffe://thinpost.test/staging/worker/one"
	serverTLS, err := controltls.Server(roots, serverCert, []string{controllerID})
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := controltls.Client(roots, clientCert, "edge.internal", workerID)
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	journalID, err := j.Identity()
	if err != nil {
		t.Fatal(err)
	}
	authority, err := leaseproof.NewAuthority(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	dialer := &neverSMTP{}
	worker := &edgeworker.Worker{Journal: j, Dialer: dialer, HeloName: "smtp.sender.example"}
	edge, err := edgesession.New("worker", []string{controllerID}, j, worker, authority, 2)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	controlv1.RegisterEdgeControlServer(server, edge)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	connection, err := grpc.NewClient("passthrough:///edge.internal", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }), grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	signer, ok := clientCert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("unexpected client signing key")
	}
	work := &oneClaimWork{repo: repo}
	gate := &withheldCommittedPermit{repo: repo, committed: make(chan struct{})}
	runner := &Runner{WorkerID: "worker", ControllerID: "controller", JournalID: journalID, Issuer: &controllease.Issuer{Repository: repo, Signer: signer, Identity: controllerID, JournalID: journalID}, Results: repo, Starts: gate, Reconciler: repo, Work: work, LeaseTTL: 30 * time.Second, ClockMargin: 6 * time.Second, PollInterval: 100 * time.Millisecond, Connect: func(ctx context.Context) (Stream, error) {
		return controlv1.NewEdgeControlClient(connection).Connect(ctx)
	}}
	firstDone := make(chan error, 1)
	go func() { firstDone <- runner.Run(ctx) }()
	select {
	case <-gate.committed:
	case <-ctx.Done():
		t.Fatal("permit was not committed")
	}
	var authorized bool
	if err = db.QueryRowContext(ctx, `SELECT start_authorized FROM delivery_attempts WHERE id=$1`, "00000000000000000000000000000003").Scan(&authorized); err != nil || !authorized {
		t.Fatal("permit not durably committed", err)
	}
	select {
	case err = <-firstDone:
		if err == nil {
			t.Fatal("session survived permit timeout")
		}
	case <-ctx.Done():
		t.Fatal("session did not time out")
	}
	if dialer.calls.Load() != 0 {
		t.Fatal("SMTP dial occurred before the permit arrived")
	}
	items, err := j.List()
	if err != nil || len(items) != 1 || items[0].Outcome != journal.Rejected {
		t.Fatal("unsent outcome was not journaled", items, err)
	}
	runner.Starts = repo
	nextCtx, stopNext := context.WithCancel(ctx)
	nextDone := make(chan error, 1)
	go func() { nextDone <- runner.Run(nextCtx) }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		var state, outcome string
		err = db.QueryRowContext(ctx, `SELECT d.state,COALESCE(a.outcome,'') FROM deliveries d JOIN delivery_attempts a ON a.delivery_id=d.id WHERE d.id=$1`, deliveryID).Scan(&state, &outcome)
		if err != nil {
			stopNext()
			t.Fatal(err)
		}
		if state == "queued" && outcome == "rejected" {
			break
		}
		select {
		case err = <-nextDone:
			stopNext()
			t.Fatal("replay session ended before committing outcome", err)
		case <-ctx.Done():
			stopNext()
			t.Fatal("replay did not commit")
		case <-ticker.C:
		}
	}
	for {
		items, err = j.List()
		if err != nil {
			stopNext()
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		select {
		case err = <-nextDone:
			stopNext()
			t.Fatal("replay session ended before acknowledgement", err)
		case <-ctx.Done():
			stopNext()
			t.Fatal("replay was not acknowledged")
		case <-ticker.C:
		}
	}
	stopNext()
	<-nextDone
	if dialer.calls.Load() != 0 {
		t.Fatal("replay unexpectedly dialed SMTP")
	}
}
