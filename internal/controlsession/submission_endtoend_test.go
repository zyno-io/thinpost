package controlsession

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/bounce"
	"github.com/zyno-io/thinpost/internal/bounceaddress"
	"github.com/zyno-io/thinpost/internal/controllease"
	"github.com/zyno-io/thinpost/internal/controltls"
	"github.com/zyno-io/thinpost/internal/dispatch"
	"github.com/zyno-io/thinpost/internal/edgesession"
	"github.com/zyno-io/thinpost/internal/edgeworker"
	"github.com/zyno-io/thinpost/internal/journal"
	"github.com/zyno-io/thinpost/internal/leaseproof"
	"github.com/zyno-io/thinpost/internal/mailroute"
	"github.com/zyno-io/thinpost/internal/objectstore"
	"github.com/zyno-io/thinpost/internal/postgres"
	"github.com/zyno-io/thinpost/internal/preparation"
	"github.com/zyno-io/thinpost/internal/providerid"
	"github.com/zyno-io/thinpost/internal/recipientpolicy"
	"github.com/zyno-io/thinpost/internal/signing"
	"github.com/zyno-io/thinpost/internal/smtpintake"
	"github.com/zyno-io/thinpost/internal/submission"
	"github.com/zyno-io/thinpost/internal/tracking"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
)

// commitGate wraps the real repository's Commit: it can hold a commit, never
// replace it. It lets a test cut the session while a commit is in flight.
type commitGate struct {
	*postgres.Repository
	hold    atomic.Bool
	entered chan struct{}
	release chan struct{}
	commits atomic.Int32
}

func (g *commitGate) Commit(ctx context.Context, record acceptance.Record) (submission.Receipt, error) {
	g.commits.Add(1)
	if g.hold.Load() {
		select {
		case g.entered <- struct{}{}:
		default:
		}
		select {
		case <-g.release:
		case <-ctx.Done():
		}
	}
	return g.Repository.Commit(ctx, record)
}

// lossySubmission models a lost acknowledgement: the intake runs (or not) and
// the client is told "temporary failure" either way.
type lossySubmission struct {
	inner Submission
	// mode: 0 pass through; 1 drop before commit (nothing runs); 2 drop after
	// commit (the intake runs, the answer is replaced).
	mode atomic.Int32
}

func (l *lossySubmission) Verify(ctx context.Context, id, secret string, ip netip.Addr) smtpintake.AuthResult {
	return l.inner.Verify(ctx, id, secret, ip)
}

func (l *lossySubmission) Submit(ctx context.Context, fence submission.Fence, m smtpintake.Message) smtpintake.Result {
	switch l.mode.Load() {
	case 1:
		return smtpintake.Result{Outcome: smtpintake.Tempfail}
	case 2:
		l.inner.Submit(ctx, fence, m)
		return smtpintake.Result{Outcome: smtpintake.Tempfail}
	}
	return l.inner.Submit(ctx, fence, m)
}

// e2eDialer is the remote MX: it accepts the message after verifying the DKIM
// signature and hands the delivered MIME to the test.
type e2eDialer struct {
	certificate tls.Certificate
	dkimRecord  string
	received    chan []byte
	failures    chan error
}

