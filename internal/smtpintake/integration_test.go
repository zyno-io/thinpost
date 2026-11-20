package smtpintake

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/objectstore"
	"github.com/zyno-io/thinpost/internal/postgres"
	"github.com/zyno-io/thinpost/internal/submission"
)

const testGeneration = "00000000000000000000000000000dde"

type verifierFunc func(context.Context, string) error

func (f verifierFunc) VerifyPresent(ctx context.Context, key string) error { return f(ctx, key) }

// dbFixture is a migrated schema with an app, a verified sender domain, an SMTP
// credential and a live controller lease, run through the whole intake stack.
type dbFixture struct {
	t      *testing.T
	ctx    context.Context
	db     *sql.DB
	repo   *postgres.Repository
	app    string
	cred   string
	secret string
	fence  submission.Fence
	ring   *objectstore.Keyring
	signer *fakeSigning
	svc    *Service
}

func newKeyringFile(t *testing.T, active string, ids ...string) string {
	t.Helper()
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		// Deterministic per ID, so every replica agrees on a version's key.
		sum := sha256.Sum256([]byte("test keyring " + id))
		keys = append(keys, fmt.Sprintf("%q:%q", id, base64.StdEncoding.EncodeToString(sum[:])))
	}
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"active":%q,"keys":{%s}}`, active, strings.Join(keys, ","))), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadRing(t *testing.T, active string, ids ...string) *objectstore.Keyring {
	t.Helper()
	ring, err := objectstore.LoadKeyring(newKeyringFile(t, active, ids...))
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func newDBFixture(t *testing.T) *dbFixture {
	t.Helper()
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL database")
	}
	ctx := context.Background()
	root, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "intake_" + hex.EncodeToString(random[:])
	if _, err = root.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = postgres.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	f := &dbFixture{t: t, ctx: ctx, db: db, app: strings.Repeat("a", 32), signer: &fakeSigning{}}
	f.exec(`UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, testGeneration)
	f.repo = &postgres.Repository{DB: db, Generation: testGeneration, Objects: verifierFunc(func(context.Context, string) error { return nil })}
	f.exec(`INSERT INTO apps(id,name,pending_message_limit,recipient_daily_limit,submission_rate_per_minute,submission_burst) VALUES ($1,'A',1000,1000,6000,100)`, f.app)
	f.exec(`UPDATE apps SET submission_tokens=100::bigint*60000000,submission_refilled_at=clock_timestamp() WHERE id=$1`, f.app)
	f.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'example.com',true,now())`, f.app)
	issued, err := f.repo.IssueSMTPCredential(ctx, "operator", f.app, "printer")
	if err != nil {
		t.Fatal(err)
	}
	f.cred, f.secret = issued.Username, issued.Password
	lease, err := f.repo.AcquireControllerLease(ctx, "worker-1", "controller-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.fence = submission.Fence{WorkerID: lease.WorkerID, ControllerID: lease.ControllerID, Epoch: lease.Epoch}
	f.ring = loadRing(t, "v1", "v1")
	f.svc = f.replica(f.ring, []byte("0123456789abcdef0123456789abcdef"), allow(true))
	return f
}

// replica builds one control replica's intake service: its own keyring and
// fingerprint key, the shared database.
func (f *dbFixture) replica(ring *objectstore.Keyring, fingerprintKey []byte, recipients acceptance.RecipientPolicy) *Service {
	accept, err := acceptance.New(f.repo, &memObjects{}, fingerprintKey, recipients)
	if err != nil {
		f.t.Fatal(err)
	}
	return &Service{Repository: f.repo, Acceptance: accept, Signing: f.signer, Replay: &ReplaySecret{Store: f.repo, Keys: ring}}
}

func (f *dbFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.ExecContext(f.ctx, query, args...); err != nil {
		f.t.Fatal(query, err)
	}
}

func (f *dbFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRowContext(f.ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatal(query, err)
	}
	return n
}

func (f *dbFixture) message(body string) Message {
	return Message{TransferID: strings.Repeat("c", 32), CredentialID: f.cred, Secret: f.secret, MailFrom: "sender@example.com", Rcpts: []string{"rcpt@example.net"},
		ClientIP: netip.MustParseAddr("192.0.2.9"), TLS: true, Received: time.Now(), Body: []byte(body)}
}

func (f *dbFixture) submit(body string) Result { return f.submitWith(f.svc, body) }

func (f *dbFixture) submitWith(s *Service, body string) Result {
	return s.Submit(f.ctx, f.fence, f.message(body))
}

func (f *dbFixture) messages() int { return f.count("SELECT count(*) FROM messages") }

func TestIntakeAcceptsAndReplaysByMessageID(t *testing.T) {
	f := newDBFixture(t)
	first := f.submit(withID)
	if first.Outcome != Accepted || first.MessageID == "" || first.Held {
		t.Fatal(first)
	}
	var key string
	var window float64
	if err := f.db.QueryRowContext(f.ctx, `SELECT key,extract(epoch FROM expires_at-clock_timestamp())/3600 FROM idempotency_keys WHERE adapter='smtp'`).Scan(&key, &window); err != nil || !strings.HasPrefix(key, "smtp1:") || window < 23.9 {
		t.Fatal(key, window, err)
	}
	// The row carries the credential and the sender, and nothing is held.
	// withID has no Subject, so the row carries an empty one.
	if n := f.count("SELECT count(*) FROM messages WHERE id=$1 AND smtp_credential_id=$2 AND subject=''", first.MessageID, f.cred); n != 1 {
		t.Fatal("the message row", n)
	}
	// A retry after a lost acknowledgement.
	again := f.submit(withID)
	if again.Outcome != Accepted || again.MessageID != first.MessageID || f.messages() != 1 {
		t.Fatal("retry", again, f.messages())
	}
	// Even if the app is paused before the retry.
	f.exec(`UPDATE apps SET active=false`)
	if again = f.submit(withID); again.Outcome != Accepted || again.MessageID != first.MessageID {
		t.Fatal("retry on a paused app", again)
	}
	if fresh := f.submit(strings.Replace(withID, "one@", "two@", 1)); fresh.Outcome != AppPaused {
		t.Fatal("a fresh message on a paused app", fresh)
	}
	f.exec(`UPDATE apps SET active=true`)
	// Message-ID reuse with different content is its own message and deduplicates itself.
	reused := strings.Replace(withID, "body", "different body", 1)
	other := f.submit(reused)
	if other.Outcome != Accepted || other.MessageID == first.MessageID || f.submit(reused).MessageID != other.MessageID || f.messages() != 2 {
		t.Fatal("Message-ID reuse", other)
	}
}

func TestIntakeReplayKindC(t *testing.T) {
	f := newDBFixture(t)
	first := f.submit(dated)
	if first.Outcome != Accepted {
		t.Fatal(first)
	}
	var key string
	var seconds float64
	if err := f.db.QueryRowContext(f.ctx, `SELECT key,extract(epoch FROM expires_at-clock_timestamp()) FROM idempotency_keys WHERE adapter='smtp'`).Scan(&key, &seconds); err != nil || !strings.HasPrefix(key, "smtp-c1:") || seconds < 590 || seconds > 600 {
		t.Fatal(key, seconds, err)
	}
	if retry := f.submit(dated); retry.MessageID != first.MessageID || f.messages() != 1 {
		t.Fatal("a retry without a Message-ID but with a Date", retry)
	}
	// Two cron alerts with identical bodies but different Date: two messages.
	later := strings.Replace(dated, "12:00:00", "12:05:00", 1)
	if second := f.submit(later); second.Outcome != Accepted || second.MessageID == first.MessageID || f.messages() != 2 {
		t.Fatal("different Date", second)
	}
	// Identical bytes after the window: a new message that reuses the expired row.
	f.exec(`UPDATE idempotency_keys SET expires_at=clock_timestamp()-interval '1 second' WHERE key=$1`, key)
	rows := f.count("SELECT count(*) FROM idempotency_keys")
	resend := f.submit(dated)
	if resend.Outcome != Accepted || resend.MessageID == first.MessageID || f.messages() != 3 || f.count("SELECT count(*) FROM idempotency_keys") != rows {
		t.Fatal("resend after 10 minutes", resend)
	}
	if n := f.count("SELECT count(*) FROM idempotency_keys WHERE key=$1 AND message_id=$2", key, resend.MessageID); n != 1 {
		t.Fatal("the expired row was not reused")
	}
	// The same bytes with a Message-ID are kind M: a different key.
	withBoth := strings.Replace(dated, "Subject: Hi\r\n", "Subject: Hi\r\nMessage-ID: <c@client.example.org>\r\n", 1)
	if m := f.submit(withBoth); m.Outcome != Accepted || f.count("SELECT count(*) FROM idempotency_keys WHERE key LIKE 'smtp1:%'") != 1 {
		t.Fatal("kind M", m)
	}
}

func TestIntakeWithNeitherHeaderHasNoReplayIdentity(t *testing.T) {
	f := newDBFixture(t)
	a, b := f.submit(neither), f.submit(neither)
	if a.Outcome != Accepted || b.Outcome != Accepted || a.MessageID == b.MessageID || f.messages() != 2 || f.count("SELECT count(*) FROM idempotency_keys") != 0 {
		t.Fatal(a, b)
	}
	// With nothing to key, the replay secret is not even needed.
	if f.count("SELECT count(*) FROM smtp_replay_secret") != 0 {
		t.Fatal("a replay secret was created for a message that has no replay identity")
	}
}

// A hit is answered whatever the bucket, policy, signing and app now say.
func TestIntakeReplayPrecedesRateAndPolicyAndSigning(t *testing.T) {
	f := newDBFixture(t)
	first := f.submit(withID)
	if first.Outcome != Accepted {
		t.Fatal(first)
	}
	f.exec(`UPDATE apps SET submission_tokens=0,submission_refilled_at=clock_timestamp(),smtp_from_mode='domain',smtp_from_domain='nowhere.example.org',active=false`)
	f.signer.err = fmt.Errorf("signing broken")
	if again := f.submit(withID); again.Outcome != Accepted || again.MessageID != first.MessageID {
		t.Fatal("replay with a drained bucket, changed policy, broken signing and a paused app", again)
	}
	f.exec(`UPDATE apps SET active=true`)
	if fresh := f.submit(strings.Replace(withID, "one@", "two@", 1)); fresh.Outcome != RateLimited && fresh.Outcome != Misconfigured {
		t.Fatal("a fresh message must be refused", fresh)
	}
}

func TestIntakeHeldReplayReturnsHeld(t *testing.T) {
	f := newDBFixture(t)
	held := "From: sender@example.com\r\nTo: rcpt@example.net\r\nX-Thinpost-Hold: true\r\nMessage-ID: <h@client.example.org>\r\n\r\nbody\r\n"
	first := f.submit(held)
	if first.Outcome != Accepted || !first.Held || f.count("SELECT count(*) FROM messages WHERE id=$1 AND state='on_hold'", first.MessageID) != 1 {
		t.Fatal(first)
	}
	again := f.submit(held)
	if again.Outcome != Accepted || !again.Held || again.MessageID != first.MessageID {
		t.Fatal("a replay of a held message returns held", again)
	}
}

// Three replicas with different keyrings and fingerprint keys derive the same
// replay key, find each other's messages, and never touch the sealed secret.
func TestIntakeFailbackAcrossReplicas(t *testing.T) {
	f := newDBFixture(t)
	sealed := func() []byte {
		var b []byte
		if err := f.db.QueryRowContext(f.ctx, `SELECT sealed FROM smtp_replay_secret`).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	fpA, fpB := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	// A: only v1, active v1. B: v1 and v2 with v2 active (a rotation in progress).
	// C: v1 and v2 with v1 active again (a rollback).
	a := f.replica(loadRing(t, "v1", "v1"), fpA, allow(true))
	b := f.replica(loadRing(t, "v2", "v1", "v2"), fpB, allow(true))
	c := f.replica(loadRing(t, "v1", "v1", "v2"), fpA, allow(true))

	first := f.submitWith(a, withID)
	if first.Outcome != Accepted {
		t.Fatal(first)
	}
	original := sealed()
	for name, replica := range map[string]*Service{"B": b, "C": c, "A": a} {
		if got := f.submitWith(replica, withID); got.Outcome != Accepted || got.MessageID != first.MessageID {
			t.Fatalf("replica %s did not find the message: %v", name, got)
		}
		if got := f.submitWith(replica, dated); got.Outcome != Accepted {
			t.Fatalf("replica %s: %v", name, got)
		}
	}
	if string(sealed()) != string(original) || f.count("SELECT count(*) FROM smtp_replay_secret") != 1 {
		t.Fatal("the sealed secret changed")
	}
	// The kind C message was accepted once, by whoever came first, and found by the rest.
	if n := f.count("SELECT count(*) FROM idempotency_keys WHERE key LIKE 'smtp-c1:%'"); n != 1 {
		t.Fatal("kind C rows", n)
	}
	if f.messages() != 2 {
		t.Fatal("messages", f.messages())
	}
}

// A replica whose keyring lacks the version the secret was sealed under fails
// closed; a replica that has it is unaffected.
func TestIntakeMissingKeyringVersionIsTempfail(t *testing.T) {
	f := newDBFixture(t)
	creator := f.replica(loadRing(t, "v2", "v1", "v2"), []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), allow(true))
	first := f.submitWith(creator, withID)
	if first.Outcome != Accepted {
		t.Fatal(first)
	}
	var before []byte
	if err := f.db.QueryRowContext(f.ctx, `SELECT sealed FROM smtp_replay_secret`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	stale := f.replica(loadRing(t, "v1", "v1"), []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), allow(true))
	if got := f.submitWith(stale, withID); got.Outcome != Tempfail {
		t.Fatal("a replica that cannot open the secret must answer a temporary failure", got)
	}
	if got := f.submitWith(stale, strings.Replace(withID, "one@", "two@", 1)); got.Outcome != Tempfail || f.messages() != 1 {
		t.Fatal("it must not accept fresh mail either", got)
	}
	// Messages without any replay identity do not need the secret.
	if got := f.submitWith(stale, neither); got.Outcome != Accepted {
		t.Fatal(got)
	}
	var after []byte
	if err := f.db.QueryRowContext(f.ctx, `SELECT sealed FROM smtp_replay_secret`).Scan(&after); err != nil || string(after) != string(before) {
		t.Fatal("a failing replica rewrote the secret", err)
	}
	// The replica recovers once it holds the version.
	fixed := f.replica(loadRing(t, "v2", "v1", "v2"), []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), allow(true))
	if got := f.submitWith(fixed, withID); got.MessageID != first.MessageID {
		t.Fatal(got)
	}
}

func TestIntakeFromShapesAndAllBcc(t *testing.T) {
	f := newDBFixture(t)
	long := strings.Repeat("Name ", 60) // folded on the wire, unfolded to one line
	foldedFrom := "From: \"" + strings.TrimSpace(long[:150]) + "\r\n \" <sender@example.com>\r\n"
	for name, c := range map[string]struct {
		body, address string
		name          string
	}{
		"folded long name": {foldedFrom + "To: rcpt@example.net\r\nMessage-ID: <f1@client.example.org>\r\n\r\nb\r\n", "sender@example.com", ""},
		"quoted name":      {"From: \"Doe, Jane\" <jane@example.com>\r\nTo: rcpt@example.net\r\nMessage-ID: <f2@client.example.org>\r\n\r\nb\r\n", "jane@example.com", "Doe, Jane"},
	} {
		res := f.submit(c.body)
		if res.Outcome != Accepted {
			t.Fatalf("%s: %v", name, res.Outcome)
		}
		var address string
		var senderName *string
		if err := f.db.QueryRowContext(f.ctx, `SELECT sender_address,sender_name FROM messages WHERE id=$1`, res.MessageID).Scan(&address, &senderName); err != nil || address != c.address {
			t.Fatalf("%s: %v %v", name, address, err)
		}
		if c.name != "" && (senderName == nil || *senderName != c.name) {
			t.Fatalf("%s: name %v", name, senderName)
		}
		if name == "folded long name" && (senderName == nil || strings.Contains(*senderName, "\r") || len(*senderName) < 100) {
			t.Fatalf("the display name was not unfolded: %v", senderName)
		}
	}
	// All-Bcc: no recipient appears in To or Cc.
	m := f.message("From: sender@example.com\r\nSubject: Quiet\r\nMessage-ID: <b2@client.example.org>\r\n\r\nb\r\n")
	m.Rcpts = []string{"one@example.net", "two@example.net"}
	res := f.svc.Submit(f.ctx, f.fence, m)
	if res.Outcome != Accepted || f.count("SELECT count(*) FROM message_recipients WHERE message_id=$1 AND kind='bcc'", res.MessageID) != 2 {
		t.Fatal("an all-Bcc message", res)
	}
	// Rewritten From records the original, unquoted.
	f.exec(`UPDATE apps SET smtp_from_mode='address',smtp_from_address='noreply@example.com',smtp_from_only_unauthorized=true`)
	m = f.message("From: \"Doe, Jane\" <jane@elsewhere.example.org>\r\nTo: rcpt@example.net\r\nMessage-ID: <r@client.example.org>\r\n\r\nb\r\n")
	res = f.svc.Submit(f.ctx, f.fence, m)
	var original, sender *string
	if res.Outcome != Accepted {
		t.Fatal(res)
	}
	if err := f.db.QueryRowContext(f.ctx, `SELECT original_from_address,sender_address FROM messages WHERE id=$1`, res.MessageID).Scan(&original, &sender); err != nil || original == nil || *original != "jane@elsewhere.example.org" || *sender != "noreply@example.com" {
		t.Fatal(original, sender, err)
	}
}

func TestIntakeFencingAndRevocation(t *testing.T) {
	f := newDBFixture(t)
	stale := f.fence
	stale.Epoch++
	if got := f.svc.Submit(f.ctx, stale, f.message(withID)); got.Outcome != Tempfail || f.messages() != 0 || f.count("SELECT count(*) FROM idempotency_keys") != 0 {
		t.Fatal("a stale fence", got)
	}
	// The retry under the right fence is accepted once.
	if got := f.submit(withID); got.Outcome != Accepted || f.messages() != 1 {
		t.Fatal(got)
	}
	f.exec(`UPDATE smtp_credentials SET revoked_at=clock_timestamp()`)
	if got := f.submit(withID); got.Outcome != CredentialInvalid {
		t.Fatal("a revoked credential", got)
	}
	if got := f.svc.Verify(f.ctx, f.cred, f.secret, netip.MustParseAddr("192.0.2.9")); got != AuthInvalid {
		t.Fatal(got)
	}
}

func TestIntakeAllowlistRefusesTheWholeMessage(t *testing.T) {
	f := newDBFixture(t)
	deny := f.replica(f.ring, []byte("0123456789abcdef0123456789abcdef"), allow(false))
	before := f.count("SELECT submission_tokens FROM apps")
	if got := f.submitWith(deny, withID); got.Outcome != RecipientNotPermitted || f.messages() != 0 {
		t.Fatal(got)
	}
	if f.count("SELECT submission_tokens FROM apps") != before {
		t.Fatal("a refused message consumed rate tokens")
	}
}

func TestIntakeSigningReadiness(t *testing.T) {
	f := newDBFixture(t)
	f.signer.err = fmt.Errorf("no key")
	if got := f.submit(withID); got.Outcome != Misconfigured || f.messages() != 0 {
		t.Fatal(got)
	}
	if len(f.signer.calls) != 1 || f.signer.calls[0] != f.app+"/example.com" {
		t.Fatal("readiness is checked for the effective sender domain", f.signer.calls)
	}
}

// Identical concurrent submissions are resolved by the primary-key conflict in
// Commit: one message, and every caller gets its ID.
func TestIntakeConcurrentIdenticalSubmits(t *testing.T) {
	for name, body := range map[string]string{"kind M": withID, "kind C": dated} {
		f := newDBFixture(t)
		const n = 8
		results := make(chan Result, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			go func() { <-start; results <- f.submit(body) }()
		}
		close(start)
		ids := map[string]int{}
		for i := 0; i < n; i++ {
			r := <-results
			if r.Outcome != Accepted {
				t.Fatalf("%s: %v", name, r.Outcome)
			}
			ids[r.MessageID]++
		}
		if len(ids) != 1 || f.messages() != 1 || f.count("SELECT count(*) FROM idempotency_keys") != 1 {
			t.Fatalf("%s: ids %v, messages %d", name, ids, f.messages())
		}
	}
}
