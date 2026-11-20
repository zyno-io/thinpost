package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/submission"
)

// sleepOnRecipientInsert makes every message_recipients insert take the given
// time, which holds the commit transaction open after its locks (the credential,
// the sender domain and the controller lease) are taken and before its final
// lease recheck. It is the only way to hold a commit at that point.
func (f *smtpCommitFixture) sleepOnRecipientInsert(seconds float64) {
	f.t.Helper()
	f.exec(fmt.Sprintf(`CREATE FUNCTION smtp_test_sleep() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(%v); RETURN NEW; END $$`, seconds))
	f.exec(`CREATE TRIGGER smtp_test_sleep BEFORE INSERT ON message_recipients FOR EACH ROW EXECUTE FUNCTION smtp_test_sleep()`)
}

// gatedVerifier blocks object checks until released and reports each arrival.
type gatedVerifier struct {
	arrived chan struct{}
	release chan struct{}
}

func newGatedVerifier() *gatedVerifier {
	return &gatedVerifier{arrived: make(chan struct{}, 8), release: make(chan struct{})}
}

func (g *gatedVerifier) VerifyPresent(ctx context.Context, _ string) error {
	g.arrived <- struct{}{}
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// lockWaiter counts backends of this database blocked on a heavyweight lock.
const lockWaiter = "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'"

// waitFor polls until the query returns a positive count, as a deterministic
// signal that a concurrent backend reached the state a test needs.
func (f *smtpCommitFixture) waitFor(what, query string) {
	f.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if f.count(query) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatal("timed out waiting for " + what)
}

func (f *smtpCommitFixture) messageCount() int { return f.count("SELECT count(*) FROM messages") }

func TestSMTPCommitRecordsTheRawMessage(t *testing.T) {
	f := newSMTPCommitFixture(t)
	record := f.smtpRecord()
	record.Subject = ""
	record.Recipients, record.RecipientKinds, record.RecipientCount = []string{"one@example.net", "two@example.net"}, []string{"bcc", "bcc"}, 2
	record.OriginalFromAddress = "someone@elsewhere.example.org"
	record.Hold = true
	receipt, err := f.repo.Commit(f.ctx, record)
	if err != nil || receipt.Replayed {
		t.Fatal(receipt, err)
	}
	var credential, original, subject, state, sender, name *string
	var consoleTest bool
	var recipients int
	if err = f.repo.DB.QueryRowContext(f.ctx, "SELECT smtp_credential_id,original_from_address,subject,state,sender_address,sender_name,console_test,(SELECT count(*) FROM message_recipients WHERE message_id=m.id AND kind='bcc') FROM messages m WHERE id=$1", receipt.ID).
		Scan(&credential, &original, &subject, &state, &sender, &name, &consoleTest, &recipients); err != nil {
		t.Fatal(err)
	}
	if *credential != f.cred || *original != "someone@elsewhere.example.org" || *subject != "" || *state != "on_hold" || *sender != "sender@example.com" || name != nil || consoleTest || recipients != 2 {
		t.Fatal(*credential, *original, *subject, *state, consoleTest, recipients)
	}
	if n := f.count("SELECT count(*) FROM webhook_events WHERE outcome='held' AND message_id=$1", receipt.ID); n != 1 {
		t.Fatal("held event", n)
	}
	if n := f.count("SELECT count(*) FROM audit_events WHERE action='message.test.submitted'"); n != 0 {
		t.Fatal("an SMTP message is not an operator test send")
	}
	if n := f.count("SELECT recipient_quota_used FROM apps WHERE id=$1", f.app); n != 2 {
		t.Fatal("quota", n)
	}
	// Quota and backlog still apply.
	f.exec("UPDATE apps SET recipient_daily_limit=2")
	if _, err = f.repo.Commit(f.ctx, f.smtpRecord()); !errors.Is(err, submission.ErrQuotaExceeded) {
		t.Fatal("quota", err)
	}
	f.exec("UPDATE apps SET recipient_daily_limit=1000,pending_message_limit=1")
	if _, err = f.repo.Commit(f.ctx, f.smtpRecord()); !errors.Is(err, submission.ErrBacklogExceeded) {
		t.Fatal("backlog", err)
	}
}

// A long unfolded display name and a preserved quoted From both commit.
func TestSMTPCommitFromShapes(t *testing.T) {
	f := newSMTPCommitFixture(t)
	for name, c := range map[string]struct{ from, address, display string }{
		"long name":    {`"` + strings.Repeat("N", 900) + `" <sender@example.com>`, "sender@example.com", strings.Repeat("N", 900)},
		"quoted comma": {`"Doe, Jane" <jane.doe@example.com>`, "jane.doe@example.com", "Doe, Jane"},
		"bare":         {"plain@example.com", "plain@example.com", ""},
	} {
		record := f.smtpRecord()
		record.From, record.SenderAddress, record.SenderName = c.from, c.address, c.display
		receipt, err := f.repo.Commit(f.ctx, record)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var address string
		if err = f.repo.DB.QueryRowContext(f.ctx, "SELECT sender_address FROM messages WHERE id=$1", receipt.ID).Scan(&address); err != nil || address != c.address {
			t.Fatal(name, address, err)
		}
	}
	record := f.smtpRecord()
	record.SenderDomain = "example.net"
	if _, err := f.repo.Commit(f.ctx, record); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatal("domain mismatch", err)
	}
	record = f.smtpRecord()
	record.OriginalFromAddress = "a\r\nb@example.org"
	if _, err := f.repo.Commit(f.ctx, record); !errors.Is(err, ErrSMTPRecordInvalid) {
		t.Fatal("original from with a line break", err)
	}
}