func (d *e2eDialer) Dial(_ context.Context, _, _ netip.Addr, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		err := func() error {
			reader := bufio.NewReader(server)
			expect := func(r *bufio.Reader, prefix string) error {
				line, err := r.ReadString('\n')
				if err != nil {
					return err
				}
				if !strings.HasPrefix(line, prefix) {
					return fmt.Errorf("expected %q, got %q", prefix, line)
				}
				return nil
			}
			if _, err := fmt.Fprint(server, "220 ready\r\n"); err != nil {
				return err
			}
			if err := expect(reader, "EHLO "); err != nil {
				return err
			}
			if _, err := fmt.Fprint(server, "250-edge.internal\r\n250 STARTTLS\r\n"); err != nil {
				return err
			}
			if err := expect(reader, "STARTTLS"); err != nil {
				return err
			}
			if _, err := fmt.Fprint(server, "220 begin TLS\r\n"); err != nil {
				return err
			}
			secured := tls.Server(server, &tls.Config{Certificates: []tls.Certificate{d.certificate}, MinVersion: tls.VersionTLS12})
			if err := secured.Handshake(); err != nil {
				return err
			}
			reader = bufio.NewReader(secured)
			if err := expect(reader, "EHLO "); err != nil {
				return err
			}
			if _, err := fmt.Fprint(secured, "250 ready\r\n"); err != nil {
				return err
			}
			for _, cmd := range []string{"MAIL FROM:", "RCPT TO:", "DATA"} {
				if err := expect(reader, cmd); err != nil {
					return err
				}
				reply := "250 ok\r\n"
				if cmd == "DATA" {
					reply = "354 send\r\n"
				}
				if _, err := fmt.Fprint(secured, reply); err != nil {
					return err
				}
			}
			body, err := textproto.NewReader(reader).ReadDotBytes()
			if err != nil {
				return err
			}
			wire := bytes.ReplaceAll(body, []byte("\n"), []byte("\r\n"))
			results, verifyErr := dkim.VerifyWithOptions(bytes.NewReader(wire), &dkim.VerifyOptions{LookupTXT: func(name string) ([]string, error) {
				if name != "thinpost1._domainkey.example.com" {
					return nil, errors.New("unexpected signing domain")
				}
				return []string{d.dkimRecord}, nil
			}})
			if verifyErr != nil || len(results) != 1 || results[0].Err != nil {
				return errors.New("delivered MIME failed DKIM verification")
			}
			d.received <- wire
			if _, err = fmt.Fprint(secured, "250 accepted\r\n"); err != nil {
				return err
			}
			if err = expect(reader, "QUIT"); err != nil {
				return err
			}
			_, err = fmt.Fprint(secured, "221 bye\r\n")
			return err
		}()
		if err != nil {
			select {
			case d.failures <- err:
			default:
			}
		}
	}()
	return client, nil
}

// subRig is the whole path: SMTP listeners on the edge, the mTLS control
// stream, the control intake, the encrypted object store, PostgreSQL,
// preparation, signing and delivery.
type subRig struct {
	t        *testing.T
	ctx      context.Context
	db       *sql.DB
	repo     *postgres.Repository
	gate     *commitGate
	edge     *edgesession.Server
	roots    *x509.CertPool
	objects  *objectstore.EncryptedStore
	preparer *preparation.Service
	lossy    *lossySubmission
	app      string
	cred     string
	secret   string
	address  string // implicit-TLS submission listener (465)
	starttls string // STARTTLS listener (587)
	received chan []byte
	failures chan error
	started  chan *controlv1.DeliveryStart

	// tracking is the visit handler of a rig made with rigOptions.tracking.
	tracking *tracking.Handler

	start func(controller string, sub Submission) (stop func())
}

type rigOptions struct {
	// allowlist restricts recipients (control-side, as in production).
	allowlist []string
	// withoutIntake runs a control that does not offer submission/1.
	withoutIntake bool
	// tracking turns on the tracking runtime: link keys, the mapping store and
	// the app defaults, as a deployment with THINPOST_TRACKING_ORIGIN has.
	tracking bool
}

