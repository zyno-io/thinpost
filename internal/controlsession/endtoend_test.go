package controlsession

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/netip"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/minio/minio-go/v7"
	controlv1 "github.com/zyno-io/thinpost/api/control/v1"
	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/bounce"
	"github.com/zyno-io/thinpost/internal/bounceaddress"
	"github.com/zyno-io/thinpost/internal/bounceingest"
	"github.com/zyno-io/thinpost/internal/controlapp"
	"github.com/zyno-io/thinpost/internal/controllease"
	"github.com/zyno-io/thinpost/internal/controltls"
	"github.com/zyno-io/thinpost/internal/dispatch"
	"github.com/zyno-io/thinpost/internal/edgesession"
	"github.com/zyno-io/thinpost/internal/edgeworker"
	"github.com/zyno-io/thinpost/internal/httpapi"
	"github.com/zyno-io/thinpost/internal/journal"
	"github.com/zyno-io/thinpost/internal/leaseproof"
	"github.com/zyno-io/thinpost/internal/mailroute"
	"github.com/zyno-io/thinpost/internal/objectstore"
	"github.com/zyno-io/thinpost/internal/postgres"
	"github.com/zyno-io/thinpost/internal/preparation"
	"github.com/zyno-io/thinpost/internal/providerid"
	"github.com/zyno-io/thinpost/internal/recipientpolicy"
	"github.com/zyno-io/thinpost/internal/signing"
	"github.com/zyno-io/thinpost/internal/tracking"
	"github.com/zyno-io/thinpost/internal/webhook"
	"github.com/zyno-io/thinpost/internal/webhook/format"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
)

type integrationObjects struct {
	mu   sync.Mutex
	data map[string][]byte
}

// integrationBounceGate delays the real database commit, and can inject a
// failure after object upload. It never substitutes for the actual repository.
type integrationBounceGate struct {
	repository *postgres.Repository
	entered    chan struct{}
	release    chan struct{}
	fail       atomic.Bool
}

func (g *integrationBounceGate) CheckBounce(ctx context.Context, token, fingerprint [32]byte) (bounceingest.Status, error) {
	return g.repository.CheckBounce(ctx, token, fingerprint)
}

func (g *integrationBounceGate) CommitBounce(ctx context.Context, record bounceingest.Record) error {
	if g.fail.Load() {
		return errors.New("injected database failure")
	}
	select {
	case g.entered <- struct{}{}:
	default:
	}
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.repository.CommitBounce(ctx, record)
}

func sendIntegrationBounce(address, recipient string, body []byte, roots *x509.CertPool) error {
	client, err := smtp.Dial(address)
	if err != nil {
		return err
	}
	defer client.Close()
	if err = client.StartTLS(&tls.Config{RootCAs: roots, ServerName: "edge.internal", MinVersion: tls.VersionTLS12}); err != nil {
		return err
	}
	if err = client.Mail(""); err != nil {
		return err
	}
	if err = client.Rcpt(recipient); err != nil {
		return err
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

func (s *integrationObjects) Put(_ context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key]; ok {
		return fmt.Errorf("object exists")
	}
	s.data[key] = bytes.Clone(data)
	return nil
}
func (s *integrationObjects) PutMIME(ctx context.Context, key string, data []byte) error {
	return s.Put(ctx, key, data)
}
func (s *integrationObjects) VerifyPresent(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[key]; !ok {
		return fmt.Errorf("object unavailable")
	}
	return nil
}
func (s *integrationObjects) Get(_ context.Context, key string, max int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.data[key]
	if !ok || int64(len(data)) > max {
		return nil, fmt.Errorf("object unavailable")
	}
	return bytes.Clone(data), nil
}

type integrationDNS struct{ record string }

func (d integrationDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	if name != "mail1._domainkey.example.com" {
		return nil, fmt.Errorf("unexpected DNS query")
	}
	return []string{d.record}, nil
}

type integrationMXDNS struct{}
type integrationMXFilter struct{}

// integrationWebhookSender posts a format-built request to a loopback fixture,
// which the production destination policy deliberately refuses.
type integrationWebhookSender struct{ client *http.Client }

func (s integrationWebhookSender) Send(ctx context.Context, endpoint string, payload format.Request, _ *admin.WebhookAuth) (webhook.Result, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload.Body))
	if err != nil {
		return webhook.Result{}, err
	}
	for name, values := range payload.Header {
		request.Header[name] = values
	}
	response, err := s.client.Do(request)
	if err != nil {
		return webhook.Result{}, err
	}
	defer response.Body.Close()
	return webhook.Result{StatusCode: response.StatusCode, Success: response.StatusCode/100 == 2}, nil
}

func (d *integrationMXDNS) LookupMX(_ context.Context, domain string) ([]*net.MX, error) {
	if domain != "example.net" {
		return nil, errors.New("unexpected recipient domain")
	}
	return []*net.MX{{Host: "edge.internal.", Pref: 10}}, nil
}
func (d *integrationMXDNS) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if host != "edge.internal" {
		return nil, errors.New("unexpected MX queried")
	}
	return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("45.0.0.9")}, nil
}
func (integrationMXFilter) AllowedFor(address netip.Addr, _, _ string) bool {
	return address == netip.MustParseAddr("8.8.8.8") || address == netip.MustParseAddr("45.0.0.9")
}

