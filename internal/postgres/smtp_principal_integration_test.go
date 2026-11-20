package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/smtpcred"
	"github.com/zyno-io/thinpost/internal/submission"
)

// smtpCommitFixture is an app with a verified sender domain, an API key, an
// SMTP credential and a live controller lease: every principal the shared
// submission paths dispatch on.
type smtpCommitFixture struct {
	t      *testing.T
	ctx    context.Context
	repo   *Repository
	exec   func(string, ...any)
	count  func(string, ...any) int
	app    string
	key    string
	cred   string
	secret string
	digest [32]byte
	lease  ControllerLease
}

func newSMTPCommitFixture(t *testing.T) *smtpCommitFixture {
	t.Helper()
	ctx, repo, exec, count := smtpOwnerFixture(t)
	repo.Objects = objectVerifierFunc(func(context.Context, string) error { return nil })
	f := &smtpCommitFixture{t: t, ctx: ctx, repo: repo, exec: exec, count: count, app: strings.Repeat("a", 32), key: strings.Repeat("b", 32)}
	exec(`INSERT INTO apps(id,name,pending_message_limit,recipient_daily_limit,submission_rate_per_minute,submission_burst) VALUES ($1,'A',1000,1000,6000,100)`, f.app)
	exec(`UPDATE apps SET submission_tokens=100::bigint*60000000,submission_refilled_at=clock_timestamp() WHERE id=$1`, f.app)
	exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'example.com',true,now())`, f.app)
	keyDigest := sha256.Sum256([]byte("api-key-secret"))
	exec(`INSERT INTO api_keys(id,digest,app_id) VALUES ($1,$2,$3)`, f.key, keyDigest[:], f.app)
	issued, err := repo.IssueSMTPCredential(ctx, "operator", f.app, "printer")
	if err != nil {
		t.Fatal(err)
	}
	f.cred, f.secret, f.digest = issued.Username, issued.Password, sha256.Sum256([]byte(issued.Password))
	f.lease, err = repo.AcquireControllerLease(ctx, "worker-1", "controller-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *smtpCommitFixture) fence() submission.Fence {
	return submission.Fence{WorkerID: f.lease.WorkerID, ControllerID: f.lease.ControllerID, Epoch: f.lease.Epoch}
}

// smtpRecord is a valid raw record for the SMTP principal.
func (f *smtpCommitFixture) smtpRecord() acceptance.Record {
	return acceptance.Record{
		AppID: f.app, SMTPCredentialID: f.cred, SMTPCredentialDigest: f.digest, Raw: true, Fence: f.fence(),
		ObjectKey: fixtureObjectKey("submissions", f.randomID()), Adapter: submission.AdapterSMTP,
		From: "sender@example.com", SenderAddress: "sender@example.com", SenderDomain: "example.com",
		RecipientCount: 1, Recipients: []string{"rcpt@example.net"}, RecipientKinds: []string{"to"}, Subject: "Hi",
	}
}

func (f *smtpCommitFixture) httpRecord() acceptance.Record {
	return withMetadata(acceptance.Record{AppID: f.app, KeyID: f.key, ObjectKey: fixtureObjectKey("submissions", f.randomID()), Adapter: "resend", From: "sender@example.com", RecipientCount: 1})
}

func (f *smtpCommitFixture) randomID() string {
	id, err := newOpaqueID()
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// A key, an operator and an SMTP principal are separate: the SMTP adapter and
// the raw path belong to the SMTP principal alone, and a record naming two
// principals is refused before any row is written.
func TestCommitPrincipalsAreExclusive(t *testing.T) {
	f := newSMTPCommitFixture(t)

	for name, mutate := range map[string]func(*acceptance.Record){
		"key on the smtp adapter": func(r *acceptance.Record) { r.Adapter = submission.AdapterSMTP },
		"key with the raw path":   func(r *acceptance.Record) { r.Raw = true },
		"key with a credential":   func(r *acceptance.Record) { r.SMTPCredentialID, r.Fence = f.cred, f.fence() },
		"key with a credential, smtp": func(r *acceptance.Record) {
			r.SMTPCredentialID, r.Adapter, r.Raw, r.Fence = f.cred, submission.AdapterSMTP, true, f.fence()
		},
		"key with a ttl": func(r *acceptance.Record) { r.IdempotencyKey, r.IdempotencyTTL = "k", time.Hour },
	} {
		record := f.httpRecord()
		mutate(&record)
		if _, err := f.repo.Commit(f.ctx, record); !errors.Is(err, submission.ErrInvalidKey) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, mutate := range map[string]func(*acceptance.Record){
		"credential and key":      func(r *acceptance.Record) { r.KeyID = f.key },
		"credential and operator": func(r *acceptance.Record) { r.Operator = "admin" },
		"wrong adapter":           func(r *acceptance.Record) { r.Adapter = "resend" },
		"console adapter":         func(r *acceptance.Record) { r.Adapter = submission.AdapterConsole },
		"not raw":                 func(r *acceptance.Record) { r.Raw = false },
		"limits override":         func(r *acceptance.Record) { r.LimitsOverride = true },
		"policy override":         func(r *acceptance.Record) { r.RecipientPolicyOverride = true },
		"another digest":          func(r *acceptance.Record) { r.SMTPCredentialDigest = sha256.Sum256([]byte("guess")) },
		"unknown credential":      func(r *acceptance.Record) { r.SMTPCredentialID = strings.Repeat("9", 32) },
	} {
		record := f.smtpRecord()
		mutate(&record)
		if _, err := f.repo.Commit(f.ctx, record); !errors.Is(err, submission.ErrInvalidKey) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// An operator record may not carry SMTP fields either.
	operator := withMetadata(acceptance.Record{AppID: f.app, Operator: "admin", ObjectKey: fixtureObjectKey("submissions", f.randomID()), Adapter: submission.AdapterConsole, From: "sender@example.com", RecipientCount: 1})
	for name, mutate := range map[string]func(*acceptance.Record){
		"smtp adapter": func(r *acceptance.Record) { r.Adapter = submission.AdapterSMTP },
		"raw path":     func(r *acceptance.Record) { r.Raw = true },
		"a credential": func(r *acceptance.Record) { r.SMTPCredentialID, r.Fence = f.cred, f.fence() },
		"a credential,": func(r *acceptance.Record) {
			r.SMTPCredentialID, r.Adapter, r.Fence = f.cred, submission.AdapterSMTP, f.fence()
		},
	} {
		record := operator
		mutate(&record)
		if _, err := f.repo.Commit(f.ctx, record); !errors.Is(err, submission.ErrInvalidKey) {
			t.Fatalf("operator with %s: %v", name, err)
		}
	}
	if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("a refused principal left %d messages", n)
	}

	// The two older principals still commit exactly as before.
	if _, err := f.repo.Commit(f.ctx, f.httpRecord()); err != nil {
		t.Fatal("key", err)
	}
	if _, err := f.repo.Commit(f.ctx, operator); err != nil {
		t.Fatal("operator", err)
	}
	if n := f.count(`SELECT count(*) FROM messages WHERE smtp_credential_id IS NULL AND original_from_address IS NULL`); n != 2 {
		t.Fatalf("HTTP and console rows carry SMTP columns: %d", n)
	}
}

// HTTP and console records keep their old shape: an empty subject and an
// all-Bcc recipient list stay refused, and a paused app reads as an invalid
// message to a key and as paused to an operator.
func TestCommitHTTPRulesAreUnchanged(t *testing.T) {
	f := newSMTPCommitFixture(t)
	record := f.httpRecord()
	record.Subject = ""
	if _, err := f.repo.Commit(f.ctx, record); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("an HTTP record with an empty subject", err)
	}
	record = f.httpRecord()
	record.RecipientKinds = []string{"bcc"}
	if _, err := f.repo.Commit(f.ctx, record); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("an HTTP record with only a Bcc recipient", err)
	}
	var rows recipientRows
	if rows.add("m", []string{"a@example.net"}, []string{"bcc"}, 1, false) || !rows.add("m", []string{"a@example.net"}, []string{"bcc"}, 1, true) {
		t.Fatal("only the raw path may start with a non-To recipient")
	}
	if rows.add("m", []string{"a@example.net", "b@example.net"}, []string{"bcc", "to"}, 2, true) {
		t.Fatal("the recipient kinds must stay ordered")
	}

	f.exec(`UPDATE apps SET active=false WHERE id=$1`, f.app)
	if _, err := f.repo.Commit(f.ctx, f.httpRecord()); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("a key on a paused app", err)
	}
	operator := withMetadata(acceptance.Record{AppID: f.app, Operator: "admin", ObjectKey: fixtureObjectKey("submissions", f.randomID()), Adapter: submission.AdapterConsole, From: "sender@example.com", RecipientCount: 1})
	if _, err := f.repo.Commit(f.ctx, operator); !errors.Is(err, submission.ErrAppPaused) {
		t.Fatal("an operator on a paused app", err)
	}
	if err := f.repo.ReserveSubmission(f.ctx, f.app, f.key, 1); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("a key reservation on a paused app", err)
	}
	if err := f.repo.ReserveOperatorSubmission(f.ctx, f.app, 1, true); !errors.Is(err, submission.ErrAppPaused) {
		t.Fatal("an operator reservation on a paused app", err)
	}
}

// HTTP idempotency rows keep the fixed 24-hour window and the batch adapters
// keep their sentinels.
func TestHTTPIdempotencyKeepsTheLiteralWindow(t *testing.T) {
	f := newSMTPCommitFixture(t)
	record := f.httpRecord()
	record.IdempotencyKey = "request-1"
	record.RequestHash = sha256.Sum256([]byte("one"))
	receipt, err := f.repo.Commit(f.ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	var window time.Duration
	var seconds float64
	if err = f.repo.DB.QueryRowContext(f.ctx, `SELECT extract(epoch FROM i.expires_at - m.accepted_at) FROM idempotency_keys i JOIN messages m ON m.id=i.message_id WHERE i.message_id=$1`, receipt.ID).Scan(&seconds); err != nil {
		t.Fatal(err)
	}
	window = time.Duration(seconds * float64(time.Second))
	if window < 24*time.Hour-time.Second || window > 24*time.Hour+time.Second {
		t.Fatalf("HTTP idempotency window %v", window)
	}
	again := f.httpRecord()
	again.IdempotencyKey, again.RequestHash = "request-1", record.RequestHash
	if replay, err := f.repo.Commit(f.ctx, again); err != nil || !replay.Replayed || replay.ID != receipt.ID {
		t.Fatal("replay", replay, err)
	}
	again = f.httpRecord()
	again.IdempotencyKey, again.RequestHash = "request-1", sha256.Sum256([]byte("two"))
	if _, err := f.repo.Commit(f.ctx, again); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatal("conflict", err)
	}
	// A batch commit never accepts the SMTP principal. The batch is otherwise
	// valid and carries a real key, so the credential is the only thing refused.
	item := func() acceptance.BatchItem {
		return withItemMetadata(acceptance.BatchItem{ObjectKey: fixtureObjectKey("submissions", f.randomID()), From: "sender@example.com", RecipientCount: 1})
	}
	batch := acceptance.BatchRecord{Record: acceptance.Record{AppID: f.app, KeyID: f.key, SMTPCredentialID: f.cred, Adapter: "resend"}, Items: []acceptance.BatchItem{item()}}
	if _, err := f.repo.CommitBatch(f.ctx, batch); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("batch with a credential", err)
	}
	batch.SMTPCredentialID = ""
	batch.Items = []acceptance.BatchItem{item()}
	if _, err := f.repo.CommitBatch(f.ctx, batch); err != nil {
		t.Fatal("the same batch without the credential must commit", err)
	}
}

func TestReserveSMTPSubmissionChecksTheCredential(t *testing.T) {
	f := newSMTPCommitFixture(t)
	if err := f.repo.ReserveSMTPSubmission(f.ctx, f.app, f.cred, f.digest); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.ReserveSMTPSubmission(f.ctx, f.app, f.cred, sha256.Sum256([]byte("wrong"))); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("wrong digest", err)
	}
	other := strings.Repeat("c", 32)
	f.exec(`INSERT INTO apps(id,name,submission_rate_per_minute,submission_burst) VALUES ($1,'Other',6000,100)`, other)
	f.exec(`UPDATE apps SET submission_tokens=100::bigint*60000000,submission_refilled_at=clock_timestamp() WHERE id=$1`, other)
	otherIssued, err := f.repo.IssueSMTPCredential(f.ctx, "operator", other, "other")
	if err != nil {
		t.Fatal(err)
	}
	otherDigest := sha256.Sum256([]byte(otherIssued.Password))
	if err := f.repo.ReserveSMTPSubmission(f.ctx, other, f.cred, f.digest); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("the first app's credential must be refused for another app", err)
	}
	if err := f.repo.ReserveSMTPSubmission(f.ctx, f.app, otherIssued.Username, otherDigest); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("another app's credential must be refused for this app", err)
	}
	if err := f.repo.ReserveSMTPSubmission(f.ctx, other, otherIssued.Username, otherDigest); err != nil {
		t.Fatal("the other app's own credential", err)
	}
	f.exec(`UPDATE apps SET active=false WHERE id=$1`, f.app)
	if err := f.repo.ReserveSMTPSubmission(f.ctx, f.app, f.cred, f.digest); !errors.Is(err, submission.ErrAppPaused) {
		t.Fatal("paused", err)
	}
	f.exec(`UPDATE apps SET active=true WHERE id=$1`, f.app)
	f.exec(`UPDATE smtp_credentials SET revoked_at=clock_timestamp() WHERE id=$1`, f.cred)
	if err := f.repo.ReserveSMTPSubmission(f.ctx, f.app, f.cred, f.digest); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("revoked", err)
	}
	// An exhausted bucket is rate limited, and a key reservation still works
	// against the same bucket.
	f.exec(`UPDATE apps SET submission_tokens=0`)
	if err := f.repo.ReserveSubmission(f.ctx, f.app, f.key, 1); !errors.Is(err, submission.ErrRateLimited) {
		t.Fatal("key on an empty bucket", err)
	}
	_ = smtpcred.SecretPrefix
}

// HTTP keeps ErrInvalidMessage where the SMTP path reports server-side faults
// with their own sentinels.
func TestSentinelsOnHTTPAndSMTPPaths(t *testing.T) {
	f := newSMTPCommitFixture(t)
	f.exec(`UPDATE sender_domains SET active=false`)
	if _, err := f.repo.Commit(f.ctx, f.httpRecord()); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("HTTP with an inactive sender domain", err)
	}
	if _, err := f.repo.Commit(f.ctx, f.smtpRecord()); !errors.Is(err, submission.ErrSenderDomainInactive) || errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("SMTP with an inactive sender domain", err)
	}
	f.exec(`UPDATE sender_domains SET active=true`)
	record := f.smtpRecord()
	record.Fence.Epoch = 0
	if _, err := f.repo.Commit(f.ctx, record); !errors.Is(err, delivery.ErrLeaseInvalid) {
		t.Fatal("a malformed fence", err)
	}
	record = f.smtpRecord()
	record.IdempotencyKey = "smtp1:x"
	if _, err := f.repo.Commit(f.ctx, record); !errors.Is(err, ErrSMTPRecordInvalid) || errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("a key without a TTL", err)
	}
}