func newSubmissionRig(t *testing.T, opts rigOptions) *subRig {
	t.Helper()
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	u, err := validatePipelineFixtureURL(dsn, os.Getenv("THINPOST_TEST_DISPOSABLE_DB") == "1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "submission_e2e_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = postgres.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	runtimeDB, cleanupRuntime := restrictedPipelineRuntime(t, ctx, db, u)
	t.Cleanup(cleanupRuntime)
	const generation = "00000000000000000000000000000ccc"
	if _, err = db.ExecContext(ctx, `UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, generation); err != nil {
		t.Fatal(err)
	}
	r := &subRig{t: t, ctx: ctx, db: db, app: "00000000000000000000000000000001", received: make(chan []byte, 16), failures: make(chan error, 16), started: make(chan *controlv1.DeliveryStart, 16)}
	r.exec(`INSERT INTO apps(id,name,recipient_daily_limit,pending_message_limit,submission_rate_per_minute,submission_burst,submission_tokens) VALUES ($1,'test',1000,1000,60000,100,6000000000)`, r.app)
	r.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'example.com',true,now())`, r.app)

	keyringPath := filepath.Join(t.TempDir(), "object-keyring.json")
	var encryptionKey [32]byte
	if _, err = rand.Read(encryptionKey[:]); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyringPath, []byte(fmt.Sprintf(`{"active":"fixture","keys":{"fixture":%q}}`, base64.StdEncoding.EncodeToString(encryptionKey[:]))), 0o600); err != nil {
		t.Fatal(err)
	}
	keyring, err := objectstore.LoadKeyring(keyringPath)
	if err != nil {
		t.Fatal(err)
	}
	r.objects, err = objectstore.NewEncrypted(pipelineRawObjects(t), keyring)
	if err != nil {
		t.Fatal(err)
	}
	r.repo = &postgres.Repository{DB: runtimeDB, Generation: generation, Objects: r.objects, DKIMKeys: keyring}
	r.gate = &commitGate{Repository: r.repo, entered: make(chan struct{}, 1), release: make(chan struct{})}

	env := map[string]string{"THINPOST_RECIPIENT_POLICY": "unrestricted"}
	if len(opts.allowlist) > 0 {
		path := filepath.Join(t.TempDir(), "recipients.txt")
		if err = os.WriteFile(path, []byte(strings.Join(opts.allowlist, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		env = map[string]string{"THINPOST_RECIPIENT_POLICY": "allowlist", "THINPOST_RECIPIENT_ALLOWLIST_FILE": path}
	}
	policy, err := recipientpolicy.Load(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	acceptor, err := acceptance.NewWithFingerprintKeys(r.gate, r.objects, [][]byte{[]byte("0123456789abcdef0123456789abcdef")}, policy)
	if err != nil {
		t.Fatal(err)
	}
	dkimRecord := activateTestDKIMKey(t, ctx, r.repo, r.app, "example.com")
	keys, err := signing.New(r.repo, keyring, integrationDNS{record: dkimRecord})
	if err != nil {
		t.Fatal(err)
	}
	r.preparer = &preparation.Service{Repository: r.repo, Objects: r.objects, Keys: keys}
	if opts.tracking {
		linkKeys, linkErr := tracking.NewLinkKeys([]byte("0123456789abcdef0123456789abcdef"), nil)
		if linkErr != nil {
			t.Fatal(linkErr)
		}
		protected, storeErr := tracking.NewProtectedStore(r.objects)
		if storeErr != nil {
			t.Fatal(storeErr)
		}
		acceptor.ApplyTrackingDefaults = true
		r.preparer.Tracking = &preparation.TrackingConfig{Origin: "https://tracking.example.com", Links: linkKeys, EventLimit: 10, Objects: protected}
		r.tracking = &tracking.Handler{Links: linkKeys, Visitor: &tracking.Service{Repository: r.repo, Objects: protected}}
	}
	intake := &smtpintake.Service{Repository: r.repo, Acceptance: acceptor, Signing: keys, Replay: &smtpintake.ReplaySecret{Store: r.repo, Keys: keyring}}
	r.lossy = &lossySubmission{inner: intake}
	issued, err := r.repo.IssueSMTPCredential(ctx, "operator", r.app, "printer")
	if err != nil {
		t.Fatal(err)
	}
	r.cred, r.secret = issued.Username, issued.Password

	roots, clientCert, serverCert := certificates(t)
	r.roots = roots
	controllerIdentity := "spiffe://thinpost.test/staging/controller/service"
	workerIdentity := "spiffe://thinpost.test/staging/worker/one"
	serverTLS, err := controltls.Server(roots, serverCert, []string{controllerIdentity})
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := controltls.Client(roots, clientCert, "edge.internal", workerIdentity)
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(filepath.Join(t.TempDir(), "journal.db"), 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	journalID, err := j.Identity()
	if err != nil {
		t.Fatal(err)
	}
	authority, err := leaseproof.NewAuthority(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	dialer := &e2eDialer{certificate: serverCert, dkimRecord: dkimRecord, received: r.received, failures: r.failures}
	worker := &edgeworker.Worker{Journal: j, Dialer: dialer, HeloName: "smtp.sender.example", Roots: roots, Sources: []netip.Addr{netip.MustParseAddr("45.0.1.22")}, SourceHostnames: []string{"bounce.example.com"}}
	r.edge, err = edgesession.New("worker", []string{controllerIdentity}, j, integrationExecutor{worker: worker, started: r.started}, authority, 2)
	if err != nil {
		t.Fatal(err)
	}
	r.edge.Advertise([]string{"45.0.1.22"}, []string{"bounce.example.com"})
	if err = r.edge.EnableSubmission(587, 465); err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)), grpc.MaxRecvMsgSize(128<<10), grpc.MaxSendMsgSize(128<<10))
	controlv1.RegisterEdgeControlServer(server, r.edge)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///edge.internal", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }), grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	signer, ok := clientCert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("unexpected key")
	}

	// Delivery: the prepared message goes out through the edge worker to the
	// fake MX.
	policyAll := &integrationSwitchPolicy{}
	policyAll.allowed.Store(true)
	envelopes := &bounceaddress.Service{Repository: r.repo, ActiveVersion: "v1", Keys: map[string][]byte{"v1": []byte("0123456789abcdef0123456789abcdef")}}
	work := &dispatch.Provider{Repository: r.repo, Objects: r.objects, Routes: &mailroute.Resolver{DNS: &integrationMXDNS{}, Filter: integrationMXFilter{}}, Envelopes: envelopes, Recipients: policyAll}
	r.start = func(controller string, sub Submission) func() {
		runner := &Runner{WorkerID: "worker", ControllerID: controller, JournalID: journalID, Issuer: &controllease.Issuer{Repository: r.repo, Signer: signer, Identity: controllerIdentity, JournalID: journalID},
			Results: r.repo, Starts: r.repo, Reconciler: r.repo, Work: work, LeaseTTL: 30 * time.Second, ClockMargin: 6 * time.Second, PollInterval: 100 * time.Millisecond,
			Connect: func(ctx context.Context) (Stream, error) { return controlv1.NewEdgeControlClient(conn).Connect(ctx) }}
		if sub != nil {
			runner.Submission = sub
		}
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- runner.Run(runCtx) }()
		return func() {
			stop()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("the control session did not stop")
			}
		}
	}

	// The submission listeners, as the edge builds them.
	servers, err := bounce.NewSubmissionServers(bounce.SubmissionConfig{Submission: r.edge.SMTPSubmission()}, []bounce.SubmissionListener{
		{Hostname: "edge.internal", TLS: &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}, Port: 587},
		{Hostname: "edge.internal", TLS: &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}, Port: 465},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, server := range servers {
		raw, listenErr := net.Listen("tcp", "127.0.0.1:0")
		if listenErr != nil {
			t.Fatal(listenErr)
		}
		serve := bounce.ClampListener(raw)
		if i == 1 {
			serve = tls.NewListener(serve, server.TLSConfig)
			r.address = raw.Addr().String()
		} else {
			r.starttls = raw.Addr().String()
		}
		go func() { _ = server.Serve(serve) }()
		t.Cleanup(func() { _ = server.Close() })
	}
	if opts.withoutIntake {
		stop := r.start("controller", nil)
		t.Cleanup(stop)
	}
	return r
}

