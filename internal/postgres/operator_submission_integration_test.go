package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/submission"
)

const operatorSubject = "operator-subject-1"

// operatorFixture is an app with a verified sender domain, an enabled
// submission rate and room under every limit.
type operatorFixture struct{ *holdFixture }

func newOperatorFixture(t *testing.T) *operatorFixture {
	f := &operatorFixture{newHoldFixture(t)}
	f.enableRate(f.app)
	return f
}

func (f *operatorFixture) enableRate(app string) {
	f.t.Helper()
	if _, err := f.repo.SetSubmissionRate(f.ctx, "fixture", app, 600, 100); err != nil {
		f.t.Fatal(err)
	}
}

// record is an operator record for from, with count recipients.
func (f *operatorFixture) record(app, from string, count int) acceptance.Record {
	return withMetadata(acceptance.Record{AppID: app, Operator: operatorSubject, ObjectKey: fixtureObjectKey("submissions", f.id()), Adapter: submission.AdapterConsole, From: from, RecipientCount: count})
}

func (f *operatorFixture) messages(app string) int {
	return f.count(`SELECT count(*) FROM messages WHERE app_id=$1`, app)
}

func (f *operatorFixture) audits(app string) int {
	return f.count(`SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='message.test.submitted'`, app)
}

func TestOperatorCommitRecordsAuditWithoutKey(t *testing.T) {
	f := newOperatorFixture(t)
	// No API key is needed: use an app that has none.
	app := f.app_("keyless")
	f.enableRate(app)
	receipt, err := f.repo.Commit(f.ctx, f.record(app, "sender@example.com", 2))
	if err != nil || receipt.ID == "" || receipt.Replayed {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM api_keys WHERE app_id=$1`, app) != 0 {
		t.Fatal("fixture app has a key")
	}
	var actor, resource, details string
	if err := f.owner.QueryRowContext(f.ctx, `SELECT actor_id,resource_id,details::text FROM audit_events WHERE app_id=$1 AND action='message.test.submitted'`, app).Scan(&actor, &resource, &details); err != nil {
		t.Fatal(err)
	}
	if actor != operatorSubject || resource != receipt.ID {
		t.Fatalf("audit actor %q resource %q", actor, resource)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(details), &parsed); err != nil || len(parsed) != 3 || parsed["recipient_count"] != float64(2) || parsed["bypass_limits"] != false || parsed["bypass_recipient_policy"] != false {
		t.Fatalf("audit details must hold only recipient_count and the two bypass flags: %s", details)
	}
	if f.messages(app) != 1 {
		t.Fatal("message not stored")
	}
}

func TestKeyCommitWritesNoOperatorAudit(t *testing.T) {
	f := newOperatorFixture(t)
	f.mustSubmit(false)
	if f.audits(f.app) != 0 || f.messages(f.app) != 1 {
		t.Fatal("a key submission was audited as a test send")
	}
}

func TestOperatorCommitRefusesEveryOtherPrincipalCombination(t *testing.T) {
	f := newOperatorFixture(t)
	valid := f.record(f.app, "sender@example.com", 1)
	tests := map[string]func(*acceptance.Record){
		"operator with a provider adapter":  func(r *acceptance.Record) { r.Adapter = "resend" },
		"operator with the native adapter":  func(r *acceptance.Record) { r.Adapter = submission.AdapterNative },
		"operator with an idempotency key":  func(r *acceptance.Record) { r.IdempotencyKey = "key-1" },
		"operator and key together":         func(r *acceptance.Record) { r.KeyID = f.key },
		"operator with a control character": func(r *acceptance.Record) { r.Operator = "a\nb" },
		"operator with padding":             func(r *acceptance.Record) { r.Operator = " " + operatorSubject },
		"operator over 256 bytes":           func(r *acceptance.Record) { r.Operator = string(make([]byte, 257)) },
		"console adapter with a key":        func(r *acceptance.Record) { r.Operator, r.KeyID = "", f.key },
		"console adapter without principal": func(r *acceptance.Record) { r.Operator = "" },
		"provider adapter without key":      func(r *acceptance.Record) { r.Operator, r.Adapter = "", "resend" },
		"provider adapter with operator":    func(r *acceptance.Record) { r.Adapter = "resend"; r.KeyID = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			record := valid
			record.ObjectKey = fixtureObjectKey("submissions", f.id())
			mutate(&record)
			if _, err := f.repo.Commit(f.ctx, record); !errors.Is(err, submission.ErrInvalidKey) {
				t.Fatalf("got %v", err)
			}
		})
	}
	if f.messages(f.app) != 0 || f.audits(f.app) != 0 {
		t.Fatal("a refused combination left a message or audit row")
	}
}

func TestOperatorCommitEnforcesTheSameAcceptancePolicy(t *testing.T) {
	f := newOperatorFixture(t)
	if _, err := f.repo.Commit(f.ctx, f.record(f.app, "sender@unverified.example", 1)); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatalf("unverified sender domain: %v", err)
	}
	f.exec(`UPDATE sender_domains SET active=false WHERE app_id=$1`, f.app)
	if _, err := f.repo.Commit(f.ctx, f.record(f.app, "sender@example.com", 1)); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatalf("inactive sender domain: %v", err)
	}
	f.exec(`UPDATE sender_domains SET active=true WHERE app_id=$1`, f.app)
	f.exec(`UPDATE apps SET recipient_daily_limit=1 WHERE id=$1`, f.app)
	if _, err := f.repo.Commit(f.ctx, f.record(f.app, "sender@example.com", 2)); !errors.Is(err, submission.ErrQuotaExceeded) {
		t.Fatalf("recipient quota: %v", err)
	}
	f.exec(`UPDATE apps SET recipient_daily_limit=1000,pending_message_limit=0 WHERE id=$1`, f.app)
	if _, err := f.repo.Commit(f.ctx, f.record(f.app, "sender@example.com", 1)); !errors.Is(err, submission.ErrBacklogExceeded) {
		t.Fatalf("pending limit: %v", err)
	}
	if f.messages(f.app) != 0 || f.audits(f.app) != 0 {
		t.Fatal("a refused submission left a message or audit row")
	}
	f.exec(`UPDATE apps SET pending_message_limit=1000 WHERE id=$1`, f.app)
	if _, err := f.repo.Commit(f.ctx, f.record(f.app, "sender@example.com", 1)); err != nil {
		t.Fatal(err)
	}
	if f.messages(f.app) != 1 || f.audits(f.app) != 1 {
		t.Fatal("accepted submission not recorded exactly once")
	}
}

func TestOperatorPausedApp(t *testing.T) {
	f := newOperatorFixture(t)
	f.exec(`UPDATE apps SET active=false WHERE id=$1`, f.app)
	if _, err := f.repo.LookupOperatorApp(f.ctx, f.app); !errors.Is(err, submission.ErrAppPaused) {
		t.Fatalf("lookup: %v", err)
	}
	if err := f.repo.ReserveOperatorSubmission(f.ctx, f.app, 1, true); !errors.Is(err, submission.ErrAppPaused) {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := f.repo.Commit(f.ctx, f.record(f.app, "sender@example.com", 1)); !errors.Is(err, submission.ErrAppPaused) {
		t.Fatalf("commit for a paused app: %v", err)
	}
	// A key commit is unchanged: the app is not found for it.
	if _, err := f.submit(f.app, f.key, false); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatalf("key commit for a paused app: %v", err)
	}
	// The key path still reports a paused app as before.
	if err := f.repo.ReserveSubmission(f.ctx, f.app, f.key, 1); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatalf("key reserve: %v", err)
	}
	if f.messages(f.app) != 0 {
		t.Fatal("a paused app stored a message")
	}
}

func TestLookupOperatorApp(t *testing.T) {
	f := newOperatorFixture(t)
	f.exec(`UPDATE apps SET list_unsubscribe=true,default_track_opens=true,default_track_clicks=false WHERE id=$1`, f.app)
	app, err := f.repo.LookupOperatorApp(f.ctx, f.app)
	if err != nil {
		t.Fatal(err)
	}
	if app.ID != f.app || app.KeyID != "" || app.Operator != "" || !app.ListUnsubscribe || !app.DefaultTrackOpens || app.DefaultTrackClicks {
		t.Fatalf("unexpected app: %#v", app)
	}
	for name, id := range map[string]string{"unknown": f.id(), "malformed": "not-an-id", "empty": ""} {
		if _, err := f.repo.LookupOperatorApp(f.ctx, id); !errors.Is(err, admin.ErrNotFound) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestOperatorReservationUsesTheSubmissionRate(t *testing.T) {
	f := newHoldFixture(t)
	// The default is disabled, exactly as for keys.
	if err := f.repo.ReserveOperatorSubmission(f.ctx, f.app, 1, true); !errors.Is(err, submission.ErrRateLimited) {
		t.Fatalf("disabled rate: %v", err)
	}
	if _, err := f.repo.SetSubmissionRate(f.ctx, "fixture", f.app, 1, 2); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := f.repo.ReserveOperatorSubmission(f.ctx, f.app, 1, true); err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
	}
	if err := f.repo.ReserveOperatorSubmission(f.ctx, f.app, 1, true); !errors.Is(err, submission.ErrRateLimited) {
		t.Fatalf("burst not enforced: %v", err)
	}
	// Operator and key submissions draw on one bucket.
	if err := f.repo.ReserveSubmission(f.ctx, f.app, f.key, 1); !errors.Is(err, submission.ErrRateLimited) {
		t.Fatalf("key reservation did not see the operator charges: %v", err)
	}
	for _, messages := range []int{0, 101} {
		if err := f.repo.ReserveOperatorSubmission(f.ctx, f.app, messages, true); !errors.Is(err, submission.ErrInvalidMessage) {
			t.Fatalf("%d messages: %v", messages, err)
		}
	}
	if err := f.repo.ReserveOperatorSubmission(f.ctx, f.id(), 1, true); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatalf("unknown app: %v", err)
	}
}

func TestKeyReservationStillRequiresAKey(t *testing.T) {
	f := newOperatorFixture(t)
	if err := f.repo.ReserveSubmission(f.ctx, f.app, "", 1); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatalf("empty key: %v", err)
	}
	if err := f.repo.ReserveSubmission(f.ctx, f.app, f.id(), 1); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatalf("another key: %v", err)
	}
	if err := f.repo.ReserveSubmission(f.ctx, f.app, f.key, 1); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorPathsFailWhenTheGateIsClosed(t *testing.T) {
	f := newOperatorFixture(t)
	f.exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`)
	if _, err := f.repo.LookupOperatorApp(f.ctx, f.app); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("lookup: %v", err)
	}
	if err := f.repo.ReserveOperatorSubmission(f.ctx, f.app, 1, true); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := f.repo.Commit(f.ctx, f.record(f.app, "sender@example.com", 1)); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("commit: %v", err)
	}
	if f.messages(f.app) != 0 || f.audits(f.app) != 0 {
		t.Fatal("a closed gate stored a message")
	}
}