func TestSMTPCommitRefusals(t *testing.T) {
	type refusal struct {
		prepare func(*smtpCommitFixture)
		mutate  func(*acceptance.Record)
		want    error
	}
	for name, c := range map[string]refusal{
		"paused app":         {func(f *smtpCommitFixture) { f.exec("UPDATE apps SET active=false") }, nil, submission.ErrAppPaused},
		"revoked credential": {func(f *smtpCommitFixture) { f.exec("UPDATE smtp_credentials SET revoked_at=clock_timestamp()") }, nil, submission.ErrInvalidKey},
		"deactivated domain": {func(f *smtpCommitFixture) { f.exec("UPDATE sender_domains SET active=false") }, nil, submission.ErrSenderDomainInactive},
		"no fence":           {nil, func(r *acceptance.Record) { r.Fence = submission.Fence{} }, delivery.ErrLeaseInvalid},
		"bad worker":         {nil, func(r *acceptance.Record) { r.Fence.WorkerID = "a b" }, delivery.ErrLeaseInvalid},
		"unknown worker":     {nil, func(r *acceptance.Record) { r.Fence.WorkerID = "worker-2" }, delivery.ErrLeaseInvalid},
		"other controller":   {nil, func(r *acceptance.Record) { r.Fence.ControllerID = "controller-9" }, delivery.ErrLeaseInvalid},
		"other epoch":        {nil, func(r *acceptance.Record) { r.Fence.Epoch++ }, delivery.ErrLeaseInvalid},
		"expired lease": {func(f *smtpCommitFixture) {
			f.exec("UPDATE controller_leases SET expires_at=clock_timestamp()-interval '1 second'")
		}, nil, delivery.ErrLeaseInvalid},
		"ttl without key": {nil, func(r *acceptance.Record) { r.IdempotencyTTL = time.Minute }, ErrSMTPRecordInvalid},
		"key without ttl": {nil, func(r *acceptance.Record) { r.IdempotencyKey = "smtp1:x" }, ErrSMTPRecordInvalid},
		"ttl over a day":  {nil, func(r *acceptance.Record) { r.IdempotencyKey, r.IdempotencyTTL = "smtp1:x", 25*time.Hour }, ErrSMTPRecordInvalid},
	} {
		f := newSMTPCommitFixture(t)
		if c.prepare != nil {
			c.prepare(f)
		}
		record := f.smtpRecord()
		if c.mutate != nil {
			c.mutate(&record)
		}
		if _, err := f.repo.Commit(f.ctx, record); !errors.Is(err, c.want) {
			t.Fatalf("%s: %v", name, err)
		}
		if f.messageCount() != 0 || f.count("SELECT count(*) FROM idempotency_keys") != 0 {
			t.Fatalf("%s: left rows", name)
		}
	}
}