func (r *subRig) exec(query string, args ...any) {
	r.t.Helper()
	if _, err := r.db.ExecContext(r.ctx, query, args...); err != nil {
		r.t.Fatal(query, err)
	}
}

func (r *subRig) count(query string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.db.QueryRowContext(r.ctx, query, args...).Scan(&n); err != nil {
		r.t.Fatal(query, err)
	}
	return n
}

func (r *subRig) waitAvailable() {
	r.t.Helper()
	eventually(r.t, "the submission session", func() bool { return r.edge.SMTPSubmission().Available() })
}

// e2eSMTP is a small SMTP client that keeps the reply text.
type e2eSMTP struct {
	t    *testing.T
	conn net.Conn
	r    *textproto.Reader
}

func (r *subRig) dial() *e2eSMTP {
	r.t.Helper()
	conn, err := tls.Dial("tcp", r.address, &tls.Config{RootCAs: r.roots, ServerName: "edge.internal", MinVersion: tls.VersionTLS12})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(90 * time.Second))
	c := &e2eSMTP{t: r.t, conn: conn, r: textproto.NewReader(bufio.NewReader(conn))}
	if code, _ := c.reply(); code != 220 {
		r.t.Fatalf("greeting %d", code)
	}
	if code, msg := c.cmd("EHLO client.example.net"); code != 250 || !strings.Contains(msg, "AUTH") || strings.Contains(msg, "CHUNKING") {
		r.t.Fatalf("EHLO: %d %q", code, msg)
	}
	return c
}