// putOnly is an object store that accepts every upload.
type putOnly struct{}

func (putOnly) Put(context.Context, string, []byte) error { return nil }

// TestOperatorSubmissionThroughAcceptance runs the whole path the console
// takes: AuthorizeOperator, then the one Accept shared with the provider APIs.
func TestOperatorSubmissionThroughAcceptance(t *testing.T) {
	f := newOperatorFixture(t)
	service, err := acceptance.New(f.repo, putOnly{}, []byte("0123456789abcdef0123456789abcdef"), allowAllRecipients{})
	if err != nil {
		t.Fatal(err)
	}
	request := submission.Request{Adapter: submission.AdapterConsole, Message: submission.Message{From: "Sender <sender@example.com>", To: []string{"first@example.net", "second@example.net"}, Subject: "Test", Text: "body"}}
	app, err := service.AuthorizeOperator(f.ctx, f.app, operatorSubject)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := service.Accept(f.ctx, app, request)
	if err != nil || receipt.ID == "" {
		t.Fatal(err)
	}
	var recipients int
	var details string
	if err := f.owner.QueryRowContext(f.ctx, `SELECT m.recipient_count,a.details::text FROM messages m JOIN audit_events a ON a.resource_id=m.id AND a.action='message.test.submitted' WHERE m.id=$1 AND a.actor_id=$2`, receipt.ID, operatorSubject).Scan(&recipients, &details); err != nil || recipients != 2 {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(details), &parsed); err != nil || len(parsed) != 3 || parsed["recipient_count"] != float64(2) || parsed["bypass_limits"] != false || parsed["bypass_recipient_policy"] != false {
		t.Fatalf("audit details: %s", details)
	}
	// An idempotency key is refused before anything is reserved or stored.
	request.IdempotencyKey = "key-1"
	if _, err := service.Accept(f.ctx, app, request); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatalf("idempotency key: %v", err)
	}
	// A provider adapter cannot reuse the operator principal.
	request.IdempotencyKey, request.Adapter = "", "resend"
	if _, err := service.Accept(f.ctx, app, request); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatalf("provider adapter: %v", err)
	}
	if f.messages(f.app) != 1 || f.audits(f.app) != 1 {
		t.Fatal("refused submissions left rows")
	}
	f.exec(`UPDATE apps SET active=false WHERE id=$1`, f.app)
	if _, err := service.AuthorizeOperator(f.ctx, f.app, operatorSubject); !errors.Is(err, submission.ErrAppPaused) {
		t.Fatalf("paused: %v", err)
	}
	if _, err := service.AuthorizeOperator(f.ctx, f.id(), operatorSubject); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown app: %v", err)
	}
}

// TestOperatorPausedBetweenReserveAndCommit pauses the app after the
// reservation succeeded, the window the 409 app_paused answer must cover.
func TestOperatorPausedBetweenReserveAndCommit(t *testing.T) {
	f := newOperatorFixture(t)
	if err := f.repo.ReserveOperatorSubmission(f.ctx, f.app, 1, true); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE apps SET active=false WHERE id=$1`, f.app)
	if _, err := f.repo.Commit(f.ctx, f.record(f.app, "sender@example.com", 1)); !errors.Is(err, submission.ErrAppPaused) {
		t.Fatalf("got %v", err)
	}
	if f.messages(f.app) != 0 || f.audits(f.app) != 0 {
		t.Fatal("rows left for a paused app")
	}
}
