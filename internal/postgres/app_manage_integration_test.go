package postgres

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/preparation"
	"github.com/zyno-io/thinpost/internal/submission"
)

func TestRenameAppPostgres(t *testing.T) {
	f := newWebhookFixture(t, "app_rename_")
	app := f.app("Old name")
	other := f.app("Other")
	repo := f.repo
	if _, err := repo.RenameApp(f.ctx, "operator", app, " bad"); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("untrimmed name accepted", err)
	}
	if _, err := repo.RenameApp(f.ctx, "operator", app, strings.Repeat("n", 129)); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("long name accepted", err)
	}
	if _, err := repo.RenameApp(f.ctx, "operator", f.id(), "New"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("unknown app", err)
	}
	renamed, err := repo.RenameApp(f.ctx, "operator", app, "New name")
	if err != nil || renamed.ID != app || renamed.Name != "New name" || !renamed.Active || renamed.CreatedAt.IsZero() {
		t.Fatal("rename", renamed, err)
	}
	var from, to string
	if err = f.owner.QueryRowContext(f.ctx, `SELECT details->>'from',details->>'to' FROM audit_events WHERE app_id=$1 AND action='app.renamed' AND actor_id='operator' AND resource_id=$1`, app).Scan(&from, &to); err != nil || from != "Old name" || to != "New name" {
		t.Fatal("audit", from, to, err)
	}
	// The same name changes and audits nothing.
	if _, err = repo.RenameApp(f.ctx, "operator", app, "New name"); err != nil || f.count(`SELECT count(*) FROM audit_events WHERE action='app.renamed'`) != 1 {
		t.Fatal("no-op rename", err)
	}
	// Names are not unique.
	if _, err = repo.RenameApp(f.ctx, "operator", other, "New name"); err != nil {
		t.Fatal("duplicate name refused", err)
	}
	apps, err := repo.ListApps(f.ctx)
	if err != nil || len(apps) != 2 || apps[0].Name != "New name" || apps[1].Name != "New name" {
		t.Fatal("list", apps, err)
	}
}

