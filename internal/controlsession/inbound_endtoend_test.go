package controlsession

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/bounce"
	"github.com/zyno-io/thinpost/internal/controllease"
	"github.com/zyno-io/thinpost/internal/controltls"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/edgesession"
	"github.com/zyno-io/thinpost/internal/inbound"
	"github.com/zyno-io/thinpost/internal/journal"
	"github.com/zyno-io/thinpost/internal/leaseproof"
	"github.com/zyno-io/thinpost/internal/objectstore"
	"github.com/zyno-io/thinpost/internal/postgres"
	"github.com/zyno-io/thinpost/internal/webhook"
	"github.com/zyno-io/thinpost/internal/webhook/format"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
)

// inboundGate wraps the real repository: it can hold or fail the database
// commit, never replace it.
type inboundGate struct {
	*postgres.Repository
	entered chan struct{}
	release chan struct{}
	hold    atomic.Bool
	fail    atomic.Bool
	commits atomic.Int32
}

func (g *inboundGate) CommitInbound(ctx context.Context, record inbound.Record) (inbound.CommitStatus, error) {
	g.commits.Add(1)
	if g.fail.Load() {
		return inbound.CommitTempfailRouting, errors.New("injected database failure")
	}
	if g.hold.Load() {
		select {
		case g.entered <- struct{}{}:
		default:
		}
		select {
		case <-g.release:
		case <-ctx.Done():
			return inbound.CommitTempfailRouting, ctx.Err()
		}
	}
	return g.Repository.CommitInbound(ctx, record)
}

// noDNS answers every query with NXDOMAIN, so authentication reports none.
type noDNS struct{}