type integrationExecutor struct {
	worker  *edgeworker.Worker
	started chan *controlv1.DeliveryStart
}

func (e integrationExecutor) Execute(ctx context.Context, fence journal.Fence, start *controlv1.DeliveryStart, mime []byte, authorized func() bool, permit func(context.Context, *controlv1.DeliveryStart) error) (*controlv1.DeliveryResult, error) {
	result, err := e.worker.Execute(ctx, fence, start, mime, authorized, permit)
	if err == nil {
		e.started <- start
	}
	return result, err
}

type integrationSMTPDialer struct {
	certificate tls.Certificate
	completed   chan error
	received    chan []byte
	dkimRecord  string
	dials       atomic.Int32
}

type integrationSwitchPolicy struct{ allowed atomic.Bool }

func (p *integrationSwitchPolicy) Allows(string) bool { return p.allowed.Load() }

func (d *integrationSMTPDialer) Dial(_ context.Context, _, address netip.Addr, _, _ string) (net.Conn, error) {
	dialNumber := d.dials.Add(1)
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		if dialNumber == 1 && address == netip.MustParseAddr("8.8.8.8") {
			_, err := fmt.Fprint(server, "554 endpoint unavailable\r\n")
			d.completed <- err
			return
		}
		if address != netip.MustParseAddr("8.8.8.8") && address != netip.MustParseAddr("45.0.0.9") {
			d.completed <- errors.New("unexpected route")
			return
		}
		readCommand := func(reader *bufio.Reader, prefix string) error {
			line, err := reader.ReadString('\n')
			if err != nil {
				return err
			}
			if !strings.HasPrefix(line, prefix) {
				return fmt.Errorf("expected SMTP command %q", prefix)
			}
			return nil
		}
		run := func() error {
			reader := bufio.NewReader(server)
			if _, err := fmt.Fprint(server, "220 ready\r\n"); err != nil {
				return err
			}
			if err := readCommand(reader, "EHLO "); err != nil {
				return err
			}
			if _, err := fmt.Fprint(server, "250-edge.internal\r\n250 STARTTLS\r\n"); err != nil {
				return err
			}
			if err := readCommand(reader, "STARTTLS"); err != nil {
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
			if err := readCommand(reader, "EHLO "); err != nil {
				return err
			}
			if _, err := fmt.Fprint(secured, "250 ready\r\n"); err != nil {
				return err
			}
			for _, cmd := range []string{"MAIL FROM:", "RCPT TO:", "DATA"} {
				if err := readCommand(reader, cmd); err != nil {
					return err
				}
				response := "250 ok\r\n"
				if cmd == "DATA" {
					response = "354 send\r\n"
				}
				if _, err := fmt.Fprint(secured, response); err != nil {
					return err
				}
			}
			body, err := textproto.NewReader(reader).ReadDotBytes()
			if err != nil || !bytes.HasPrefix(body, []byte("DKIM-Signature:")) {
				return errors.New("signed MIME not received")
			}
			wireMIME := bytes.ReplaceAll(body, []byte("\n"), []byte("\r\n"))
			results, verifyErr := dkim.VerifyWithOptions(bytes.NewReader(wireMIME), &dkim.VerifyOptions{LookupTXT: func(name string) ([]string, error) {
				if name != "mail1._domainkey.example.com" {
					return nil, errors.New("unexpected signing domain")
				}
				return []string{d.dkimRecord}, nil
			}})
			if verifyErr != nil || len(results) != 1 || results[0].Err != nil {
				return errors.New("received MIME failed DKIM verification")
			}
			d.received <- wireMIME
			message, err := mail.ReadMessage(bytes.NewReader(body))
			if err != nil {
				return errors.New("invalid MIME received")
			}
			replies, err := message.Header.AddressList("Reply-To")
			if err != nil || len(replies) != 1 || replies[0].Address != "help@example.com" {
				return errors.New("reply address missing from MIME")
			}
			if dialNumber == 2 {
				_, err = fmt.Fprint(secured, "451 temporary failure\r\n")
				return err
			}
			if _, err = fmt.Fprint(secured, "250 accepted\r\n"); err != nil {
				return err
			}
			if err = readCommand(reader, "QUIT"); err != nil {
				return err
			}
			_, err = fmt.Fprint(secured, "221 bye\r\n")
			return err
		}
		d.completed <- run()
	}()
	return client, nil
}