func TestDeleteAppPostgres(t *testing.T) {
	f := newHoldFixture(t)
	ctx, repo := f.ctx, f.repo
	app, key := f.app, f.key
	other := f.app_("other")
	otherKey := f.keyFor(other)
	otherCred, err := repo.IssueSMTPCredential(ctx, "operator", other, "other printer")
	if err != nil {
		t.Fatal(err)
	}

	cred, err := repo.IssueSMTPCredential(ctx, "operator", app, "printer")
	if err != nil {
		t.Fatal(err)
	}
	credDigest := sha256.Sum256([]byte(cred.Password))
	keyDigest := sha256.Sum256([]byte(key))
	if _, _, err = repo.VerifySMTPCredential(ctx, cred.Username, credDigest); err != nil {
		t.Fatal("credential before delete", err)
	}
	if _, err = repo.LookupKey(ctx, keyDigest); err != nil {
		t.Fatal("key before delete", err)
	}
	held := f.mustSubmit(true)
	queued := f.mustSubmit(false)
	f.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'second.example',true,now())`, app)

	// The app owns an active receiving domain and a pending claim; another app
	// cannot verify the domain while it is owned.
	claim, err := repo.RequestInboundDomain(ctx, "operator", app, "mail.example.net")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.VerifyInboundDomain(ctx, "operator", app, "mail.example.net", inboundDigest(claim.DNSValue)); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.RequestInboundDomain(ctx, "operator", app, "pending.example.net"); err != nil {
		t.Fatal(err)
	}
	otherClaim, err := repo.RequestInboundDomain(ctx, "operator", other, "mail.example.net")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.VerifyInboundDomain(ctx, "operator", other, "mail.example.net", inboundDigest(otherClaim.DNSValue)); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("owned domain verified by another app", err)
	}

	if err = repo.DeleteApp(ctx, "operator", "bad"); !errors.Is(err, admin.ErrInvalidInput) {
		t.Fatal("bad id", err)
	}
	if err = repo.DeleteApp(ctx, "operator", f.id()); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("unknown app", err)
	}
	if err = repo.DeleteApp(ctx, "operator", app); err != nil {
		t.Fatal("delete", err)
	}
	if err = repo.DeleteApp(ctx, "operator", app); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("second delete", err)
	}

	// Gone from the console.
	if exists, err := repo.AppExists(ctx, app); err != nil || exists {
		t.Fatal("deleted app exists", exists, err)
	}
	if exists, err := repo.AppExists(ctx, other); err != nil || !exists {
		t.Fatal("other app hidden", exists, err)
	}
	apps, err := repo.ListApps(ctx)
	if err != nil || len(apps) != 1 || apps[0].ID != other {
		t.Fatal("deleted app listed", apps, err)
	}
	if _, err = repo.RenameApp(ctx, "operator", app, "Revived"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("renamed a deleted app", err)
	}
	if err = repo.SetAppActive(ctx, "operator", app, true); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("resumed a deleted app", err)
	}
	if _, err = repo.IssueKeyForOperator(ctx, "operator", app, "late"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("issued a key for a deleted app", err)
	}
	if _, err = repo.IssueSMTPCredential(ctx, "operator", app, "late"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("issued a credential for a deleted app", err)
	}
	if _, err = repo.LookupOperatorApp(ctx, app); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("operator submission for a deleted app", err)
	}

	// Authentication fails the way an unknown or revoked key does.
	if _, err = repo.LookupKey(ctx, keyDigest); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("key still works", err)
	}
	if _, _, err = repo.VerifySMTPCredential(ctx, cred.Username, credDigest); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("credential still works", err)
	}
	if f.count(`SELECT count(*) FROM api_keys WHERE app_id=$1 AND active`, app) != 0 || f.count(`SELECT count(*) FROM smtp_credentials WHERE app_id=$1 AND revoked_at IS NULL`, app) != 0 {
		t.Fatal("keys or credentials not revoked")
	}
	// A bare deleted check also refuses an unrevoked key and credential.
	f.exec(`UPDATE api_keys SET active=true WHERE id=$1`, key)
	f.exec(`UPDATE smtp_credentials SET revoked_at=NULL WHERE id=$1`, cred.Username)
	if _, err = repo.LookupKey(ctx, keyDigest); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("deleted check missing for keys", err)
	}
	if _, _, err = repo.VerifySMTPCredential(ctx, cred.Username, credDigest); !errors.Is(err, submission.ErrInvalidKey) {
		t.Fatal("deleted check missing for credentials", err)
	}
	f.exec(`UPDATE api_keys SET active=false WHERE id=$1`, key)
	// Other apps are untouched.
	if _, err = repo.LookupKey(ctx, sha256.Sum256([]byte(otherKey))); err != nil {
		t.Fatal("other key broken", err)
	}
	if _, _, err = repo.VerifySMTPCredential(ctx, otherCred.Username, sha256.Sum256([]byte(otherCred.Password))); err != nil {
		t.Fatal("other credential broken", err)
	}

	// Mail not yet sent is not sent, and a hold cannot be released.
	if _, err = repo.ClaimPreparation(ctx, f.id(), 5*time.Second); !errors.Is(err, preparation.ErrNoWork) {
		t.Fatal("deleted app's message claimed", err)
	}
	f.exec(`UPDATE api_keys SET active=true WHERE id=$1`, key)
	if err = repo.ReleaseHeldMessage(ctx, app, key, held); err == nil {
		t.Fatal("released a hold of a deleted app")
	}
	if err = repo.CancelHeldMessage(ctx, app, key, held); err == nil {
		t.Fatal("cancelled a hold of a deleted app")
	}
	f.exec(`UPDATE api_keys SET active=false WHERE id=$1`, key)
	if state, pending := f.state(held); state != "cancelled" || pending {
		t.Fatal("held message not cancelled", state, pending)
	}
	if state, pending := f.state(queued); state != "preparation_failed" || pending {
		t.Fatal("queued message not ended", state, pending)
	}

	// Receiving domains are released; the other app can claim and own one.
	if f.count(`SELECT count(*) FROM inbound_domains WHERE app_id=$1`, app) != 0 {
		t.Fatal("receiving domains remain")
	}
	otherClaim, err = repo.RequestInboundDomain(ctx, "operator", other, "mail.example.net")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.VerifyInboundDomain(ctx, "operator", other, "mail.example.net", inboundDigest(otherClaim.DNSValue)); err != nil {
		t.Fatal("freed domain not claimable", err)
	}

	// Data and domain rows are kept; the name is reusable.
	if f.count(`SELECT count(*) FROM messages WHERE app_id=$1`, app) != 2 || f.count(`SELECT count(*) FROM sender_domains WHERE app_id=$1`, app) != 2 ||
		f.count(`SELECT count(*) FROM apps WHERE id=$1 AND deleted_at IS NOT NULL AND NOT active`, app) != 1 {
		t.Fatal("data not kept")
	}
	if _, err = repo.CreateApp(ctx, "operator", "hold"); err != nil {
		t.Fatal("name not reusable", err)
	}

	var keys, creds, domains int
	if err = f.owner.QueryRowContext(ctx, `SELECT (details->>'api_keys_revoked')::int,(details->>'smtp_credentials_revoked')::int,(details->>'inbound_domains_removed')::int
		FROM audit_events WHERE app_id=$1 AND action='app.deleted' AND actor_id='operator' AND resource_id=$1`, app).Scan(&keys, &creds, &domains); err != nil || keys != 1 || creds != 1 || domains != 2 {
		t.Fatal("audit", keys, creds, domains, err)
	}
}

// Delete ends the app's unfinished work in existing terminal states, stops its
// webhooks and leaves nothing for any claim or the Operations counts.
func TestDeleteAppEndsUnfinishedWork(t *testing.T) {
	f := newHoldFixture(t)
	ctx, repo := f.ctx, f.repo
	app := f.app
	other := f.app_("other")
	otherKey := f.keyFor(other)
	f.webhookFixture.create(other, "https://hooks.example.com/other", "preparation_failed")
	_, err := f.submit(other, otherKey, false)
	if err != nil {
		t.Fatal(err)
	}
	held, queued := f.mustSubmit(true), f.mustSubmit(false)
	f.exec(`UPDATE messages SET state='preparing',prepare_token=$2,prepare_until=now()+interval '1 minute',prepare_attempts=1 WHERE id=$1`, queued, f.id())
	failedPrep := f.mustSubmit(false)
	f.exec(`UPDATE messages SET state='preparation_failed',prepare_failure='internal' WHERE id=$1`, failedPrep)

	// A prepared message with one delivery in each unfinished state, one
	// accepted, and a second prepared message whose only delivery is accepted.
	prepared := f.id()
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'k'||$1,false,'prepared')`, prepared, app)
	states := []string{"queued", "uncertain", "recipient_policy_held", "tracking_held", "accepted"}
	deliveries := map[string]string{}
	for i, state := range states {
		id := f.id()
		deliveries[state] = id
		f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ($1,$2,$3,'f'||$1,$4)`, id, prepared, i, state)
	}
	done := f.id()
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state,pending) VALUES ($1,$2,'k'||$1,false,'prepared',true)`, done, app)
	f.exec(`INSERT INTO deliveries(id,message_id,recipient_index,final_object_key,state) VALUES ($1,$2,0,'f'||$1,'accepted')`, f.id(), done)
	recovery := f.id()
	f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,state) VALUES ($1,$2,'k'||$1,false,'recovery_hold')`, recovery, app)

	if ops, err := repo.Operations(ctx); err != nil || ops.Messages.OnHold+ops.Messages.QueuedDue+ops.Messages.Preparing+ops.Messages.PreparationFailed == 0 || ops.Deliveries.Uncertain != 1 {
		t.Fatal("operations before delete", ops.Messages, ops.Deliveries, err)
	}
	pendingWebhooks := f.count(`SELECT count(*) FROM webhook_deliveries WHERE app_id=$1 AND state IN ('queued','in_flight')`, app)
	if pendingWebhooks == 0 {
		t.Fatal("expected pending webhook deliveries from the hold events")
	}
	// One webhook delivery is mid-send: a claimed attempt that delete must mark unknown.
	attemptID := f.id()
	f.exec(`UPDATE webhook_deliveries SET state='in_flight',claim_token=$2,claim_until=now()+interval '1 minute',attempts=1 WHERE app_id=$1 AND state='queued' AND ctid=(SELECT min(ctid) FROM webhook_deliveries WHERE app_id=$1 AND state='queued')`, app, attemptID)
	f.exec(`INSERT INTO webhook_attempts(id,event_id,subscription_id) SELECT claim_token,event_id,subscription_id FROM webhook_deliveries WHERE claim_token=$1`, attemptID)

	if err = repo.DeleteApp(ctx, "operator", app); err != nil {
		t.Fatal(err)
	}
	state := func(table, id string) string {
		var s string
		if err := f.owner.QueryRowContext(ctx, `SELECT state FROM `+table+` WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if state("messages", held) != "cancelled" || state("messages", queued) != "preparation_failed" || state("messages", failedPrep) != "preparation_failed" {
		t.Fatal("unprepared messages not ended")
	}
	if f.count(`SELECT count(*) FROM messages WHERE app_id=$1 AND state='preparation_failed' AND prepare_failure='app_deleted' AND NOT pending AND prepare_token IS NULL`, app) != 2 {
		t.Fatal("reason or pending not recorded")
	}
	if state("messages", recovery) != "recovery_hold" {
		t.Fatal("recovery hold touched")
	}
	for _, name := range []string{"queued", "uncertain", "recipient_policy_held", "tracking_held"} {
		if state("deliveries", deliveries[name]) != "failed" {
			t.Fatal("delivery not failed", name)
		}
	}
	if state("deliveries", deliveries["accepted"]) != "accepted" {
		t.Fatal("accepted delivery changed")
	}
	if _, p := f.state(prepared); p {
		t.Fatal("prepared message still pending")
	}
	if _, p := f.state(done); p {
		// pending must be cleared once every delivery is terminal
		t.Fatal("finished message still pending")
	}
	if f.count(`SELECT count(*) FROM webhook_subscriptions WHERE app_id=$1 AND active`, app) != 0 ||
		f.count(`SELECT count(*) FROM webhook_deliveries WHERE app_id=$1 AND state IN ('queued','in_flight')`, app) != 0 ||
		f.count(`SELECT count(*) FROM webhook_deliveries WHERE app_id=$1 AND state='cancelled'`, app) != pendingWebhooks ||
		f.count(`SELECT count(*) FROM webhook_attempts WHERE id=$1 AND state='unknown' AND finished_at IS NOT NULL`, attemptID) != 1 {
		t.Fatal("webhooks not stopped")
	}
	if f.count(`SELECT count(*) FROM webhook_subscriptions WHERE app_id=$1`, other) == 0 {
		t.Fatal("test needs another app's endpoint")
	}

	// Nothing is claimable, and Operations show only the other app's work.
	// The only claimable message is the other app's, and then there is none.
	if claim, err := repo.ClaimPreparation(ctx, f.id(), 5*time.Second); err != nil || claim.AppID != other {
		t.Fatal("claimed the wrong work", claim.AppID, err)
	}
	if _, err = repo.ClaimPreparation(ctx, f.id(), 5*time.Second); !errors.Is(err, preparation.ErrNoWork) {
		t.Fatal("work still claimable", err)
	}
	if n := f.count(`SELECT count(*) FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE m.app_id=$1 AND d.state='queued'`, app); n != 0 {
		t.Fatal("queued deliveries remain", n)
	}
	ops, err := repo.Operations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ops.Messages.Preparing != 1 || ops.Messages.PreparationFailed != 0 || ops.Messages.OnHold != 0 || ops.Messages.RecoveryHold != 0 ||
		ops.Deliveries.Uncertain != 0 || ops.Deliveries.RecipientPolicyHeld != 0 || ops.Deliveries.TrackingHeld != 0 || ops.Deliveries.RecoveryHold != 0 || ops.Webhooks.QueuedDue+ops.Webhooks.QueuedWaiting+ops.Webhooks.InFlight != 0 {
		t.Fatal("operations still count the deleted app", ops.Messages, ops.Deliveries, ops.Webhooks)
	}
	page, err := repo.Attention(ctx, "", 50)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("attention lists a deleted app", page.Items, err)
	}

	// A later event for old mail (a bounce or an open) creates no webhook delivery.
	before := f.count(`SELECT count(*) FROM webhook_deliveries WHERE app_id=$1`, app)
	f.fanout(f.acceptedEvent(app), app)
	f.fanout(f.event(app), app)
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE app_id=$1`, app) != before {
		t.Fatal("a deleted app got a webhook delivery")
	}
	f.fanout(f.event(other), other)
	if f.count(`SELECT count(*) FROM webhook_deliveries WHERE app_id=$1 AND state='queued'`, other) == 0 {
		t.Fatal("another app lost its webhooks")
	}

	// Per-domain audits and a retry after deletion ends instead of requeueing.
	if f.count(`SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='app.deleted'`, app) != 1 {
		t.Fatal("audit")
	}
}

// Receiving domains are audited one by one; a deleted app's bounce domains are
// removed (and audited) and no longer block another app's receiving domain, and
// a racing sending-IP change cannot re-pin a deleted app.
func TestDeleteAppDomainsAndSendingIP(t *testing.T) {
	f := newHoldFixture(t)
	ctx, repo := f.ctx, f.repo
	app, other := f.app, f.app_("other")
	for _, d := range []string{"a.example.net", "b.example.net"} {
		claim, err := repo.RequestInboundDomain(ctx, "operator", app, d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = repo.VerifyInboundDomain(ctx, "operator", app, d, inboundDigest(claim.DNSValue)); err != nil {
			t.Fatal(err)
		}
	}
	f.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'bounce.example',true,now())`, app)
	f.exec(`INSERT INTO bounce_domains(id,app_id,sender_domain,domain) VALUES ($1,$2,'bounce.example','b.bounce.example')`, f.id(), app)
	f.exec(`INSERT INTO sender_domains(app_id,domain,active,verified_at) VALUES ($1,'bounce.example',true,now())`, other)
	if _, err := repo.RequestInboundDomain(ctx, "operator", other, "b.bounce.example"); !errors.Is(err, admin.ErrBounceDomainClash) {
		t.Fatal("bounce domain did not block", err)
	}
	f.exec(`INSERT INTO tracking_domains(id,app_id,sender_domain,domain) VALUES ($1,$2,'bounce.example','t.bounce.example')`, f.id(), app)
	if err := repo.DeleteApp(ctx, "operator", app); err != nil {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='inbound_domain.deleted' AND resource_id IN ('a.example.net','b.example.net')`, app) != 2 ||
		f.count(`SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='bounce_domain.removed' AND resource_id='b.bounce.example'`, app) != 1 {
		t.Fatal("domain removals not audited")
	}
	if f.count(`SELECT count(*) FROM bounce_domains WHERE app_id=$1`, app) != 0 || f.count(`SELECT count(*) FROM tracking_domains WHERE app_id=$1`, app) != 1 {
		t.Fatal("bounce domains kept or tracking domains removed")
	}
	if _, err := repo.RequestInboundDomain(ctx, "operator", other, "b.bounce.example"); err != nil {
		t.Fatal("deleted app's bounce domain still blocks", err)
	}
	// The exclusion check also ignores a bounce row left by a deleted app.
	f.exec(`INSERT INTO bounce_domains(id,app_id,sender_domain,domain) VALUES ($1,$2,'bounce.example','c.bounce.example')`, f.id(), app)
	if _, err := repo.RequestInboundDomain(ctx, "operator", other, "c.bounce.example"); err != nil {
		t.Fatal("stale bounce row blocks", err)
	}
	if _, err := repo.SetAppSendingIP(ctx, "operator", app, nil); !errors.Is(err, admin.ErrNotFound) {
		t.Fatal("a deleted app's sending IP changed", err)
	}
}

// Delete takes the per-app advisory lock inbound commit takes before the app
// row, so the two cannot deadlock: it waits for an inbound commit in progress.
func TestDeleteAppWaitsForTheInboundAppLock(t *testing.T) {
	f := newHoldFixture(t)
	ctx := f.ctx
	tx, err := f.owner.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, appLockKey(f.app)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.repo.DeleteApp(ctx, "operator", f.app) }()
	deadline := time.Now().Add(10 * time.Second)
	for f.count(`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("delete did not wait for the advisory lock")
		}
		select {
		case err = <-done:
			t.Fatal("delete did not wait", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