func TestSMTPIdempotencyKindsAndWindows(t *testing.T) {
	f := newSMTPCommitFixture(t)
	idem := func(key string, hash string, ttl time.Duration) acceptance.Record {
		r := f.smtpRecord()
		r.IdempotencyKey, r.RequestHash, r.IdempotencyTTL = key, sha256.Sum256([]byte(hash)), ttl
		return r
	}
	// Kind C: a retry replays, a resend after the window is a new message that
	// reuses the expired row.
	first, err := f.repo.Commit(f.ctx, idem("smtp-c1:aa", "a", 10*time.Minute))
	if err != nil || first.Replayed {
		t.Fatal(first, err)
	}
	retry, err := f.repo.Commit(f.ctx, idem("smtp-c1:aa", "a", 10*time.Minute))
	if err != nil || !retry.Replayed || retry.ID != first.ID || f.messageCount() != 1 {
		t.Fatal("retry", retry, err)
	}
	if _, err = f.repo.Commit(f.ctx, idem("smtp-c1:aa", "different", 10*time.Minute)); !errors.Is(err, submission.ErrIdempotencyConflict) {
		t.Fatal("a different request hash under one key", err)
	}
	if f.messageCount() != 1 {
		t.Fatal("a conflicting commit left a message")
	}
	var seconds float64
	if err = f.repo.DB.QueryRowContext(f.ctx, "SELECT extract(epoch FROM expires_at-clock_timestamp()) FROM idempotency_keys WHERE key='smtp-c1:aa'").Scan(&seconds); err != nil || seconds < 590 || seconds > 600 {
		t.Fatal("10 minute window", seconds, err)
	}
	f.exec("UPDATE idempotency_keys SET expires_at=clock_timestamp()-interval '1 second' WHERE key='smtp-c1:aa'")
	again, err := f.repo.Commit(f.ctx, idem("smtp-c1:aa", "a", 10*time.Minute))
	if err != nil || again.Replayed || again.ID == first.ID || f.messageCount() != 2 || f.count("SELECT count(*) FROM idempotency_keys") != 1 {
		t.Fatal("resend after the window", again, err)
	}
	if n := f.count("SELECT count(*) FROM idempotency_keys WHERE message_id=$1", again.ID); n != 1 {
		t.Fatal("the expired row was not reused")
	}
	// Kind M keeps a 24 hour window, and the key is not shared with another adapter.
	if _, err = f.repo.Commit(f.ctx, idem("smtp1:aa", "a", 24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.DB.QueryRowContext(f.ctx, "SELECT extract(epoch FROM expires_at-clock_timestamp())/3600 FROM idempotency_keys WHERE key='smtp1:aa'").Scan(&seconds); err != nil || seconds < 23.9 || seconds > 24 {
		t.Fatal("24 hour window", seconds, err)
	}
	http := f.httpRecord()
	http.IdempotencyKey, http.RequestHash = "smtp1:aa", sha256.Sum256([]byte("a"))
	if r, err := f.repo.Commit(f.ctx, http); err != nil || r.Replayed {
		t.Fatal("the same key under another adapter must not replay", r, err)
	}
	// Neither header: no row, two messages.
	before := f.count("SELECT count(*) FROM idempotency_keys")
	one, err1 := f.repo.Commit(f.ctx, f.smtpRecord())
	two, err2 := f.repo.Commit(f.ctx, f.smtpRecord())
	if err1 != nil || err2 != nil || one.ID == two.ID || f.count("SELECT count(*) FROM idempotency_keys") != before {
		t.Fatal("no-key submissions", err1, err2)
	}
}

// The idempotency window starts when the message is accepted, not when its
// transaction began: a stalled object check must not shorten it.
func TestSMTPIdempotencyWindowStartsAtAcceptance(t *testing.T) {
	f := newSMTPCommitFixture(t)
	gate := newGatedVerifier()
	f.repo.Objects = gate
	record := f.smtpRecord()
	record.IdempotencyKey, record.RequestHash, record.IdempotencyTTL = "smtp-c1:win", sha256.Sum256([]byte("w")), 10*time.Minute
	done := make(chan error, 1)
	go func() { _, err := f.repo.Commit(f.ctx, record); done <- err }()
	<-gate.arrived
	time.Sleep(3 * time.Second)
	var release, after time.Time
	if err := f.repo.DB.QueryRowContext(f.ctx, "SELECT clock_timestamp()").Scan(&release); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := f.repo.DB.QueryRowContext(f.ctx, "SELECT clock_timestamp()").Scan(&after); err != nil {
		t.Fatal(err)
	}
	var expires time.Time
	if err := f.repo.DB.QueryRowContext(f.ctx, "SELECT expires_at FROM idempotency_keys WHERE key='smtp-c1:win'").Scan(&expires); err != nil {
		t.Fatal(err)
	}
	start := expires.Add(-10 * time.Minute)
	if start.Before(release) || start.After(after) {
		t.Fatalf("window started at %v, outside [%v, %v]: it was measured from the transaction start", start, release, after)
	}
}

// The lease is taken last: a commit stalled in its object check holds no lock
// on the lease row, so renewal proceeds and the commit still succeeds.
func TestSMTPCommitDoesNotHoldTheLeaseDuringTheObjectCheck(t *testing.T) {
	f := newSMTPCommitFixture(t)
	gate := newGatedVerifier()
	f.repo.Objects = gate
	done := make(chan error, 1)
	go func() { _, err := f.repo.Commit(f.ctx, f.smtpRecord()); done <- err }()
	<-gate.arrived
	renewed := make(chan error, 1)
	go func() { _, err := f.repo.RenewControllerLease(f.ctx, f.lease, time.Minute); renewed <- err }()
	select {
	case err := <-renewed:
		if err != nil {
			t.Fatal("renewal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("renewal was blocked by a commit stalled in its object check")
	}
	close(gate.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A takeover while the commit is stalled in the object check: the next epoch is
// acquired without waiting, and the commit then finds its lease gone.
func TestSMTPCommitTakeoverDuringObjectCheck(t *testing.T) {
	f := newSMTPCommitFixture(t)
	gate := newGatedVerifier()
	f.repo.Objects = gate
	done := make(chan error, 1)
	go func() { _, err := f.repo.Commit(f.ctx, f.smtpRecord()); done <- err }()
	<-gate.arrived
	if _, err := f.repo.AcquireControllerLease(f.ctx, f.lease.WorkerID, "controller-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	if err := <-done; !errors.Is(err, delivery.ErrLeaseInvalid) {
		t.Fatal("a commit under a superseded epoch", err)
	}
	if f.messageCount() != 0 || f.count("SELECT count(*) FROM message_recipients") != 0 {
		t.Fatal("a fenced commit left rows")
	}
}

// Once the commit holds the lease, a takeover waits for it, so the committed
// message is ordered before the new epoch.
func TestSMTPCommitOrdersBeforeALaterEpoch(t *testing.T) {
	f := newSMTPCommitFixture(t)
	f.sleepOnRecipientInsert(2)
	var committed, acquired time.Time
	var wg sync.WaitGroup
	var commitErr, acquireErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, commitErr = f.repo.Commit(f.ctx, f.smtpRecord())
		committed = time.Now()
	}()
	f.waitFor("the commit to be inside the recipient insert, lease held", "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='PgSleep'")
	go func() {
		defer wg.Done()
		_, acquireErr = f.repo.AcquireControllerLease(f.ctx, f.lease.WorkerID, "controller-1", time.Minute)
		acquired = time.Now()
	}()
	f.waitFor("the takeover to wait on the commit's lease lock", lockWaiter)
	wg.Wait()
	if commitErr != nil || acquireErr != nil {
		t.Fatal(commitErr, acquireErr)
	}
	if acquired.Before(committed) {
		t.Fatalf("the later epoch was acquired %v before the commit finished", committed.Sub(acquired))
	}
}

// The lease expiring after it was locked rolls the commit back.
func TestSMTPCommitRechecksTheLeaseBeforeCommit(t *testing.T) {
	f := newSMTPCommitFixture(t)
	f.sleepOnRecipientInsert(2)
	f.exec("UPDATE controller_leases SET expires_at=clock_timestamp()+interval '1 second'")
	record := f.smtpRecord()
	record.IdempotencyKey, record.RequestHash, record.IdempotencyTTL = "smtp1:exp", sha256.Sum256([]byte("e")), time.Hour
	if _, err := f.repo.Commit(f.ctx, record); !errors.Is(err, delivery.ErrLeaseInvalid) {
		t.Fatal("expiry after the lock", err)
	}
	if f.messageCount() != 0 || f.count("SELECT count(*) FROM message_recipients") != 0 || f.count("SELECT count(*) FROM idempotency_keys") != 0 || f.count("SELECT recipient_quota_used FROM apps") != 0 {
		t.Fatal("a rolled-back commit left rows or consumed quota")
	}
}

// Revoking a credential waits for a commit that has rechecked it.
func TestSMTPRevocationWaitsForAnInFlightCommit(t *testing.T) {
	f := newSMTPCommitFixture(t)
	f.sleepOnRecipientInsert(2)
	var committed, revoked time.Time
	var wg sync.WaitGroup
	var commitErr, revokeErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, commitErr = f.repo.Commit(f.ctx, f.smtpRecord())
		committed = time.Now()
	}()
	f.waitFor("the commit to be inside the recipient insert, credential locked", "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='PgSleep'")
	go func() {
		defer wg.Done()
		revokeErr = f.repo.RevokeSMTPCredential(f.ctx, "operator", f.app, f.cred)
		revoked = time.Now()
	}()
	f.waitFor("the revocation to wait on the commit's lock", lockWaiter)
	wg.Wait()
	if commitErr != nil || revokeErr != nil {
		t.Fatal(commitErr, revokeErr)
	}
	if revoked.Before(committed) {
		t.Fatal("the revocation did not wait for the commit that had verified the credential")
	}
}

func TestLookupSMTPReplay(t *testing.T) {
	f := newSMTPCommitFixture(t)
	if _, found, err := f.repo.LookupSMTPReplay(f.ctx, f.app, "smtp1:x"); err != nil || found {
		t.Fatal("empty", found, err)
	}
	record := f.smtpRecord()
	record.IdempotencyKey, record.RequestHash = "smtp1:abc", sha256.Sum256([]byte("a"))
	record.IdempotencyTTL = time.Hour
	if _, err := f.repo.Commit(f.ctx, record); err != nil {
		t.Fatal(err)
	}
	receipt, found, err := f.repo.LookupSMTPReplay(f.ctx, f.app, "smtp1:abc")
	if err != nil || !found || !receipt.Replayed || receipt.ID == "" || receipt.AcceptedAt.IsZero() {
		t.Fatal(receipt, found, err)
	}
	// Another app, another adapter and an expired row never match; a pause does not hide it.
	if _, found, _ = f.repo.LookupSMTPReplay(f.ctx, strings.Repeat("c", 32), "smtp1:abc"); found {
		t.Fatal("cross-app replay")
	}
	f.exec("UPDATE apps SET active=false WHERE id=$1", f.app)
	if _, found, err = f.repo.LookupSMTPReplay(f.ctx, f.app, "smtp1:abc"); err != nil || !found {
		t.Fatal("a paused app hides a replay", found, err)
	}
	f.exec("UPDATE idempotency_keys SET adapter='thinpost'")
	if _, found, _ = f.repo.LookupSMTPReplay(f.ctx, f.app, "smtp1:abc"); found {
		t.Fatal("another adapter's row served as an SMTP replay")
	}
	f.exec("UPDATE idempotency_keys SET adapter='smtp',expires_at=clock_timestamp()-interval '1 second'")
	if _, found, _ = f.repo.LookupSMTPReplay(f.ctx, f.app, "smtp1:abc"); found {
		t.Fatal("an expired row served as a replay")
	}
}