func TestDevelopmentMailPipelineWithPostgres(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	u, err := validatePipelineFixtureURL(dsn, os.Getenv("THINPOST_TEST_DISPOSABLE_DB") == "1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	schema := "pipeline_test_" + hex.EncodeToString(random[:])
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
	const testGeneration = "00000000000000000000000000000aaa"
	if _, err = db.ExecContext(ctx, `UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, testGeneration); err != nil {
		t.Fatal(err)
	}
	const app = "00000000000000000000000000000001"
	const keyID = "00000000000000000000000000000002"
	const apiKey = "example-app-key"
	digest := sha256.Sum256([]byte(apiKey))
	if _, err = db.ExecContext(ctx, `INSERT INTO apps(id,name,recipient_daily_limit,pending_message_limit,submission_rate_per_minute,submission_burst,submission_tokens) VALUES ($1,'test',100,100,60000,100,6000000000)`, app); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO api_keys(id,app_id,digest) VALUES ($1,$2,$3)`, keyID, app, digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'example.com',true,now())`, app); err != nil {
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
	repo := &postgres.Repository{DB: runtimeDB, Generation: testGeneration, Objects: objects}
	policy, err := recipientpolicy.Load(func(key string) string {
		if key == "THINPOST_RECIPIENT_POLICY" {
			return "unrestricted"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	acceptor, err := acceptance.NewWithFingerprintKeys(repo, objects, [][]byte{[]byte("0123456789abcdef0123456789abcdef")}, policy)
	if err != nil {
		t.Fatal(err)
	}
	attachmentContent := []byte("fixture-private-attachment-847291")
	attachmentBase64 := base64.StdEncoding.EncodeToString(attachmentContent)
	payload := fmt.Sprintf(`{"from":"sender@example.com","to":["recipient@example.net"],"subject":"Hello","text":"fixture-private-body-203921","reply_to":"help@example.com","attachments":[{"filename":"report.txt","content":%q,"content_type":"text/plain"}]}`, attachmentBase64)
	request := httptest.NewRequest("POST", "/emails", strings.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("X-Thinpost-Enhanced-Protection", "true")
	response := httptest.NewRecorder()
	httpapi.NewWithOptions(acceptor, httpapi.Options{}).ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("submission status %d", response.Code)
	}
	signingKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	public, err := x509.MarshalPKIXPublicKey(&signingKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	dkimRecord := "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(public)
	keys, err := signing.New(repo, integrationDNS{record: dkimRecord}, []signing.Entry{{AppID: app, Domain: "example.com", Selector: "mail1", Signer: signingKey}})
	if err != nil {
		t.Fatal(err)
	}
	preparer := &preparation.Service{Repository: repo, Objects: objects, Keys: keys}
	if err = preparer.PrepareNext(ctx); err != nil {
		t.Fatal(err)
	}
	roots, clientCert, serverCert := certificates(t)
	const hookID = "00000000000000000000000000000004"
	const hookSecret = "0123456789abcdef0123456789abcdef"
	hookReceived := make(chan string, 4)
	hookHeaders := make(chan string, 16)
	hookServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		valid := format.VerifyThinpost([]byte(hookSecret), r.Header.Get("X-Thinpost-Event-ID"), r.Header.Get("X-Thinpost-Timestamp"), body.Bytes(), r.Header.Get("X-Thinpost-Signature"), time.Now(), time.Minute)
		var event webhook.Event
		if !valid || bytes.Contains(body.Bytes(), []byte("fixture-private-body-203921")) || bytes.Contains(body.Bytes(), []byte("recipient@example.net")) || bytes.Contains(body.Bytes(), []byte("Untrusted bounce")) || json.Unmarshal(body.Bytes(), &event) != nil {
			hookReceived <- "invalid"
		} else {
			hookReceived <- event.Outcome
			select {
			case hookHeaders <- event.MessageIDHeader:
			default:
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	hookServer.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}
	hookServer.StartTLS()
	defer hookServer.Close()
	_, hookPort, err := net.SplitHostPort(hookServer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	hookURL := "https://edge.internal:" + hookPort + "/events"
	sealedHook, err := keyring.Seal(webhook.SecretAAD(app, hookID), []byte(hookSecret))
	if err != nil {
		t.Fatal(err)
	}
	// Inserted as owner: the loopback fixture host is not a valid public endpoint.
	if _, err = db.ExecContext(ctx, `INSERT INTO webhook_subscriptions(id,app_id,endpoint_url,secret_ciphertext) VALUES ($1,$2,$3,$4)`, hookID, app, hookURL, sealedHook); err != nil {
		t.Fatal(err)
	}
	hookSender := integrationWebhookSender{client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "edge.internal", MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, hookServer.Listener.Addr().String())
	}}}}
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
	started := make(chan *controlv1.DeliveryStart, 2)
	smtpDialer := &integrationSMTPDialer{certificate: serverCert, completed: make(chan error, 3), received: make(chan []byte, 2), dkimRecord: dkimRecord}
	controlPolicy := &integrationSwitchPolicy{}
	controlPolicy.allowed.Store(true)
	smtpWorker := &edgeworker.Worker{Journal: j, Dialer: smtpDialer, HeloName: "smtp.sender.example", Roots: roots}
	edge, err := edgesession.New("worker", []string{controllerIdentity}, j, integrationExecutor{worker: smtpWorker, started: started}, authority, 2)
	if err != nil {
		t.Fatal(err)
	}
	edge.Advertise([]string{"45.0.1.22"}, []string{"bounce.example.com"}, []string{"bounce.example.com"})
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
	envelopes := &bounceaddress.Service{Repository: repo, ActiveVersion: "v1", Keys: map[string][]byte{"v1": []byte("0123456789abcdef0123456789abcdef")}}
	mxDNS := &integrationMXDNS{}
	work := &dispatch.Provider{Repository: repo, Objects: objects, Routes: &mailroute.Resolver{DNS: mxDNS, Filter: integrationMXFilter{}}, Envelopes: envelopes, Recipients: controlPolicy}
	bounceGate := &integrationBounceGate{repository: repo, entered: make(chan struct{}, 1), release: make(chan struct{})}
	runner := &Runner{WorkerID: "worker", ControllerID: "controller", JournalID: journalID, Issuer: &controllease.Issuer{Repository: repo, Signer: signer, Identity: controllerIdentity, JournalID: journalID}, Results: repo, Starts: repo, Reconciler: repo, Work: work, Bounces: &bounceingest.Service{Repository: bounceGate, Objects: objects}, LeaseTTL: 30 * time.Second, ClockMargin: 6 * time.Second, PollInterval: 100 * time.Millisecond, Connect: func(ctx context.Context) (Stream, error) { return controlv1.NewEdgeControlClient(conn).Connect(ctx) }}
	done := make(chan error, 1)
	firstCtx, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	go func() { done <- runner.Run(firstCtx) }()
	var start *controlv1.DeliveryStart
	select {
	case start = <-started:
	case <-ctx.Done():
		t.Fatal("delivery not started")
	}
	if start.DestinationIp != "8.8.8.8" {
		t.Fatal("initial delivery did not use the first route")
	}
	for {
		var state string
		var attempts int
		if err = db.QueryRowContext(ctx, `SELECT state,route_attempts FROM deliveries WHERE id=$1`, start.DeliveryId).Scan(&state, &attempts); err != nil {
			t.Fatal(err)
		}
		if state == "queued" && attempts == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("route rejection not durably queued for retry")
		case <-time.After(10 * time.Millisecond):
		}
	}
	var pending bool
	if err = db.QueryRowContext(ctx, `SELECT m.pending FROM messages m JOIN deliveries d ON d.message_id=m.id WHERE d.id=$1`, start.DeliveryId).Scan(&pending); err != nil || !pending {
		t.Fatal("retrying delivery did not remain pending", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE deliveries SET retry_after=clock_timestamp()-interval '1 second' WHERE id=$1`, start.DeliveryId); err != nil {
		t.Fatal(err)
	}
	select {
	case start = <-started:
	case <-ctx.Done():
		t.Fatal("alternate route was not attempted")
	}
	if start.DestinationIp != "45.0.0.9" || !start.EnhancedProtection {
		t.Fatal("retry did not use the alternate route with TLS required")
	}
	for {
		var state string
		var attempts int
		if err = db.QueryRowContext(ctx, `SELECT state,route_attempts FROM deliveries WHERE id=$1`, start.DeliveryId).Scan(&state, &attempts); err != nil {
			t.Fatal(err)
		}
		if state == "queued" && attempts == 2 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("temporary DATA rejection was not durably queued")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err = db.ExecContext(ctx, `UPDATE deliveries SET retry_after=clock_timestamp()-interval '1 second' WHERE id=$1`, start.DeliveryId); err != nil {
		t.Fatal(err)
	}
	select {
	case start = <-started:
	case <-ctx.Done():
		t.Fatal("temporary DATA rejection was not retried")
	}
	for {
		var state string
		if err = db.QueryRowContext(ctx, `SELECT state FROM deliveries WHERE id=$1`, start.DeliveryId).Scan(&state); err != nil {
			t.Fatal(err)
		}
		items, err := j.List()
		if err != nil {
			t.Fatal(err)
		}
		if state == "accepted" && len(items) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("delivery not committed")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = db.QueryRowContext(ctx, `SELECT m.pending FROM messages m JOIN deliveries d ON d.message_id=m.id WHERE d.id=$1`, start.DeliveryId).Scan(&pending); err != nil || pending {
		t.Fatal("accepted delivery did not release pending capacity", err)
	}
	var sourceKey, finalKey string
	if err = db.QueryRowContext(ctx, `SELECT m.object_key,d.final_object_key FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE d.id=$1`, start.DeliveryId).Scan(&sourceKey, &finalKey); err != nil {
		t.Fatal(err)
	}
	finalMIME, err := objects.Get(ctx, finalKey, 16<<20)
	if err != nil {
		t.Fatal("cannot read encrypted final MIME", err)
	}
	// The wire MIME carries the shared Message-ID, <{message id}@{sender domain}>.
	var committedMessageID string
	if err = db.QueryRowContext(ctx, `SELECT message_id FROM deliveries WHERE id=$1`, start.DeliveryId).Scan(&committedMessageID); err != nil {
		t.Fatal(err)
	}
	wantMessageID := providerid.MessageIDHeader(committedMessageID, "example.com")
	if parsedFinal, parseErr := mail.ReadMessage(bytes.NewReader(finalMIME)); parseErr != nil || parsedFinal.Header.Get("Message-ID") != wantMessageID {
		t.Fatal("wire Message-ID is not the shared message-scoped header", parseErr)
	}
	source, err := objects.Get(ctx, sourceKey, 16<<20)
	if err != nil || !bytes.Contains(source, []byte(attachmentBase64)) || !bytes.Contains(finalMIME, []byte(attachmentBase64)) {
		t.Fatal("attachment missing from encrypted source or signed MIME", err)
	}
	for range 2 {
		select {
		case received := <-smtpDialer.received:
			if !bytes.Equal(received, finalMIME) {
				t.Fatal("SMTP retry did not use the stored signed MIME bytes")
			}
		case <-ctx.Done():
			t.Fatal("SMTP receiver did not record signed MIME")
		}
	}
	dispatcher := &webhook.Dispatcher{Repository: repo, Sender: hookSender, Keys: keyring}
	outcomes := make(map[string]bool)
	for range 3 {
		if err = dispatcher.DispatchNext(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-hookReceived:
			outcomes[got] = true
		case <-ctx.Done():
			t.Fatal("webhook not received")
		}
	}
	if !outcomes["route_rejected"] || !outcomes["rejected"] || !outcomes["accepted"] || len(outcomes) != 3 {
		t.Fatal("incorrect signed webhook outcomes", outcomes)
	}
	for range 3 {
		if header := <-hookHeaders; header != wantMessageID {
			t.Fatal("native webhook message_id_header differs from the wire Message-ID", header)
		}
	}
	var delivered int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM webhook_deliveries WHERE subscription_id=$1 AND state='delivered'`, hookID).Scan(&delivered); err != nil || delivered != 3 {
		t.Fatal("attempt webhooks not durably delivered", delivered, err)
	}
	local, _, ok := strings.Cut(start.EnvelopeFrom, "@")
	if !ok || !strings.HasPrefix(local, "b+") {
		t.Fatal("invalid bounce address")
	}
	bounceServer, err := bounce.NewServer(bounce.Config{Hostname: "smtp.example.com", Domains: []string{"bounce.example.com"}, TLS: &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12}, MaxBytes: 1 << 20, MaxConcurrent: 1, ForwardTimeout: 5 * time.Second, Forwarder: edge})
	if err != nil {
		t.Fatal(err)
	}
	bounceListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = bounceServer.Serve(bounceListener) }()
	defer bounceServer.Close()
	bounceBody := []byte("Subject: undelivered\r\n\r\nUntrusted bounce\r\n")
	smtpResult := make(chan error, 1)
	go func() {
		smtpResult <- sendIntegrationBounce(bounceListener.Addr().String(), start.EnvelopeFrom, bounceBody, roots)
	}()
	select {
	case <-bounceGate.entered:
	case <-ctx.Done():
		t.Fatal("bounce did not reach database commit")
	}
	select {
	case err = <-smtpResult:
		t.Fatal("SMTP replied before the database commit", err)
	default:
	}
	var bounceCount int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bounce_receipts WHERE delivery_id=$1`, start.DeliveryId).Scan(&bounceCount); err != nil || bounceCount != 0 {
		t.Fatal("bounce receipt visible before commit", bounceCount, err)
	}
	close(bounceGate.release)
	select {
	case err = <-smtpResult:
		if err != nil {
			t.Fatal("committed bounce was not accepted over SMTP", err)
		}
	case <-ctx.Done():
		t.Fatal("SMTP did not acknowledge committed bounce")
	}
	var objectKey string
	if err = db.QueryRowContext(ctx, `SELECT object_key FROM bounce_receipts WHERE delivery_id=$1`, start.DeliveryId).Scan(&objectKey); err != nil {
		t.Fatal(err)
	}
	stored, err := objects.Get(ctx, objectKey, 1024)
	if err != nil || !bytes.Equal(stored, bounceBody) {
		t.Fatal("bounce not stored", err)
	}
	for _, key := range []string{sourceKey, finalKey, objectKey} {
		data, rawErr := rawObjects.Get(ctx, key, 16<<20)
		if rawErr != nil || len(data) == 0 || bytes.Contains(data, []byte("fixture-private-body-203921")) || bytes.Contains(data, attachmentContent) || bytes.Contains(data, []byte(attachmentBase64)) || bytes.Contains(data, bounceBody) || bytes.Contains(data, []byte("recipient@example.net")) {
			t.Fatal("raw object was missing or contained message content", rawErr)
		}
		if err = rawObjects.Put(ctx, key, []byte("replacement")); err == nil {
			t.Fatal("raw object store accepted an overwrite")
		}
		if _, isS3 := rawObjects.(*objectstore.Store); isS3 && minio.ToErrorResponse(err).StatusCode != http.StatusPreconditionFailed {
			t.Fatal("S3 conditional overwrite failed for an unexpected reason", err)
		}
		after, rawErr := rawObjects.Get(ctx, key, 16<<20)
		if rawErr != nil || !bytes.Equal(after, data) {
			t.Fatal("conditional overwrite changed an existing object", rawErr)
		}
	}
	if err = sendIntegrationBounce(bounceListener.Addr().String(), start.EnvelopeFrom, bounceBody, roots); err != nil {
		t.Fatal("duplicate bounce was not accepted over SMTP", err)
	}
	if err = dispatcher.DispatchNext(ctx); err != nil {
		t.Fatal("bounce webhook was not dispatched", err)
	}
	select {
	case got := <-hookReceived:
		if got != "bounce_received_unverified" {
			t.Fatal("bounce webhook claimed a different outcome", got)
		}
	case <-ctx.Done():
		t.Fatal("bounce webhook not received")
	}
	if err = dispatcher.DispatchNext(ctx); !errors.Is(err, webhook.ErrNoWork) {
		t.Fatal("duplicate bounce created another webhook", err)
	}
	// A failed commit must produce 451 and no receipt or webhook; a later
	// retry of the same body may commit and emit one unverified event.
	secondBounce := []byte("Subject: retry\r\n\r\nAnother untrusted bounce\r\n")
	bounceGate.fail.Store(true)
	err = sendIntegrationBounce(bounceListener.Addr().String(), start.EnvelopeFrom, secondBounce, roots)
	var smtpError *textproto.Error
	if !errors.As(err, &smtpError) || smtpError.Code != 451 {
		t.Fatal("failed database commit did not return SMTP 451", err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bounce_receipts WHERE delivery_id=$1`, start.DeliveryId).Scan(&bounceCount); err != nil || bounceCount != 1 {
		t.Fatal("failed bounce created a receipt", bounceCount, err)
	}
	if err = dispatcher.DispatchNext(ctx); !errors.Is(err, webhook.ErrNoWork) {
		t.Fatal("failed bounce created a webhook", err)
	}
	bounceGate.fail.Store(false)
	if err = sendIntegrationBounce(bounceListener.Addr().String(), start.EnvelopeFrom, secondBounce, roots); err != nil {
		t.Fatal("bounce retry was not accepted", err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM bounce_receipts WHERE delivery_id=$1`, start.DeliveryId).Scan(&bounceCount); err != nil || bounceCount != 2 {
		t.Fatal("bounce retry did not create exactly one receipt", bounceCount, err)
	}
	if err = dispatcher.DispatchNext(ctx); err != nil {
		t.Fatal("bounce retry webhook was not dispatched", err)
	}
	select {
	case got := <-hookReceived:
		if got != "bounce_received_unverified" {
			t.Fatal("bounce retry webhook claimed a different outcome", got)
		}
	case <-ctx.Done():
		t.Fatal("bounce retry webhook not received")
	}
	if err = dispatcher.DispatchNext(ctx); !errors.Is(err, webhook.ErrNoWork) {
		t.Fatal("bounce retry emitted duplicate webhooks", err)
	}
	for i := 0; i < 3; i++ {
		if err = <-smtpDialer.completed; err != nil {
			t.Fatal("SMTP test server failed", err)
		}
	}
	// Control is authoritative for the recipient allowlist: the Edge no longer
	// checks it. Acceptance still allows this recipient, but control's dispatch
	// recheck now denies it. Both the first delivery and an explicit release must
	// stay held at claim time, without an SMTP permit or an Edge start.
	controlPolicy.allowed.Store(false)
	heldRequest := httptest.NewRequest("POST", "/emails", strings.NewReader(`{"from":"sender@example.com","to":["recipient@example.net"],"subject":"Policy hold","text":"Body"}`))
	heldRequest.Header.Set("Authorization", "Bearer "+apiKey)
	heldRequest.Header.Set("X-Thinpost-Enhanced-Protection", "true")
	heldResponse := httptest.NewRecorder()
	httpapi.New(acceptor).ServeHTTP(heldResponse, heldRequest)
	if heldResponse.Code != http.StatusOK {
		t.Fatal("policy hold submission failed", heldResponse.Code)
	}
	var heldReceipt struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(heldResponse.Body.Bytes(), &heldReceipt); err != nil || heldReceipt.ID == "" {
		t.Fatal("invalid held submission receipt", err)
	}
	// Provider adapters return provider-shaped IDs; the database keys on the 32-hex form.
	if heldReceipt.ID, _ = providerid.Normalize(heldReceipt.ID); heldReceipt.ID == "" {
		t.Fatal("unrecognized held submission ID")
	}
	if err = preparer.PrepareNext(ctx); err != nil {
		t.Fatal("held message preparation", err)
	}
	var heldDelivery string
	if err = db.QueryRowContext(ctx, `SELECT id FROM deliveries WHERE message_id=$1`, heldReceipt.ID).Scan(&heldDelivery); err != nil {
		t.Fatal(err)
	}
	for cycle := 0; cycle < 2; cycle++ {
		for {
			var state string
			var denied int
			var authorized bool
			err = db.QueryRowContext(ctx, `SELECT d.state,(SELECT count(*) FROM delivery_attempts a WHERE a.delivery_id=d.id AND a.outcome='recipient_policy_denied'),
				COALESCE((SELECT bool_or(a.start_authorized) FROM delivery_attempts a WHERE a.delivery_id=d.id),false) FROM deliveries d WHERE d.id=$1`, heldDelivery).Scan(&state, &denied, &authorized)
			if err != nil {
				t.Fatal(err)
			}
			items, listErr := j.List()
			if listErr != nil {
				t.Fatal(listErr)
			}
			if state == "recipient_policy_held" && denied == cycle+1 {
				if authorized || len(items) != 0 || smtpDialer.dials.Load() != 3 {
					t.Fatal("denied recipient received permit, Edge start or SMTP dial")
				}
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("policy hold not committed")
			case <-time.After(10 * time.Millisecond):
			}
		}
		select {
		case <-started:
			t.Fatal("control-denied recipient reached the Edge")
		default:
		}
		if cycle == 0 {
			if err = repo.RetryPolicyHold(ctx, "operator", app, heldReceipt.ID, heldDelivery); err != nil {
				t.Fatal("policy hold release", err)
			}
		}
	}
	second := *runner
	second.ControllerID = "controller-two"
	if err = second.Run(ctx); !errors.Is(err, postgres.ErrLeaseHeld) {
		t.Fatal("second controller acquired an active lease", err)
	}
	cancelFirst()
	<-done
	const orphanMessage = "00000000000000000000000000000005"
	const orphanDelivery = "00000000000000000000000000000006"
	const orphanAttempt = "00000000000000000000000000000007"
	if _, err = db.ExecContext(ctx, `INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'submissions/orphan',true,'prepared')`, orphanMessage, app); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,'final/orphan')`, orphanDelivery, orphanMessage); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE deliveries SET state='in_flight',current_attempt_id=$2 WHERE id=$1`, orphanDelivery, orphanAttempt); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO delivery_attempts(id,delivery_id,worker_id,epoch,start_authorized) VALUES ($1,$2,'worker',$3,false)`, orphanAttempt, orphanDelivery, start.Epoch); err != nil {
		t.Fatal(err)
	}
	secondCtx, cancelSecond := context.WithCancel(ctx)
	defer cancelSecond()
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.Run(secondCtx) }()
	for {
		var state string
		if err = db.QueryRowContext(ctx, `SELECT state FROM deliveries WHERE id=$1`, orphanDelivery).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "uncertain" {
			break
		}
		select {
		case err = <-secondDone:
			t.Fatal("second controller stopped before reconciliation", err)
		case <-ctx.Done():
			t.Fatal("second controller did not hold orphan")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case <-started:
		t.Fatal("orphan was sent instead of held")
	default:
	}
	cancelSecond()
	<-secondDone
	protected, err := tracking.NewProtectedStore(objects)
	if err != nil {
		t.Fatal(err)
	}
	linkKeys, err := tracking.NewLinkKeys([]byte("0123456789abcdef0123456789abcdef"), nil)
	if err != nil {
		t.Fatal(err)
	}
	preparer.Tracking = &preparation.TrackingConfig{Origin: "https://tracking.example.com", Links: linkKeys, EventLimit: 10, Objects: protected}
	if _, err = repo.SetTrackingQuota(ctx, "operator", app, 10); err != nil {
		t.Fatal(err)
	}
	trackedRequest := httptest.NewRequest("POST", "/email", strings.NewReader(`{"From":"sender@example.com","To":"first@example.net,second@example.net","Subject":"Tracked","HtmlBody":"<a href=\"https://example.net/private?case=fixture\">link</a>","ReplyTo":"help@example.com","TrackOpens":true,"TrackLinks":"HtmlOnly"}`))
	trackedRequest.Header.Set("X-Postmark-Server-Token", apiKey)
	trackedRequest.Header.Set("X-Thinpost-Enhanced-Protection", "true")
	trackedResponse := httptest.NewRecorder()
	httpapi.NewWithOptions(acceptor, httpapi.Options{Tracking: true}).ServeHTTP(trackedResponse, trackedRequest)
	var trackedReceipt struct{ MessageID string }
	if trackedResponse.Code != http.StatusOK || json.Unmarshal(trackedResponse.Body.Bytes(), &trackedReceipt) != nil || trackedReceipt.MessageID == "" {
		t.Fatal("tracked submission failed", trackedResponse.Code)
	}
	if trackedReceipt.MessageID, _ = providerid.Normalize(trackedReceipt.MessageID); trackedReceipt.MessageID == "" {
		t.Fatal("unrecognized tracked submission ID")
	}
	if err = preparer.PrepareNext(ctx); err != nil {
		t.Fatal("tracked preparation", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT d.final_object_key,t.object_key FROM deliveries d JOIN tracking_objects t ON t.delivery_id=d.id WHERE d.message_id=$1 ORDER BY d.recipient_index`, trackedReceipt.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	type trackedKeys struct{ final, mapping string }
	var trackedObjects []trackedKeys
	for rows.Next() {
		var item trackedKeys
		if err = rows.Scan(&item.final, &item.mapping); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		trackedObjects = append(trackedObjects, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if len(trackedObjects) != 2 || trackedObjects[0].mapping == trackedObjects[1].mapping {
		t.Fatal("tracking mappings were not committed independently")
	}
	pathPattern := regexp.MustCompile(`https://tracking\.example\.com/t/[oc]/v2/[0-9a-f]{32}/[A-Za-z0-9_-]+`)
	handler := controlapp.RouterWithRuntime(http.NotFoundHandler(), nil, nil, nil, controlapp.MailRuntime{Tracking: &tracking.Handler{Origin: "https://tracking.example.com", Links: linkKeys, Visitor: &tracking.Service{Repository: repo, Objects: protected}}})
	seenPaths := make(map[string]bool)
	for _, item := range trackedObjects {
		mime, readErr := objects.Get(ctx, item.final, 16<<20)
		if readErr != nil {
			t.Fatal(readErr)
		}
		results, verifyErr := dkim.VerifyWithOptions(bytes.NewReader(mime), &dkim.VerifyOptions{LookupTXT: func(name string) ([]string, error) {
			if name != "mail1._domainkey.example.com" {
				return nil, errors.New("unexpected signing domain")
			}
			return []string{dkimRecord}, nil
		}})
		if verifyErr != nil || len(results) != 1 || results[0].Err != nil {
			t.Fatal("tracked MIME did not verify")
		}
		parsed, parseErr := mail.ReadMessage(bytes.NewReader(mime))
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		html, readErr := io.ReadAll(quotedprintable.NewReader(parsed.Body))
		if readErr != nil {
			t.Fatal(readErr)
		}
		paths := pathPattern.FindAllString(string(html), -1)
		if len(paths) != 2 || bytes.Contains(html, []byte("https://example.net/private")) {
			t.Fatal("tracked MIME did not contain exactly two protected links")
		}
		for _, path := range paths {
			if seenPaths[path] {
				t.Fatal("tracking capability reused across recipients")
			}
			seenPaths[path] = true
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if strings.Contains(path, "/t/c/") && (response.Code != http.StatusFound || response.Header().Get("Location") != "https://example.net/private?case=fixture") {
				t.Fatal("committed click did not resolve")
			}
			if strings.Contains(path, "/t/o/") && response.Code != http.StatusOK {
				t.Fatal("committed open did not resolve")
			}
		}
		ciphertext, rawErr := rawObjects.Get(ctx, item.mapping, 16<<20)
		if rawErr != nil || len(ciphertext) == 0 || bytes.Contains(ciphertext, []byte("https://example.net/private")) {
			t.Fatal("tracking destination was not encrypted")
		}
	}
	// This fixture has already covered first-route failure above. Start the two
	// tracked deliveries on the working simulated route to exercise their
	// indefinite tracking through the real mTLS stream and SMTP worker.
	if _, err = db.ExecContext(ctx, `UPDATE deliveries SET route_attempts=1 WHERE message_id=$1`, trackedReceipt.MessageID); err != nil {
		t.Fatal(err)
	}
	controlPolicy.allowed.Store(true)
	third := *runner
	third.ControllerID = "controller-three"
	thirdCtx, cancelThird := context.WithCancel(ctx)
	defer cancelThird()
	thirdDone := make(chan error, 1)
	go func() { thirdDone <- third.Run(thirdCtx) }()
	for range 2 {
		var trackedStart *controlv1.DeliveryStart
		select {
		case trackedStart = <-started:
		case err = <-thirdDone:
			t.Fatal("tracked controller stopped", err)
		case <-ctx.Done():
			t.Fatal("tracked delivery not executed")
		}
		var expiry sql.NullTime
		var finalKey string
		if err = db.QueryRowContext(ctx, `SELECT t.expires_at-t.post_send_validity_seconds*interval '1 second',d.final_object_key FROM deliveries d JOIN tracking_objects t ON t.delivery_id=d.id WHERE d.id=$1`, trackedStart.DeliveryId).Scan(&expiry, &finalKey); err != nil || expiry.Valid || trackedStart.TrackingSendCutoffUnixMs != 0 {
			t.Fatal("indefinite tracking acquired a sending deadline", err)
		}
		finalMIME, readErr := objects.Get(ctx, finalKey, 16<<20)
		if readErr != nil {
			t.Fatal(readErr)
		}
		select {
		case received := <-smtpDialer.received:
			if !bytes.Equal(received, finalMIME) {
				t.Fatal("tracked SMTP did not use its committed MIME")
			}
		case <-ctx.Done():
			t.Fatal("tracked SMTP did not receive MIME")
		}
		select {
		case smtpErr := <-smtpDialer.completed:
			if smtpErr != nil {
				t.Fatal("tracked SMTP fixture", smtpErr)
			}
		case <-ctx.Done():
			t.Fatal("tracked SMTP did not finish")
		}
	}
	for {
		var accepted int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM deliveries WHERE message_id=$1 AND state='accepted'`, trackedReceipt.MessageID).Scan(&accepted); err != nil {
			t.Fatal(err)
		}
		items, listErr := j.List()
		if listErr != nil {
			t.Fatal(listErr)
		}
		if accepted == 2 && len(items) == 0 {
			break
		}
		select {
		case err = <-thirdDone:
			t.Fatal("tracked controller stopped before acknowledgement", err)
		case <-ctx.Done():
			t.Fatal("tracked results not committed and acknowledged")
		case <-time.After(10 * time.Millisecond):
		}
	}
	var validAfterSend bool
	if err = db.QueryRowContext(ctx, `SELECT bool_and(t.post_send_validity_seconds IS NULL AND t.expires_at IS NULL) FROM tracking_objects t JOIN deliveries d ON d.id=t.delivery_id WHERE d.message_id=$1`, trackedReceipt.MessageID).Scan(&validAfterSend); err != nil || !validAfterSend {
		t.Fatal("tracked messages acquired an expiry", err)
	}
	cancelThird()
	<-thirdDone
}