func (c *e2eSMTP) reply() (int, string) {
	code, msg, err := c.r.ReadResponse(0)
	if err != nil {
		return 0, err.Error()
	}
	return code, msg
}

func (c *e2eSMTP) cmd(line string) (int, string) {
	c.t.Helper()
	if _, err := c.conn.Write([]byte(line + "\r\n")); err != nil {
		c.t.Fatal(err)
	}
	return c.reply()
}

func (c *e2eSMTP) auth(user, pass string) (int, string) {
	return c.cmd("AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00"+user+"\x00"+pass)))
}

// send runs one transaction and returns the reply of the first command that
// failed, or of the DATA terminator.
func (c *e2eSMTP) send(from string, rcpts []string, raw string) (int, string) {
	c.t.Helper()
	if code, msg := c.cmd("MAIL FROM:<" + from + ">"); code != 250 {
		return code, msg
	}
	for _, rcpt := range rcpts {
		if code, msg := c.cmd("RCPT TO:<" + rcpt + ">"); code != 250 {
			return code, msg
		}
	}
	if code, msg := c.cmd("DATA"); code != 354 {
		return code, msg
	}
	var wire strings.Builder
	atStart := true
	for i := 0; i < len(raw); i++ {
		if atStart && raw[i] == '.' {
			wire.WriteByte('.')
		}
		wire.WriteByte(raw[i])
		atStart = raw[i] == '\n'
	}
	wire.WriteString(".\r\n")
	if _, err := c.conn.Write([]byte(wire.String())); err != nil {
		c.t.Fatal(err)
	}
	return c.reply()
}

var queuedRe = regexp.MustCompile(`OK: (queued|held) as ([0-9a-f]{32})$`)

func queuedID(t *testing.T, code int, msg string) string {
	t.Helper()
	m := queuedRe.FindStringSubmatch(msg)
	if code != 250 || m == nil {
		t.Fatalf("expected 250 with a message ID, got %d %q", code, msg)
	}
	return m[2]
}

const (
	e2eFrom = "sender@example.com"
	e2eRcpt = "recipient@example.net"
)

func e2eMessage(id, subject string, extra ...string) string {
	lines := []string{"From: Printer <" + e2eFrom + ">", "To: " + e2eRcpt, "Subject: " + subject}
	if id != "" {
		lines = append(lines, "Message-ID: <"+id+"@client.example.net>")
	}
	lines = append(lines, extra...)
	return strings.Join(lines, "\r\n") + "\r\n\r\nHello over SMTP 5559\r\n"
}

