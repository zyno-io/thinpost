package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/acceptance"
	"github.com/zyno-io/thinpost/internal/delivery"
	"github.com/zyno-io/thinpost/internal/submission"
)

// limitsState is what a limits-bypassed send must leave exactly as it was.
type limitsState struct {
	tokens       int64
	refilled     time.Time
	quotaUsed    int
	quotaDay     time.Time
	messageCount int
}

func (f *operatorFixture) limitsState(app string) limitsState {
	f.t.Helper()
	var s limitsState
	if err := f.owner.QueryRowContext(f.ctx, `SELECT submission_tokens,submission_refilled_at,recipient_quota_used,recipient_quota_day,(SELECT count(*) FROM messages WHERE app_id=$1) FROM apps WHERE id=$1`, app).Scan(&s.tokens, &s.refilled, &s.quotaUsed, &s.quotaDay, &s.messageCount); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// overrideRecord is an operator record with the given overrides.
func (f *operatorFixture) overrideRecord(app string, limits, recipients bool) acceptance.Record {
	record := f.record(app, "sender@example.com", 2)
	record.LimitsOverride, record.RecipientPolicyOverride = limits, recipients
	return record
}

func TestLimitsOverrideSkipsAndDoesNotCount(t *testing.T) {
	f := newOperatorFixture(t)
	// Every limit is closed: no submission rate, no daily quota, no pending room.
	if _, err := f.repo.SetSubmissionRate(f.ctx, "fixture", f.app, 0, 0); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE apps SET recipient_daily_limit=0,pending_message_limit=0 WHERE id=$1`, f.app)
	if err := f.repo.ReserveOperatorSubmission(f.ctx, f.app, 1, true); !errors.Is(err, submission.ErrRateLimited) {
		t.Fatalf("a charged send passed a disabled rate: %v", err)
	}
	if _, err := f.repo.Commit(f.ctx, f.overrideRecord(f.app, false, false)); !errors.Is(err, submission.ErrQuotaExceeded) {
		t.Fatalf("a charged send passed a zero quota: %v", err)
	}
	before := f.limitsState(f.app)
	var receipts []string
	for i := 0; i < 3; i++ {
		if err := f.repo.ReserveOperatorSubmission(f.ctx, f.app, 1, false); err != nil {
			t.Fatalf("bypassed reservation %d: %v", i, err)
		}
		receipt, err := f.repo.Commit(f.ctx, f.overrideRecord(f.app, true, false))
		if err != nil || receipt.ID == "" {
			t.Fatalf("bypassed commit %d: %v", i, err)
		}
		receipts = append(receipts, receipt.ID)
	}
	after := f.limitsState(f.app)
	if after.tokens != before.tokens || !after.refilled.Equal(before.refilled) || after.quotaUsed != before.quotaUsed || !after.quotaDay.Equal(before.quotaDay) || after.messageCount != before.messageCount+3 {
		t.Fatalf("bypassed sends touched the limits: before %+v after %+v", before, after)
	}
	for _, id := range receipts {
		var consoleTest, limits, recipients, pending bool
		if err := f.owner.QueryRowContext(f.ctx, `SELECT console_test,limits_override,recipient_policy_override,pending FROM messages WHERE id=$1`, id).Scan(&consoleTest, &limits, &recipients, &pending); err != nil || !consoleTest || !limits || recipients || !pending {
			t.Fatalf("stored flags %v %v %v %v: %v", consoleTest, limits, recipients, pending, err)
		}
	}
	// The messages are pending, so the operations backlog sees them, but the
	// app's pending count for its limit does not.
	quota, err := f.repo.GetPendingQuota(f.ctx, f.app)
	if err != nil || quota.Pending != 0 {
		t.Fatalf("bypassed messages counted against the app pending limit: %+v %v", quota, err)
	}
	if f.count(`SELECT count(*) FROM messages WHERE app_id=$1 AND pending`, f.app) != 3 {
		t.Fatal("bypassed messages are not pending in the queue")
	}
}

func TestLimitsOverrideDoesNotConsumeThePendingLimitForKeys(t *testing.T) {
	f := newOperatorFixture(t)
	f.exec(`UPDATE apps SET pending_message_limit=1 WHERE id=$1`, f.app)
	for i := 0; i < 3; i++ {
		if _, err := f.repo.Commit(f.ctx, f.overrideRecord(f.app, true, false)); err != nil {
			t.Fatal(err)
		}
	}
	// A later key submission at the limit is unaffected by the three bypassed ones.
	if _, err := f.submit(f.app, f.key, false); err != nil {
		t.Fatalf("key submission refused by bypassed messages: %v", err)
	}
	if _, err := f.submit(f.app, f.key, false); !errors.Is(err, submission.ErrBacklogExceeded) {
		t.Fatalf("pending limit not enforced for keys: %v", err)
	}
	// A charged operator send is counted and refused like any other.
	if _, err := f.repo.Commit(f.ctx, f.overrideRecord(f.app, false, false)); !errors.Is(err, submission.ErrBacklogExceeded) {
		t.Fatalf("charged operator send ignored the pending limit: %v", err)
	}
	quota, err := f.repo.GetPendingQuota(f.ctx, f.app)
	if err != nil || quota.Pending != 1 {
		t.Fatalf("pending count %+v %v", quota, err)
	}
}

func TestLimitsOverrideStillRequiresEverythingElse(t *testing.T) {
	f := newOperatorFixture(t)
	f.exec(`UPDATE apps SET recipient_daily_limit=0,pending_message_limit=0 WHERE id=$1`, f.app)
	both := f.overrideRecord(f.app, true, true)
	// An unverified sender domain.
	unverified := f.overrideRecord(f.app, true, true)
	unverified.From = "sender@unverified.example"
	unverified = withMetadata(unverified)
	if _, err := f.repo.Commit(f.ctx, unverified); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatalf("unverified domain: %v", err)
	}
	// A deactivated sender domain.
	f.exec(`UPDATE sender_domains SET active=false WHERE app_id=$1`, f.app)
	both.ObjectKey = fixtureObjectKey("submissions", f.id())
	if _, err := f.repo.Commit(f.ctx, both); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatalf("inactive domain: %v", err)
	}
	f.exec(`UPDATE sender_domains SET active=true WHERE app_id=$1`, f.app)
	// A paused app, at reservation and at commit.
	f.exec(`UPDATE apps SET active=false WHERE id=$1`, f.app)
	if err := f.repo.ReserveOperatorSubmission(f.ctx, f.app, 1, false); !errors.Is(err, submission.ErrAppPaused) {
		t.Fatalf("paused reserve: %v", err)
	}
	both.ObjectKey = fixtureObjectKey("submissions", f.id())
	if _, err := f.repo.Commit(f.ctx, both); !errors.Is(err, submission.ErrAppPaused) {
		t.Fatalf("paused commit: %v", err)
	}
	f.exec(`UPDATE apps SET active=true WHERE id=$1`, f.app)
	// A closed admission gate.
	f.exec(`UPDATE runtime_gate SET mode='closed' WHERE singleton`)
	if err := f.repo.ReserveOperatorSubmission(f.ctx, f.app, 1, false); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("closed gate reserve: %v", err)
	}
	both.ObjectKey = fixtureObjectKey("submissions", f.id())
	if _, err := f.repo.Commit(f.ctx, both); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("closed gate commit: %v", err)
	}
	f.exec(`UPDATE runtime_gate SET mode='open' WHERE singleton`)
	// Recipient count bounds are not an override matter.
	tooMany := f.overrideRecord(f.app, true, true)
	tooMany.RecipientCount = 101
	if _, err := f.repo.Commit(f.ctx, tooMany); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Fatalf("recipient cap: %v", err)
	}
	if f.messages(f.app) != 0 || f.audits(f.app) != 0 {
		t.Fatal("a refused send left rows")
	}
}

func TestCommitRefusesOverridesForAnyOtherPrincipal(t *testing.T) {
	f := newOperatorFixture(t)
	for _, overrides := range [][2]bool{{true, false}, {false, true}, {true, true}} {
		keyed := withMetadata(acceptance.Record{AppID: f.app, KeyID: f.key, ObjectKey: fixtureObjectKey("submissions", f.id()), Adapter: "resend", From: "sender@example.com", RecipientCount: 1, LimitsOverride: overrides[0], RecipientPolicyOverride: overrides[1]})
		if _, err := f.repo.Commit(f.ctx, keyed); !errors.Is(err, submission.ErrInvalidKey) {
			t.Fatalf("key principal with overrides %v: %v", overrides, err)
		}
		native := keyed
		native.Adapter, native.ObjectKey = submission.AdapterNative, fixtureObjectKey("submissions", f.id())
		if _, err := f.repo.Commit(f.ctx, native); !errors.Is(err, submission.ErrInvalidKey) {
			t.Fatalf("native adapter with overrides %v: %v", overrides, err)
		}
		console := f.overrideRecord(f.app, overrides[0], overrides[1])
		for name, mutate := range map[string]func(*acceptance.Record){
			"provider adapter":   func(r *acceptance.Record) { r.Adapter = "resend" },
			"native adapter":     func(r *acceptance.Record) { r.Adapter = submission.AdapterNative },
			"key with console":   func(r *acceptance.Record) { r.Operator, r.KeyID = "", f.key },
			"idempotency key":    func(r *acceptance.Record) { r.IdempotencyKey = "key-1" },
			"operator plus key":  func(r *acceptance.Record) { r.KeyID = f.key },
			"no principal":       func(r *acceptance.Record) { r.Operator = "" },
			"no principal (key)": func(r *acceptance.Record) { r.Operator, r.Adapter = "", "resend" },
		} {
			record := console
			record.ObjectKey = fixtureObjectKey("submissions", f.id())
			mutate(&record)
			if _, err := f.repo.Commit(f.ctx, record); !errors.Is(err, submission.ErrInvalidKey) {
				t.Fatalf("%s with overrides %v: %v", name, overrides, err)
			}
		}
	}
	if f.messages(f.app) != 0 || f.audits(f.app) != 0 {
		t.Fatal("a refused override left rows")
	}
}

func TestOverridesCheckConstraintBindsNewRows(t *testing.T) {
	f := newOperatorFixture(t)
	insert := `INSERT INTO messages(id,app_id,object_key,enhanced_protection,console_test,limits_override,recipient_policy_override) VALUES ($1,$2,$3,false,$4,$5,$6)`
	for name, flags := range map[string][3]bool{
		"limits override without console":    {false, true, false},
		"recipient override without console": {false, false, true},
		"both without console":               {false, true, true},
	} {
		if _, err := f.owner.ExecContext(f.ctx, insert, f.id(), f.app, "k-"+f.id(), flags[0], flags[1], flags[2]); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	for name, flags := range map[string][3]bool{
		"plain":                {false, false, false},
		"console":              {true, false, false},
		"console and limits":   {true, true, false},
		"console and policy":   {true, false, true},
		"console and both":     {true, true, true},
		"console, none set up": {true, false, false},
	} {
		if _, err := f.owner.ExecContext(f.ctx, insert, f.id(), f.app, "k-"+f.id(), flags[0], flags[1], flags[2]); err != nil {
			t.Fatalf("%s was refused: %v", name, err)
		}
	}
	// Updating an existing plain row to carry an override is refused too.
	plain := f.mustSubmit(false)
	if _, err := f.owner.ExecContext(f.ctx, `UPDATE messages SET limits_override=true WHERE id=$1`, plain); err == nil {
		t.Fatal("a non-console message was given an override")
	}
}

func TestOperatorAuditCarriesTheBypassFlags(t *testing.T) {
	f := newOperatorFixture(t)
	for _, flags := range [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
		receipt, err := f.repo.Commit(f.ctx, f.overrideRecord(f.app, flags[0], flags[1]))
		if err != nil {
			t.Fatal(err)
		}
		var keys int
		var limits, recipients bool
		var count int
		if err = f.owner.QueryRowContext(f.ctx, `SELECT (SELECT count(*) FROM jsonb_object_keys(details)),(details->>'bypass_limits')::boolean,(details->>'bypass_recipient_policy')::boolean,(details->>'recipient_count')::integer
			FROM audit_events WHERE resource_id=$1 AND action='message.test.submitted'`, receipt.ID).Scan(&keys, &limits, &recipients, &count); err != nil {
			t.Fatal(err)
		}
		if keys != 3 || limits != flags[0] || recipients != flags[1] || count != 2 {
			t.Fatalf("audit for %v: keys=%d limits=%v recipients=%v count=%d", flags, keys, limits, recipients, count)
		}
	}
}

func TestOperatorMessagesCarryTheTestTagInHistory(t *testing.T) {
	f := newOperatorFixture(t)
	keyed := f.mustSubmit(false)
	plain, err := f.repo.Commit(f.ctx, f.overrideRecord(f.app, false, false))
	if err != nil {
		t.Fatal(err)
	}
	bypassed, err := f.repo.Commit(f.ctx, f.overrideRecord(f.app, true, true))
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.repo.ListMessages(f.ctx, "actor", f.app, "", 10)
	if err != nil || len(page.Messages) != 3 {
		t.Fatal(err, len(page.Messages))
	}
	for _, message := range page.Messages {
		switch message.ID {
		case keyed:
			if message.OperatorTest != nil {
				t.Fatalf("a key submission is tagged: %+v", message.OperatorTest)
			}
		case plain.ID:
			if message.OperatorTest == nil || message.OperatorTest.BypassLimits || message.OperatorTest.BypassRecipientPolicy {
				t.Fatalf("plain console test: %+v", message.OperatorTest)
			}
		case bypassed.ID:
			if message.OperatorTest == nil || !message.OperatorTest.BypassLimits || !message.OperatorTest.BypassRecipientPolicy {
				t.Fatalf("bypassed console test: %+v", message.OperatorTest)
			}
		default:
			t.Fatal("unexpected message", message.ID)
		}
	}
	detail, err := f.repo.GetMessage(f.ctx, "actor", f.app, bypassed.ID)
	if err != nil || detail.Message.OperatorTest == nil || !detail.Message.OperatorTest.BypassLimits || !detail.Message.OperatorTest.BypassRecipientPolicy {
		t.Fatalf("detail: %+v %v", detail.Message.OperatorTest, err)
	}
}

func TestClaimReturnsTheRecipientPolicyOverride(t *testing.T) {
	f := newOperatorFixture(t)
	override, err := f.repo.Commit(f.ctx, f.overrideRecord(f.app, false, true))
	if err != nil {
		t.Fatal(err)
	}
	limitsOnly, err := f.repo.Commit(f.ctx, f.overrideRecord(f.app, true, false))
	if err != nil {
		t.Fatal(err)
	}
	keyed := f.mustSubmit(false)
	want := map[string]bool{override.ID: true, limitsOnly.ID: false, keyed: false}
	for id := range want {
		f.exec(`UPDATE messages SET state='prepared' WHERE id=$1`, id)
		f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key) VALUES ($1,$2,0,'signed')`, f.id(), id)
	}
	lease, err := f.repo.AcquireControllerLease(f.ctx, "worker", "controller", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for remaining := len(want); remaining > 0; remaining-- {
		claim, claimErr := f.repo.ClaimDelivery(f.ctx, lease, f.id(), delivery.EdgeCapabilities{})
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		expected, ok := want[claim.MessageID]
		if !ok || claim.RecipientPolicyOverride != expected {
			t.Fatalf("message %s: claim override %v, want %v", claim.MessageID, claim.RecipientPolicyOverride, expected)
		}
		delete(want, claim.MessageID)
	}
}