func (noDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}
func (noDNS) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}
func (noDNS) LookupIPAddr(_ context.Context, name string) ([]net.IPAddr, error) {
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}
func (noDNS) LookupAddr(_ context.Context, name string) ([]string, error) {
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

type noWork struct{}

func (noWork) Next(context.Context, postgres.ControllerLease, delivery.EdgeCapabilities) (*controlv1.DeliveryStart, []byte, error) {
	return nil, nil, delivery.ErrNoWork
}

func sendInboundMail(address, from string, rcpts []string, body []byte, roots *x509.CertPool) error {
	client, err := smtp.Dial(address)
	if err != nil {
		return err
	}
	defer client.Close()
	if err = client.StartTLS(&tls.Config{RootCAs: roots, ServerName: "edge.internal", MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
	if err = client.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range rcpts {
		if err = client.Rcpt(rcpt); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = writer.Write(body); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

func replyCode(err error) int {
	var e *textproto.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// captureSender records every request the dispatcher sends and answers 204.
type captureSender struct {
	mu       sync.Mutex
	requests []format.Request
}

func (c *captureSender) Send(_ context.Context, _ string, request format.Request, _ *admin.WebhookAuth) (webhook.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, request)
	return webhook.Result{StatusCode: 204, Success: true}, nil
}

type storedObjects struct{ *objectstore.EncryptedStore }

func (o storedObjects) GetMIME(ctx context.Context, key string, maximum int64) ([]byte, error) {
	return o.Get(ctx, key, maximum)
}

type deliveredInbound struct {
	Outcome string `json:"outcome"`
	AppID   string `json:"app_id"`
	Inbound struct {
		Recipient struct {
			Address string `json:"address"`
			Tag     string `json:"tag"`
		} `json:"recipient"`
		Envelope struct {
			RcptTo   []string `json:"rcpt_to"`
			MailFrom string   `json:"mail_from"`
			ClientIP string   `json:"client_ip"`
		} `json:"envelope"`
		Headers struct {
			Subject string `json:"subject"`
		} `json:"headers"`
		Text           string `json:"text"`
		Raw            string `json:"raw"`
		Authentication struct {
			SPF struct {
				Result string `json:"result"`
			} `json:"spf"`
		} `json:"authentication"`
		Correlation struct {
			Matched     bool   `json:"matched"`
			MessageID   string `json:"message_id"`
			DeliveryID  string `json:"delivery_id"`
			Header      string `json:"header"`
			FromMatches bool   `json:"from_matches_recipient"`
		} `json:"correlation"`
	} `json:"inbound"`
}

// TestInboundMailPipelineWithPostgres runs a real SMTP client through the Edge
// receiver, the mTLS control stream, control intake, the encrypted object store
// and PostgreSQL: 250 only after the commit, 451 on an injected commit failure,
// and an identical retry gives one message.
func TestInboundMailPipelineWithPostgres(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	u, err := validatePipelineFixtureURL(dsn, os.Getenv("THINPOST_TEST_DISPOSABLE_DB") == "1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
	schema := "inbound_pipeline_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
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
	runtimeDB, cleanupRuntime := restrictedPipelineRuntime(t, ctx, db, u)
	defer cleanupRuntime()
	const generation = "00000000000000000000000000000bbb"
	if _, err = db.ExecContext(ctx, `UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, generation); err != nil {
		t.Fatal(err)
	}
	const app = "00000000000000000000000000000001"
	if _, err = db.ExecContext(ctx, `INSERT INTO apps(id,name) VALUES ($1,'test')`, app); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO inbound_domains(id,app_id,domain,state,verified_at) VALUES ('00000000000000000000000000000002',$1,'mail.example.com','active',now()),
		('00000000000000000000000000000003',$1,'paused.example.com','disabled',now())`, app); err != nil {
		t.Fatal(err)
	}
	// The app's inbound webhook endpoint; its secret is sealed below, once the
	// keyring exists.
	if _, err = db.ExecContext(ctx, `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,format,secret_ciphertext) VALUES ('00000000000000000000000000000004',$1,'https://hooks.example.com/in','thinpost_inbound','\x73')`, app); err != nil {
		t.Fatal(err)
	}
	rawObjects := pipelineRawObjects(t)
	keyringPath := filepath.Join(t.TempDir(), "object-keyring.json")
	var encryptionKey [32]byte
	if _, err = rand.Read(encryptionKey[:]); err != nil {
		t.Fatal(err)
	}
	keyringJSON := fmt.Sprintf(`{"active":"fixture","keys":{"fixture":%q}}`, base64.StdEncoding.EncodeToString(encryptionKey[:]))
	if err = os.WriteFile(keyringPath, []byte(keyringJSON), 0600); err != nil {
		t.Fatal(err)
	}
	keyring, err := objectstore.LoadKeyring(keyringPath)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := objectstore.NewEncrypted(rawObjects, keyring)
	if err != nil {
		t.Fatal(err)
	}
	repo := &postgres.Repository{DB: runtimeDB, Generation: generation, Objects: objects}
	gate := &inboundGate{Repository: repo, entered: make(chan struct{}, 1), release: make(chan struct{})}
	service := &inbound.Service{Repository: gate, Objects: objects, Keys: [][]byte{[]byte("0123456789abcdef0123456789abcdef")}, Resolver: noDNS{}}

	roots, clientCert, serverCert := certificates(t)
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
	defer j.Close()
	journalID, err := j.Identity()
	if err != nil {
		t.Fatal(err)
	}
	authority, err := leaseproof.NewAuthority(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := edgesession.New("worker", []string{controllerIdentity}, j, journalExecutor{journal: j}, authority, 2)
	if err != nil {
		t.Fatal(err)
	}
	edge.EnableInbound()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)), grpc.MaxRecvMsgSize(128<<10), grpc.MaxSendMsgSize(128<<10))
	controlv1.RegisterEdgeControlServer(server, edge)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient("passthrough:///edge.internal", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }), grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	signer, ok := clientCert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("unexpected key")
	}
	runner := &Runner{WorkerID: "worker", ControllerID: "controller", JournalID: journalID, Issuer: &controllease.Issuer{Repository: repo, Signer: signer, Identity: controllerIdentity, JournalID: journalID},
		Results: repo, Starts: repo, Reconciler: repo, Work: noWork{}, Inbound: service, LeaseTTL: 30 * time.Second, ClockMargin: 6 * time.Second, PollInterval: 100 * time.Millisecond, RoutesInterval: 100 * time.Millisecond,
		Connect: func(ctx context.Context) (Stream, error) { return controlv1.NewEdgeControlClient(conn).Connect(ctx) }}
	done := make(chan error, 1)
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	go func() { done <- runner.Run(runCtx) }()

	smtpServer, err := bounce.NewServer(bounce.Config{Hostname: "smtp.example.com", Domains: []string{"bounce.example.com"}, TLS: &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12},
		MaxBytes: 16 << 20, MaxConcurrent: 1, ForwardTimeout: 5 * time.Second, Forwarder: edge, Inbound: edge.Inbound(), InboundForwardTimeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	smtpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = smtpServer.Serve(smtpListener) }()
	defer smtpServer.Close()
	address := smtpListener.Addr().String()

	// The routing snapshot reaches the worker: until it does, RCPT is a 550.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, state := edge.Inbound().Lookup("mail.example.com"); state == bounce.RouteAccept {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the routing snapshot never arrived")
		}
		time.Sleep(20 * time.Millisecond)
	}

	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	body := []byte("From: Alice <alice@example.org>\r\nTo: reply@mail.example.com\r\nSubject: Fixture subject 4471\r\nMessage-ID: <e2e1@example.org>\r\n\r\nfixture-inbound-body-90210\r\n")
	rcpts := []string{"reply+ticket@mail.example.com", "other@MAIL.example.com"}

	// 250 only after the database commit.
	gate.hold.Store(true)
	result := make(chan error, 1)
	go func() { result <- sendInboundMail(address, "alice@example.org", rcpts, body, roots) }()
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the message never reached the database commit")
	}
	select {
	case err = <-result:
		t.Fatalf("SMTP answered before the commit: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if count(`SELECT count(*) FROM inbound_messages`) != 0 {
		t.Fatal("message visible before the commit")
	}
	gate.hold.Store(false)
	close(gate.release)
	select {
	case err = <-result:
		if err != nil {
			t.Fatalf("committed message refused: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no SMTP answer after the commit")
	}
	var objectKey, mailFrom, subject, helo, receiver, ip, spf string
	if err = db.QueryRowContext(ctx, `SELECT object_key,mail_from,subject,COALESCE(helo,''),COALESCE(receiver,''),host(client_ip),spf_result FROM inbound_messages WHERE app_id=$1`, app).Scan(&objectKey, &mailFrom, &subject, &helo, &receiver, &ip, &spf); err != nil {
		t.Fatal(err)
	}
	if mailFrom != "alice@example.org" || subject != "Fixture subject 4471" || receiver != "smtp.example.com" || ip != "127.0.0.1" || helo == "" || spf != "none" {
		t.Fatal("stored envelope", mailFrom, subject, receiver, ip, helo, spf)
	}
	stored, err := objects.Get(ctx, objectKey, 1<<20)
	if err != nil || !bytes.Equal(stored, body) {
		t.Fatal("raw MIME not stored", err)
	}
	if raw, rawErr := rawObjects.Get(ctx, objectKey, 1<<20); rawErr != nil || bytes.Contains(raw, []byte("fixture-inbound-body-90210")) {
		t.Fatal("the object is not encrypted at rest", rawErr)
	}
	if count(`SELECT count(*) FROM inbound_recipients`) != 2 || count(`SELECT count(*) FROM webhook_events WHERE outcome='inbound_received'`) != 2 || count(`SELECT count(*) FROM webhook_deliveries WHERE lane='inbound'`) != 2 {
		t.Fatal("recipients, events or deliveries missing")
	}

	// An identical retry is acknowledged and changes nothing.
	if err = sendInboundMail(address, "alice@example.org", rcpts, body, roots); err != nil {
		t.Fatalf("retry refused: %v", err)
	}
	if count(`SELECT count(*) FROM inbound_messages`) != 1 || count(`SELECT count(*) FROM webhook_events WHERE outcome='inbound_received'`) != 2 {
		t.Fatal("an identical retry created another message")
	}

	// An injected commit failure is a 451 and stores nothing; the retry works.
	second := []byte(strings.Replace(string(body), "e2e1", "e2e2", 1))
	gate.fail.Store(true)
	err = sendInboundMail(address, "alice@example.org", rcpts[:1], second, roots)
	if replyCode(err) != 451 || strings.Contains(err.Error(), "injected") {
		t.Fatalf("injected failure: %v", err)
	}
	if count(`SELECT count(*) FROM inbound_messages`) != 1 {
		t.Fatal("a failed commit created a message")
	}
	gate.fail.Store(false)
	if err = sendInboundMail(address, "alice@example.org", rcpts[:1], second, roots); err != nil {
		t.Fatalf("retry after the failure: %v", err)
	}
	if count(`SELECT count(*) FROM inbound_messages`) != 2 {
		t.Fatal("the retry did not create exactly one message")
	}

	// Routing answers at RCPT: unknown domains are refused, a disabled domain
	// is a tempfail, and the bounce path is untouched.
	client, err := smtp.Dial(address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err = client.Mail("alice@example.org"); err != nil {
		t.Fatal(err)
	}
	if err = client.Rcpt("x@unknown.example.net"); replyCode(err) != 550 || !strings.Contains(err.Error(), "5.7.1") {
		t.Fatalf("unknown domain: %v", err)
	}
	if err = client.Rcpt("x@paused.example.com"); replyCode(err) != 451 {
		t.Fatalf("disabled domain: %v", err)
	}
	if err = client.Rcpt("user@bounce.example.com"); replyCode(err) != 550 || !strings.Contains(err.Error(), "5.1.1") {
		t.Fatalf("bounce domain with no token: %v", err)
	}
	if gate.commits.Load() != 3 {
		t.Fatalf("unexpected commit attempts: %d", gate.commits.Load())
	}

	// Delivery to the app: an outbound message the app sent to alice, and a reply
	// that quotes its Message-ID. Then every queued event is dispatched through
	// the real claim, object store, renderer and signer to a capturing sender.
	const sentMessage = "00000000000000000000000000000010"
	if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,subject,recipient_count,sender_domain,sender_domain_rev,sender_address,state,shared_message_id)
		VALUES ($1,$2,'k'||$1,false,'Your receipt',1,'sender.example','example.sender.','noreply@sender.example','prepared',true)`, sentMessage, app); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO message_recipients(message_id,app_id,recipient_index,address,address_lower,domain_lower,domain_rev,kind) VALUES ($1,$2,0,'alice@example.org','alice@example.org','example.org','gro.elpmaxe.','to')`, sentMessage, app); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state,bounce_key_version,bounce_token_hash) VALUES ('00000000000000000000000000000011',$1,0,'f','accepted','v1',decode(repeat('ab',32),'hex'))`, sentMessage); err != nil {
		t.Fatal(err)
	}
	reply := []byte("From: Alice <alice@example.org>\r\nTo: reply@mail.example.com\r\nSubject: Re: Your receipt\r\nMessage-ID: <e2e3@example.org>\r\nIn-Reply-To: <" + sentMessage + "@sender.example>\r\nAuthentication-Results: smtp.example.com; dmarc=pass header.from=example.org\r\n\r\nreply-body-77\r\n")
	if err = sendInboundMail(address, "alice@example.org", []string{"reply+t3@mail.example.com"}, reply, roots); err != nil {
		t.Fatalf("reply refused: %v", err)
	}
	if count(`SELECT count(*) FROM inbound_messages WHERE correlated_message_id=$1 AND correlated_delivery_id='00000000000000000000000000000011'`, sentMessage) != 1 {
		t.Fatal("the reply was not correlated")
	}
	secret := []byte("tpwh_end-to-end-signing-secret-0123456789")
	const subscription = "00000000000000000000000000000004"
	sealed, err := keyring.Seal(webhook.SecretAAD(app, subscription), secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE webhook_subscriptions SET secret_ciphertext=$1 WHERE id=$2`, sealed, subscription); err != nil {
		t.Fatal(err)
	}
	capture := &captureSender{}
	dispatcher := &webhook.InboundDispatcher{Repository: repo, Sender: capture, Keys: keyring, Objects: storedObjects{objects}, Budget: webhook.NewRenderBudget(webhook.RenderBudgetBytes)}
	for i := 0; ; i++ {
		if err = dispatcher.DispatchNext(ctx); errors.Is(err, webhook.ErrNoWork) {
			break
		} else if err != nil || i > 10 {
			t.Fatalf("dispatch: %v", err)
		}
	}
	if len(capture.requests) != 4 || count(`SELECT count(*) FROM webhook_deliveries WHERE lane='inbound' AND state='delivered'`) != 4 {
		t.Fatalf("%d requests delivered, want 4", len(capture.requests))
	}
	var correlated, plain int
	for _, request := range capture.requests {
		timestamp, signature, eventID := request.Header.Get("X-Thinpost-Timestamp"), request.Header.Get("X-Thinpost-Signature"), request.Header.Get("X-Thinpost-Event-ID")
		if !format.VerifyThinpost(secret, eventID, timestamp, request.Body, signature, time.Now(), time.Minute) {
			t.Fatal("a delivery is not signed with the endpoint's secret")
		}
		var payload deliveredInbound
		if err = json.Unmarshal(request.Body, &payload); err != nil {
			t.Fatal(err)
		}
		raw, rawErr := base64.StdEncoding.DecodeString(payload.Inbound.Raw)
		if rawErr != nil || payload.Outcome != "inbound_received" || payload.AppID != app || payload.Inbound.Envelope.ClientIP != "127.0.0.1" ||
			!strings.HasPrefix(string(raw), "Authentication-Results: smtp.example.com;") || !strings.Contains(string(raw), "\r\nReceived: from ") {
			t.Fatal("payload", payload.Outcome, payload.AppID, payload.Inbound.Envelope)
		}
		if strings.Count(string(raw), "smtp.example.com;") != 1 {
			t.Fatal("a sender's Authentication-Results claiming our authserv-id was delivered")
		}
		switch payload.Inbound.Headers.Subject {
		case "Fixture subject 4471":
			plain++
			if payload.Inbound.Correlation.Matched || !strings.Contains(payload.Inbound.Text, "fixture-inbound-body-90210") || payload.Inbound.Authentication.SPF.Result != "none" {
				t.Fatal("uncorrelated payload", payload.Inbound.Correlation, payload.Inbound.Text)
			}
		case "Re: Your receipt":
			correlated++
			c := payload.Inbound.Correlation
			if !c.Matched || c.MessageID != sentMessage || c.DeliveryID != "00000000000000000000000000000011" || c.Header != "in_reply_to" || !c.FromMatches ||
				payload.Inbound.Recipient.Tag != "t3" || !strings.Contains(payload.Inbound.Text, "reply-body-77") {
				t.Fatal("correlated payload", c, payload.Inbound.Recipient)
			}
		default:
			t.Fatal("unexpected subject", payload.Inbound.Headers.Subject)
		}
	}
	if plain != 3 || correlated != 1 {
		t.Fatalf("%d plain and %d correlated deliveries", plain, correlated)
	}
	cancelRun()
	<-done
}