// messageIDsBySubject returns the IDs of the stored messages with a subject.
func (r *subRig) messageIDsBySubject(subject string) []string {
	r.t.Helper()
	rows, err := r.db.QueryContext(r.ctx, `SELECT id FROM messages WHERE subject=$1 ORDER BY accepted_at`, subject)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			r.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// TestSubmissionPipelineWithPostgres: AUTH, DATA and 250 with the ID, then the
// source object, preparation (header surgery and DKIM) and delivery.
func TestSubmissionPipelineWithPostgres(t *testing.T) {
	r := newSubmissionRig(t, rigOptions{})
	stop := r.start("controller", r.lossy)
	defer stop()
	r.waitAvailable()

	c := r.dial()
	if code, _ := c.auth(r.cred, "tps_"+strings.Repeat("A", 43)); code != 535 {
		t.Fatalf("a wrong secret: %d", code)
	}
	if code, msg := c.auth(r.cred, r.secret); code != 235 {
		t.Fatalf("AUTH: %d %q", code, msg)
	}
	raw := e2eMessage("client-1", "SMTP e2e", "Bcc: hidden@example.org",
		"Received: from marker.example.net by relay.example.net; Mon, 01 Jan 2024 00:00:00 +0000", "Date: Mon, 01 Jan 2024 00:00:00 +0000")
	code, msg := c.send(e2eFrom, []string{e2eRcpt}, raw)
	id := queuedID(t, code, msg)
	if !strings.Contains(msg, "queued as") {
		t.Fatalf("reply %q", msg)
	}
	var credential, original sql.NullString
	if err := r.db.QueryRowContext(r.ctx, `SELECT smtp_credential_id,original_from_address FROM messages WHERE id=$1`, id).Scan(&credential, &original); err != nil || credential.String != r.cred {
		t.Fatal("the message row does not carry the credential", err)
	}
	if err := r.preparer.PrepareNext(r.ctx); err != nil {
		t.Fatal(err)
	}
	var delivered []byte
	select {
	case delivered = <-r.received:
	case err := <-r.failures:
		t.Fatal("the remote MX refused the message:", err)
	case <-r.ctx.Done():
		t.Fatal("nothing was delivered")
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(delivered))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Header.Get("Bcc") != "" {
		t.Fatal("the Bcc header was delivered")
	}
	if strings.Contains(string(delivered), "marker.example.net") {
		t.Fatal("the client's Received header was delivered")
	}
	var deliveryMessageID string
	if err = r.db.QueryRowContext(r.ctx, `SELECT message_id FROM deliveries`).Scan(&deliveryMessageID); err != nil {
		t.Fatal(err)
	}
	if got, want := parsed.Header.Get("Message-ID"), providerid.MessageIDHeader(deliveryMessageID, "example.com"); got != want || strings.Contains(got, "client-1") {
		t.Fatalf("Message-ID %q, want %q", got, want)
	}
	if !strings.HasPrefix(string(delivered), "DKIM-Signature:") || !strings.Contains(string(delivered), "Hello over SMTP 5559") {
		t.Fatal("the delivered MIME is not signed or lost its body")
	}
	// SMTP mail is stored as a recipe over the stored source; the bytes the MX
	// got were rebuilt from it (dispatch checks their size and SHA-256).
	if r.count(`SELECT count(*) FROM deliveries WHERE prepared_format=2`) != 1 {
		t.Fatal("the SMTP delivery was not prepared as a recipe")
	}
	eventually(t, "the delivery to be accepted", func() bool {
		return r.count(`SELECT count(*) FROM deliveries WHERE state='accepted'`) == 1
	})
	select {
	case err = <-r.failures:
		t.Fatal(err)
	default:
	}

	// A message that asks for a hold is acknowledged as held and not prepared.
	code, msg = c.send(e2eFrom, []string{e2eRcpt}, e2eMessage("client-2", "SMTP held", "X-Thinpost-Hold: true"))
	heldID := queuedID(t, code, msg)
	if !strings.Contains(msg, "2.0.0 OK: held as "+heldID) {
		t.Fatalf("held reply %q", msg)
	}
	if r.count(`SELECT count(*) FROM messages WHERE id=$1 AND state='on_hold' AND hold_expires_at IS NOT NULL`, heldID) != 1 {
		t.Fatal("the message was not stored on hold")
	}
}

// TestSubmissionRecipientAllowlistIsRefusedAtData: the edge accepts any
// well-formed recipient; control refuses the whole message at DATA.
func TestSubmissionRecipientAllowlistIsRefusedAtData(t *testing.T) {
	r := newSubmissionRig(t, rigOptions{allowlist: []string{e2eRcpt}})
	stop := r.start("controller", r.lossy)
	defer stop()
	r.waitAvailable()
	c := r.dial()
	if code, _ := c.auth(r.cred, r.secret); code != 235 {
		t.Fatal("AUTH")
	}
	code, msg := c.send(e2eFrom, []string{"someone@example.org"}, e2eMessage("client-3", "SMTP policy"))
	if code != 550 || !strings.Contains(msg, "5.7.1") {
		t.Fatalf("got %d %q", code, msg)
	}
	if r.count(`SELECT count(*) FROM messages`) != 0 {
		t.Fatal("a refused message was stored")
	}
	if code, msg = c.send(e2eFrom, []string{e2eRcpt}, e2eMessage("client-4", "SMTP policy ok")); code != 250 {
		t.Fatalf("an allowed recipient: %d %q", code, msg)
	}
}

// TestSubmissionQuotedRecipientIsRefused: a quoted local part decodes at the
// edge to a different mailbox once re-parsed, so RCPT refuses it and nothing is
// stored or sent to the unquoted mailbox.
func TestSubmissionQuotedRecipientIsRefused(t *testing.T) {
	r := newSubmissionRig(t, rigOptions{allowlist: []string{"ops@example.net"}})
	stop := r.start("controller", r.lossy)
	defer stop()
	r.waitAvailable()
	c := r.dial()
	if code, _ := c.auth(r.cred, r.secret); code != 235 {
		t.Fatal("AUTH")
	}
	code, msg := c.send(e2eFrom, []string{`"\"ops\""@example.net`}, e2eMessage("client-q", "Quoted"))
	if code != 553 || !strings.Contains(msg, "5.1.3") {
		t.Fatalf("got %d %q", code, msg)
	}
	if r.count(`SELECT count(*) FROM messages`) != 0 {
		t.Fatal("a refused message was stored")
	}
}

// TestSubmissionPauseAfterLostAcknowledgement: the commit happened but the
// client never heard. Even with the app paused, the retry is answered with the
// same ID and one row; a different message is refused.
func TestSubmissionPauseAfterLostAcknowledgement(t *testing.T) {
	r := newSubmissionRig(t, rigOptions{})
	stop := r.start("controller", r.lossy)
	defer stop()
	r.waitAvailable()
	raw := e2eMessage("client-5", "SMTP lost ack")
	r.lossy.mode.Store(2)
	c := r.dial()
	if code, _ := c.auth(r.cred, r.secret); code != 235 {
		t.Fatal("AUTH")
	}
	if code, msg := c.send(e2eFrom, []string{e2eRcpt}, raw); code != 451 {
		t.Fatalf("the lost acknowledgement: %d %q", code, msg)
	}
	ids := r.messageIDsBySubject("SMTP lost ack")
	if len(ids) != 1 {
		t.Fatalf("%d rows after the commit", len(ids))
	}
	r.exec(`UPDATE apps SET active=false WHERE id=$1`, r.app)
	r.lossy.mode.Store(0)
	c = r.dial()
	// Authentication is the credential only: a paused app can still log in.
	if code, msg := c.auth(r.cred, r.secret); code != 235 {
		t.Fatalf("AUTH on a paused app: %d %q", code, msg)
	}
	code, msg := c.send(e2eFrom, []string{e2eRcpt}, raw)
	if got := queuedID(t, code, msg); got != ids[0] {
		t.Fatalf("the retry got %s, want the original %s", got, ids[0])
	}
	if n := len(r.messageIDsBySubject("SMTP lost ack")); n != 1 {
		t.Fatalf("%d rows after the retry", n)
	}
	if code, msg = c.send(e2eFrom, []string{e2eRcpt}, e2eMessage("client-6", "SMTP while paused")); code != 550 || !strings.Contains(msg, "5.7.1") {
		t.Fatalf("a different message on a paused app: %d %q", code, msg)
	}
}

// TestSubmissionDisconnectCases covers what a client sees when the answer is
// lost around the commit.
func TestSubmissionDisconnectCases(t *testing.T) {
	r := newSubmissionRig(t, rigOptions{})
	stop := r.start("controller", r.lossy)
	defer stop()
	r.waitAvailable()
	c := r.dial()
	if code, _ := c.auth(r.cred, r.secret); code != 235 {
		t.Fatal("AUTH")
	}
	submit := func(raw string) (int, string) {
		t.Helper()
		return c.send(e2eFrom, []string{e2eRcpt}, raw)
	}

	t.Run("forced pre-commit rollback", func(t *testing.T) {
		raw := e2eMessage("client-7", "SMTP rollback")
		r.lossy.mode.Store(1)
		if code, _ := submit(raw); code != 451 {
			t.Fatalf("got %d", code)
		}
		if n := len(r.messageIDsBySubject("SMTP rollback")); n != 0 {
			t.Fatalf("%d rows without a commit", n)
		}
		r.lossy.mode.Store(0)
		code, msg := submit(raw)
		queuedID(t, code, msg)
		if n := len(r.messageIDsBySubject("SMTP rollback")); n != 1 {
			t.Fatalf("the retry created %d rows", n)
		}
	})
	retryAfterLoss := func(name, raw, subject string, wantRows int) {
		t.Run(name, func(t *testing.T) {
			r.lossy.mode.Store(2)
			if code, _ := submit(raw); code != 451 {
				t.Fatalf("got %d", code)
			}
			r.lossy.mode.Store(0)
			code, msg := submit(raw)
			id := queuedID(t, code, msg)
			ids := r.messageIDsBySubject(subject)
			if len(ids) != wantRows {
				t.Fatalf("%d rows, want %d", len(ids), wantRows)
			}
			if wantRows == 1 && ids[0] != id {
				t.Fatal("the retry was not answered with the original ID")
			}
		})
	}
	retryAfterLoss("drop after commit with a Message-ID", e2eMessage("client-8", "SMTP drop id"), "SMTP drop id", 1)
	// Only a Date: the retry within 10 minutes is the same message.
	retryAfterLoss("drop after commit with only a Date", e2eMessage("", "SMTP drop date", "Date: "+time.Now().UTC().Format(time.RFC1123Z)), "SMTP drop date", 1)
	// Neither header: there is nothing to recognize the retry by.
	retryAfterLoss("drop after commit with neither", e2eMessage("", "SMTP drop neither"), "SMTP drop neither", 2)
}

// TestSubmissionTakeoverDuringBlockedCommit: the session is cut while a commit
// is blocked; the client sees 451 and nothing is committed. A new controller
// takes over and the retry is accepted exactly once.
func TestSubmissionTakeoverDuringBlockedCommit(t *testing.T) {
	r := newSubmissionRig(t, rigOptions{})
	stop := r.start("controller", r.lossy)
	r.waitAvailable()
	c := r.dial()
	if code, _ := c.auth(r.cred, r.secret); code != 235 {
		t.Fatal("AUTH")
	}
	raw := e2eMessage("client-9", "SMTP takeover")
	r.gate.hold.Store(true)
	type answer struct {
		code int
		msg  string
	}
	answers := make(chan answer, 1)
	go func() {
		code, msg := c.send(e2eFrom, []string{e2eRcpt}, raw)
		answers <- answer{code, msg}
	}()
	select {
	case <-r.gate.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the commit was never reached")
	}
	stop()
	select {
	case a := <-answers:
		if a.code != 451 {
			t.Fatalf("the client got %d %q when the session was cut", a.code, a.msg)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the client was never answered")
	}
	r.gate.hold.Store(false)
	close(r.gate.release)
	eventually(t, "the old session to detach", func() bool { return !r.edge.SMTPSubmission().Available() })
	if n := len(r.messageIDsBySubject("SMTP takeover")); n != 0 {
		t.Fatalf("%d rows committed by the cut session", n)
	}
	// The first controller is gone; its lease runs out and another one takes over.
	r.exec(`UPDATE controller_leases SET expires_at=clock_timestamp()-interval '1 second'`)
	stop2 := r.start("controller2", r.lossy)
	defer stop2()
	r.waitAvailable()
	c2 := r.dial()
	if code, _ := c2.auth(r.cred, r.secret); code != 235 {
		t.Fatal("AUTH after the takeover")
	}
	code, msg := c2.send(e2eFrom, []string{e2eRcpt}, raw)
	id := queuedID(t, code, msg)
	if ids := r.messageIDsBySubject("SMTP takeover"); len(ids) != 1 || ids[0] != id {
		t.Fatalf("rows after the takeover: %v", ids)
	}
}

// TestNewEdgeWithOldControlAnswers454: the edge offers AUTH, but a control
// that does not offer submission/1 never serves it.
func TestNewEdgeWithOldControlAnswers454(t *testing.T) {
	r := newSubmissionRig(t, rigOptions{withoutIntake: true})
	// Wait until the control session is up, so the 454 is not just "not yet".
	eventually(t, "the edge to be connected", func() bool { return r.count(`SELECT count(*) FROM controller_leases`) > 0 })
	c := r.dial()
	if code, msg := c.auth(r.cred, r.secret); code != 454 || !strings.Contains(msg, "4.7.0") {
		t.Fatalf("AUTH against a control without submission: %d %q", code, msg)
	}
	if code, _ := c.cmd("MAIL FROM:<" + e2eFrom + ">"); code != 530 {
		t.Fatalf("MAIL before AUTH: %d", code)
	}
	if r.edge.SMTPSubmission().Available() {
		t.Fatal("submission is available without negotiation")
	}
}
