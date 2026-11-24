package postgres

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/customdomain"
	"github.com/zyno-io/thinpost/internal/dkimkey"
	"github.com/zyno-io/thinpost/internal/preparation"
	"github.com/zyno-io/thinpost/internal/submission"
)

// newRemovalFixture is the bounce fixture (apps, an edge, bounce and tracking
// domains, delivery claims) with the DKIM keyring, so domains can be verified
// through the real key flow.
func newRemovalFixture(t *testing.T) *bounceFixture {
	t.Helper()
	f := newBounceFixture(t)
	f.repo.DKIMKeys = testKeyring(t)
	return f
}

// verified requests the domain and activates its first key, as the console's
// DNS check does. It returns the active selector.
func (f *bounceFixture) verified(app, domain string) string {
	f.t.Helper()
	challenge, err := f.repo.RequestDomain(f.ctx, "operator", app, domain)
	if err != nil {
		f.t.Fatal(err)
	}
	ref, err := f.repo.PendingDKIMKey(f.ctx, app, domain, "")
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err = f.repo.ActivateDKIMKey(f.ctx, "operator", app, domain, ref, false); err != nil {
		f.t.Fatal(err)
	}
	return strings.TrimSuffix(challenge.DNSName, "._domainkey."+domain)
}

func (f *bounceFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRowContext(f.ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// remove deletes the domain and fails the test on an error.
func (f *bounceFixture) remove(app, domain string) admin.DomainRemoval {
	f.t.Helper()
	removal, err := f.repo.DeleteDomain(f.ctx, "operator", app, domain)
	if err != nil {
		f.t.Fatal(err)
	}
	return removal
}

func (f *bounceFixture) expectedKeep() time.Duration {
	f.t.Helper()
	tx, err := f.db.BeginTx(f.ctx, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	keep, err := keepRetiredRecord(f.ctx, tx)
	if err != nil {
		f.t.Fatal(err)
	}
	return keep
}

func TestDeleteDomainRetiresKeysAndHidesTheDomain(t *testing.T) {
	f := newRemovalFixture(t)
	ctx := f.ctx
	app := f.app("A")
	f.verified(app, "example.com")
	pending, err := f.repo.CreateDKIMKey(ctx, "operator", app, "example.com")
	if err != nil || pending.Domain.PendingKey == nil {
		t.Fatal(err)
	}
	ref, err := f.repo.PendingDKIMKey(ctx, app, "example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	other := f.app("B")
	f.verified(other, "other.example")

	before := time.Now()
	removal := f.remove(app, "example.com")
	if removal.Name != "example.com" || removal.RemovedAt.Before(before.Add(-time.Minute)) || removal.BounceDomain != nil || removal.TrackingDomain != nil {
		t.Fatalf("removal: %+v", removal)
	}
	if len(removal.DKIMRecords) != 2 {
		t.Fatalf("records: %+v", removal.DKIMRecords)
	}
	retired, cancelled := removal.DKIMRecords[0], removal.DKIMRecords[1]
	keep := f.expectedKeep()
	if retired.Selector != "thinpost1" || retired.DNSName != "thinpost1._domainkey.example.com" || retired.KeepUntil == nil ||
		retired.KeepUntil.Before(before.Add(keep-time.Minute)) || retired.KeepUntil.After(time.Now().Add(keep+time.Minute)) {
		t.Fatalf("retired record: %+v (keep %v)", retired, keep)
	}
	if cancelled.Selector != "thinpost2" || cancelled.DNSName != "thinpost2._domainkey.example.com" || cancelled.KeepUntil != nil {
		t.Fatalf("cancelled record: %+v", cancelled)
	}
	if removal.KeepRecordsUntil == nil || !removal.KeepRecordsUntil.Equal(*retired.KeepUntil) {
		t.Fatalf("keep date: %+v", removal.KeepRecordsUntil)
	}

	// The active key is retired and shredded; the pending one is gone.
	var state string
	var sealed []byte
	var keepUntil *time.Time
	if err = f.db.QueryRowContext(ctx, `SELECT state,sealed_private_key,keep_record_until FROM dkim_keys WHERE app_id=$1 AND selector='thinpost1'`, app).Scan(&state, &sealed, &keepUntil); err != nil {
		t.Fatal(err)
	}
	if state != "retired" || sealed != nil || keepUntil == nil || !keepUntil.Equal(*retired.KeepUntil) {
		t.Fatal("key not retired and shredded", state, sealed != nil, keepUntil)
	}
	if f.count(`SELECT count(*) FROM dkim_keys WHERE app_id=$1 AND state='pending'`, app) != 0 || f.count(`SELECT count(*) FROM dkim_keys WHERE app_id=$1`, app) != 1 {
		t.Fatal("pending key not deleted, or a key row lost")
	}
	var activeFlag bool
	var removedAt, verifiedAt *time.Time
	if err = f.db.QueryRowContext(ctx, `SELECT active,removed_at,verified_at FROM sender_domains WHERE app_id=$1 AND domain='example.com'`, app).Scan(&activeFlag, &removedAt, &verifiedAt); err != nil {
		t.Fatal(err)
	}
	if activeFlag || removedAt == nil || !removedAt.Equal(removal.RemovedAt) || verifiedAt == nil {
		t.Fatal("domain row", activeFlag, removedAt, verifiedAt)
	}

	// Hidden and unusable.
	domains, err := f.repo.ListDomains(ctx, app)
	if err != nil || len(domains) != 0 {
		t.Fatal("removed domain listed", domains, err)
	}
	if _, err = f.repo.PendingDKIMKey(ctx, app, "example.com", ""); !errors.Is(err, admin.ErrNotFound) {
		t.Error("pending key of a removed domain", err)
	}
	if _, err = f.repo.CreateDKIMKey(ctx, "operator", app, "example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Error("key for a removed domain", err)
	}
	if _, err = f.repo.ActivateDKIMKey(ctx, "operator", app, "example.com", ref, false); !errors.Is(err, admin.ErrNotFound) {
		t.Error("activate on a removed domain", err)
	}
	if _, err = f.repo.ActiveSigningKey(ctx, app, "example.com"); !errors.Is(err, dkimkey.ErrNoKey) {
		t.Error("a removed domain still has a signing key", err)
	}
	if _, err = f.repo.SetBounceDomain(ctx, "operator", app, "example.com", "bounces.example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Error("bounce domain on a removed domain", err)
	}
	if _, err = f.repo.SetTrackingDomain(ctx, "operator", app, "example.com", "links.example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Error("tracking domain on a removed domain", err)
	}
	if rows, err := f.repo.ListDomains(ctx, other); err != nil || len(rows) != 1 || rows[0].ActiveKey == nil {
		t.Error("another app's domain was touched", rows, err)
	}

	// One event, with details.
	var retiredKey, cancelledKey, bounce, tracking string
	var actor, resource string
	if err = f.db.QueryRowContext(ctx, `SELECT actor_id,resource_id,details->>'retired_key',details->>'cancelled_key',details->>'bounce_domain_removed',details->>'tracking_domain_removed' FROM audit_events WHERE app_id=$1 AND action='domain.removed'`, app).
		Scan(&actor, &resource, &retiredKey, &cancelledKey, &bounce, &tracking); err != nil {
		t.Fatal(err)
	}
	if actor != "operator" || resource != "example.com" || retiredKey != "thinpost1" || cancelledKey != "thinpost2" || bounce != "false" || tracking != "false" {
		t.Fatal("audit event", actor, resource, retiredKey, cancelledKey, bounce, tracking)
	}

	// Input validation.
	if _, err = f.repo.DeleteDomain(ctx, "operator", "short", "example.com"); !errors.Is(err, admin.ErrInvalidInput) {
		t.Error("bad app id", err)
	}
	if _, err = f.repo.DeleteDomain(ctx, "operator", app, "Example.COM"); !errors.Is(err, admin.ErrInvalidInput) {
		t.Error("unnormalized domain", err)
	}
}

func TestDeleteDomainRemovesBounceAndTrackingDomains(t *testing.T) {
	f := newRemovalFixture(t)
	ctx := f.ctx
	a := f.app("A")
	f.verified(a, "example.com")
	f.sender(a, "mail.example.com", true)
	if _, err := f.repo.SetBounceDomain(ctx, "operator", a, "example.com", "bounces.example.com"); err != nil {
		t.Fatal(err)
	}
	f.check(a, "example.com", customdomain.ResultOK, mainName)
	if _, err := f.repo.SetTrackingDomain(ctx, "operator", a, "example.com", "links.example.com"); err != nil {
		t.Fatal(err)
	}
	f.trackingCheck(a, "example.com", outcome(customdomain.HTTPSOK))

	// Mail from a subdomain uses the parent's bounce and tracking domains.
	preparing := func(sender string) string {
		t.Helper()
		f.exec(`DELETE FROM messages WHERE state IN ('queued','preparing')`)
		m := f.id()
		f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,sender_domain,sender_domain_rev,subject,recipient_count) VALUES ($1,$2,$3,false,$4,reverse($4::text)||'.','s',1)`, m, a, "source-"+m, sender)
		claim, err := f.repo.ClaimPreparation(ctx, f.id(), 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return claim.TrackingDomain
	}
	if got := preparing("mail.example.com"); got != "links.example.com" {
		t.Fatal("setup: the subdomain uses its parent's tracking domain", got)
	}
	drained := f.queue(a, "example.com")

	// Checks that started before the removal write nothing afterwards.
	bounceCheck, err := f.repo.BeginBounceCheck(ctx, a, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	trackingCheck, err := f.repo.BeginTrackingCheck(ctx, a, "example.com")
	if err != nil {
		t.Fatal(err)
	}

	removal := f.remove(a, "example.com")
	if removal.BounceDomain == nil || *removal.BounceDomain != "bounces.example.com" || removal.TrackingDomain == nil || *removal.TrackingDomain != "links.example.com" {
		t.Fatalf("removal: %+v", removal)
	}
	if f.count(`SELECT count(*) FROM bounce_domains WHERE app_id=$1`, a) != 0 {
		t.Error("bounce row kept")
	}
	state, _, _ := f.trackingState(a, "links.example.com")
	var token *string
	if err = f.db.QueryRowContext(ctx, `SELECT check_token FROM tracking_domains WHERE app_id=$1`, a).Scan(&token); err != nil || state != "removed" || token != nil {
		t.Fatal("tracking domain is not a tombstone", state, token, err)
	}
	listed, err := f.repo.ListAllTrackingDomains(ctx)
	if err != nil || len(listed) != 1 || listed[0].TrackingDomain.State != "removed" {
		t.Fatal("tombstone not listed for the operator", listed, err)
	}
	if _, written, err := f.repo.FinishBounceCheck(ctx, "operator", bounceCheck, customdomain.ResultOK, mainName); err != nil && !errors.Is(err, admin.ErrNotFound) || written {
		t.Error("a bounce check started before the removal wrote", written, err)
	}
	if _, written, err := f.repo.FinishTrackingCheck(ctx, "operator", trackingCheck, outcome(customdomain.HTTPSOK)); err != nil && !errors.Is(err, admin.ErrNotFound) || written {
		t.Error("a tracking check started before the removal wrote", written, err)
	}
	if f.count(`SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='bounce_domain.removed' AND resource_id='bounces.example.com'`, a) != 1 ||
		f.count(`SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='tracking_domain.removed' AND resource_id='links.example.com'`, a) != 1 {
		t.Error("sub-resource audit events")
	}
	var bounceFlag, trackingFlag string
	if err = f.db.QueryRowContext(ctx, `SELECT details->>'bounce_domain_removed',details->>'tracking_domain_removed' FROM audit_events WHERE app_id=$1 AND action='domain.removed'`, a).Scan(&bounceFlag, &trackingFlag); err != nil || bounceFlag != "true" || trackingFlag != "true" {
		t.Error("domain.removed details", bounceFlag, trackingFlag, err)
	}

	// No subdomain falls back to the removed parent.
	if got := preparing("mail.example.com"); got != "" {
		t.Error("the subdomain still uses the removed domain's tracking domain", got)
	}

	// Mail already prepared drains, from the HELO name.
	if f.count(`SELECT count(*) FROM deliveries WHERE id=$1 AND state='queued'`, drained) != 1 {
		t.Fatal("prepared delivery was touched")
	}
	claim := f.claim(f.caps(true))
	if claim.DeliveryID != drained || claim.BounceDomain != mainName || f.returnPath(drained).Valid {
		t.Errorf("a prepared delivery did not drain from the HELO name: %+v", claim)
	}
	f.resetDeliveries()
	d := f.queue(a, "mail.example.com")
	if claim = f.claim(f.caps(true)); claim.DeliveryID != d || claim.BounceDomain != mainName {
		t.Errorf("a subdomain fell back to the removed parent's bounce domain: %+v", claim)
	}
}

func TestDeleteDomainRefusesWhileTheSMTPPolicySendsAsIt(t *testing.T) {
	f := newRemovalFixture(t)
	ctx := f.ctx
	a := f.app("A")
	f.sender(a, "example.com", true)
	f.sender(a, "other.example", true)
	domain, address := "example.com", "noreply@example.com"
	for name, policy := range map[string]admin.SMTPFromPolicy{
		"domain":  {Mode: admin.SMTPFromDomain, Domain: &domain},
		"address": {Mode: admin.SMTPFromAddress, Address: &address},
	} {
		if _, err := f.repo.SetSMTPSettings(ctx, "operator", a, policy); err != nil {
			t.Fatal(name, err)
		}
		if _, err := f.repo.DeleteDomain(ctx, "operator", a, "example.com"); !errors.Is(err, admin.ErrDomainInSMTPPolicy) || !errors.Is(err, admin.ErrConflict) {
			t.Fatal(name, "policy target removed", err)
		}
		if f.count(`SELECT count(*) FROM sender_domains WHERE app_id=$1 AND domain='example.com' AND active AND removed_at IS NULL`, a) != 1 || f.count(`SELECT count(*) FROM audit_events WHERE action='domain.removed'`) != 0 {
			t.Fatal(name, "refusal changed something")
		}
	}
	// A policy on another domain does not block, and neither does one that is off.
	other := "other.example"
	if _, err := f.repo.SetSMTPSettings(ctx, "operator", a, admin.SMTPFromPolicy{Mode: admin.SMTPFromDomain, Domain: &other}); err != nil {
		t.Fatal(err)
	}
	f.remove(a, "example.com")
	// A policy cannot be set to a removed domain.
	if _, err := f.repo.SetSMTPSettings(ctx, "operator", a, admin.SMTPFromPolicy{Mode: admin.SMTPFromDomain, Domain: &domain}); !errors.Is(err, admin.ErrConflict) {
		t.Fatal("policy set to a removed domain", err)
	}

	// Setting the policy and removing the domain at the same time: exactly one wins.
	for i := 0; i < 12; i++ {
		name := strings.Repeat("x", i+1) + ".example"
		f.sender(a, name, true)
		if _, err := f.repo.SetSMTPSettings(ctx, "operator", a, admin.SMTPFromPolicy{Mode: admin.SMTPFromOff}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var setErr, deleteErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, setErr = f.repo.SetSMTPSettings(ctx, "operator", a, admin.SMTPFromPolicy{Mode: admin.SMTPFromDomain, Domain: &name})
		}()
		go func() {
			defer wg.Done()
			_, deleteErr = f.repo.DeleteDomain(ctx, "operator", a, name)
		}()
		wg.Wait()
		if (setErr == nil) == (deleteErr == nil) {
			t.Fatalf("%s: set %v, delete %v", name, setErr, deleteErr)
		}
		if setErr != nil && !errors.Is(setErr, admin.ErrConflict) || deleteErr != nil && !errors.Is(deleteErr, admin.ErrDomainInSMTPPolicy) {
			t.Fatalf("%s: set %v, delete %v", name, setErr, deleteErr)
		}
	}
}

func TestDeleteDomainIsPerApp(t *testing.T) {
	f := newRemovalFixture(t)
	ctx := f.ctx
	a, b := f.app("A"), f.app("B")
	if sel := f.verified(a, "example.com"); sel != "thinpost1" {
		t.Fatal(sel)
	}
	if sel := f.verified(b, "example.com"); sel != "thinpost2" {
		t.Fatal("selectors are unique across apps", sel)
	}
	for _, app := range []string{a, b} {
		// Two apps may name the same bounce and tracking domains.
		if _, err := f.repo.SetBounceDomain(ctx, "operator", app, "example.com", "bounces.example.com"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.repo.SetTrackingDomain(ctx, "operator", app, "example.com", "links.example.com"); err != nil {
			t.Fatal(err)
		}
		f.trackingCheck(app, "example.com", outcome(customdomain.HTTPSOK))
	}
	f.remove(a, "example.com")

	domains, err := f.repo.ListDomains(ctx, b)
	if err != nil || len(domains) != 1 || !domains[0].Active || domains[0].ActiveKey == nil || domains[0].ActiveKey.Selector != "thinpost2" || domains[0].RetiredKey != nil {
		t.Fatal("the other app's domain changed", domains, err)
	}
	if _, err = f.repo.ActiveSigningKey(ctx, b, "example.com"); err != nil {
		t.Error("the other app cannot sign any more", err)
	}
	if f.count(`SELECT count(*) FROM bounce_domains WHERE app_id=$1`, b) != 1 {
		t.Error("the other app's bounce domain was removed")
	}
	if state, _, _ := f.trackingState(b, "links.example.com"); state == "removed" {
		t.Error("the other app's tracking domain was removed")
	}
	// B's next key skips A's retired selector.
	next, err := f.repo.CreateDKIMKey(ctx, "operator", b, "example.com")
	if err != nil || next.DNSName != "thinpost3._domainkey.example.com" {
		t.Fatal("rotation reused a retired selector", next.DNSName, err)
	}
}

func TestAddingARemovedDomainRevivesIt(t *testing.T) {
	f := newRemovalFixture(t)
	ctx := f.ctx
	a := f.app("A")
	f.verified(a, "example.com")
	removal := f.remove(a, "example.com")

	challenge, err := f.repo.RequestDomain(ctx, "operator", a, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if challenge.Domain.Active || challenge.Domain.VerifiedAt != nil || challenge.Domain.ActiveKey != nil || challenge.Domain.PendingKey == nil {
		t.Fatalf("revived domain: %+v", challenge.Domain)
	}
	if challenge.DNSName == "thinpost1._domainkey.example.com" {
		t.Fatal("a retired selector was reused")
	}
	if !challenge.Domain.CreatedAt.After(removal.RemovedAt) {
		t.Error("created_at was not renewed", challenge.Domain.CreatedAt, removal.RemovedAt)
	}
	// The old record still counts while inside its window.
	if challenge.Domain.RetiredKey == nil || challenge.Domain.RetiredKey.Selector != "thinpost1" {
		t.Error("the retired key is not shown after re-adding", challenge.Domain.RetiredKey)
	}
	var removedAt, verifiedAt *time.Time
	if err = f.db.QueryRowContext(ctx, `SELECT removed_at,verified_at FROM sender_domains WHERE app_id=$1 AND domain='example.com'`, a).Scan(&removedAt, &verifiedAt); err != nil || removedAt != nil || verifiedAt != nil {
		t.Fatal("row not revived", removedAt, verifiedAt, err)
	}
	// Asking again is the ordinary idempotent request, not another revival.
	again, err := f.repo.RequestDomain(ctx, "operator", a, "example.com")
	if err != nil || again.DNSName != challenge.DNSName || !again.Domain.CreatedAt.Equal(challenge.Domain.CreatedAt) {
		t.Fatal("second request", again, err)
	}
	ref, err := f.repo.PendingDKIMKey(ctx, a, "example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.repo.ActivateDKIMKey(ctx, "operator", a, "example.com", ref, false)
	if err != nil || !d.Active || d.VerifiedAt == nil {
		t.Fatal("verify", d, err)
	}
	if f.count(`SELECT count(*) FROM audit_events WHERE app_id=$1 AND action='domain.verified'`, a) != 2 {
		t.Error("verifying again is not audited")
	}
	// A removed domain can be removed again after coming back.
	f.remove(a, "example.com")
}

func TestDeleteDomainNotFoundCases(t *testing.T) {
	f := newRemovalFixture(t)
	ctx := f.ctx
	a, deleted, paused := f.app("A"), f.app("Deleted"), f.app("Paused")
	f.verified(a, "example.com")
	f.verified(deleted, "example.com")
	f.verified(paused, "example.com")
	f.remove(a, "example.com")
	if _, err := f.repo.DeleteDomain(ctx, "operator", a, "example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Error("a repeat removal", err)
	}
	if _, err := f.repo.DeleteDomain(ctx, "operator", a, "never.example"); !errors.Is(err, admin.ErrNotFound) {
		t.Error("an unknown domain", err)
	}
	if _, err := f.repo.DeleteDomain(ctx, "operator", f.id(), "example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Error("an unknown app", err)
	}
	if err := f.repo.DeleteApp(ctx, "operator", deleted); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.DeleteDomain(ctx, "operator", deleted, "example.com"); !errors.Is(err, admin.ErrNotFound) {
		t.Error("a deleted app", err)
	}
	f.exec(`UPDATE apps SET active=false WHERE id=$1`, paused)
	if _, err := f.repo.DeleteDomain(ctx, "operator", paused, "example.com"); err != nil {
		t.Error("a paused app may remove a domain", err)
	}
}

func TestDeleteDomainRefusesNewMail(t *testing.T) {
	f := newSMTPCommitFixture(t)
	// The commit fixture's domain was inserted active without a key; removal
	// does not need one.
	if _, err := f.repo.DeleteDomain(f.ctx, "operator", f.app, "example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Commit(f.ctx, f.httpRecord()); !errors.Is(err, submission.ErrInvalidMessage) {
		t.Error("API mail from a removed domain", err)
	}
	if _, err := f.repo.Commit(f.ctx, f.smtpRecord()); !errors.Is(err, submission.ErrSenderDomainInactive) {
		t.Error("SMTP mail from a removed domain", err)
	}
	if f.messageCount() != 0 {
		t.Error("a refused message left rows")
	}
	_, verified, err := f.repo.SMTPPolicy(f.ctx, f.app, []string{"example.com"})
	if err != nil || len(verified) != 0 {
		t.Error("the SMTP verified list still has the removed domain", verified, err)
	}
}

func TestDeleteDomainFailsUnpreparedMailFast(t *testing.T) {
	f := newRemovalFixture(t)
	ctx := f.ctx
	f.repo.Objects = objectVerifierFunc(func(context.Context, string) error { return nil })
	a := f.app("A")
	f.verified(a, "example.com")
	f.sender(a, "pending.example", false)
	message := func(sender string) string {
		m := f.id()
		f.exec(`INSERT INTO messages(id,app_id,object_key,enhanced_protection,sender_domain,sender_domain_rev,subject,recipient_count) VALUES ($1,$2,$3,false,$4,reverse($4::text)||'.','s',1)`, m, a, "source-"+m, sender)
		return m
	}
	claim := func(m string) preparation.Claim {
		t.Helper()
		c, err := f.repo.ClaimPreparation(ctx, f.id(), time.Minute)
		if err != nil || c.MessageID != m {
			t.Fatal("claim", c.MessageID, m, err)
		}
		return c
	}
	state := func(m string) string {
		var s string
		if err := f.db.QueryRowContext(ctx, `SELECT state FROM messages WHERE id=$1`, m).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// A domain that was never removed keeps the ordinary retry schedule.
	unverified := message("pending.example")
	if terminal, err := f.repo.FailPreparation(ctx, claim(unverified), "signing_key"); err != nil || terminal || state(unverified) != "queued" {
		t.Fatal("an unverified domain's failure is final", terminal, err, state(unverified))
	}
	f.exec(`DELETE FROM messages`)

	removed := message("example.com")
	inflight := claim(removed)
	f.remove(a, "example.com")
	// A commit racing the removal fails and leaves no deliveries.
	inflight.SigningDomain = "example.com"
	id := f.id()
	if err := f.repo.CommitPreparation(ctx, inflight, []preparation.Delivery{{Format: 1, ID: id, RecipientIndex: 0, ObjectKey: fixtureObjectKey("final", id)}}); err == nil {
		t.Fatal("a commit for a removed domain succeeded")
	}
	if f.count(`SELECT count(*) FROM deliveries`) != 0 {
		t.Fatal("a delivery was queued")
	}
	// Other failures of the same message stay on the retry schedule.
	if terminal, err := f.repo.FailPreparation(ctx, inflight, "source_read"); err != nil || terminal || state(removed) != "queued" {
		t.Fatal("a non-key failure is final", terminal, err, state(removed))
	}
	f.exec(`UPDATE messages SET prepare_next_at=now() WHERE id=$1`, removed)
	again := claim(removed)
	terminal, err := f.repo.FailPreparation(ctx, again, "signing_key")
	if err != nil || !terminal || state(removed) != "preparation_failed" {
		t.Fatal("a signing_key failure for a removed domain is not final", terminal, err, state(removed))
	}
	if f.count(`SELECT count(*) FROM webhook_events WHERE message_id=$1 AND outcome='preparation_failed'`, removed) != 1 {
		t.Error("no preparation_failed event")
	}
	var reason string
	if err = f.db.QueryRowContext(ctx, `SELECT prepare_failure FROM messages WHERE id=$1`, removed).Scan(&reason); err != nil || reason != "signing_key" {
		t.Error("failure reason", reason, err)
	}
	// Retry works once the domain is back.
	if err = f.repo.RetryPreparation(ctx, "operator", a, removed); err != nil {
		t.Fatal(err)
	}
	if state(removed) != "queued" {
		t.Error("retry did not requeue", state(removed))
	}
	f.verified(a, "example.com")
	if f.count(`SELECT count(*) FROM sender_domains WHERE app_id=$1 AND domain='example.com' AND active AND removed_at IS NULL`, a) != 1 {
		t.Error("domain not back")
	}
}

// Removing a domain while other operations on it and its app run must never
// deadlock or leave a removed domain with a usable key, bounce or tracking row.
func TestDeleteDomainRaces(t *testing.T) {
	f := newRemovalFixture(t)
	ctx := f.ctx
	allowed := func(err error) bool {
		return err == nil || errors.Is(err, admin.ErrNotFound) || errors.Is(err, admin.ErrConflict) || errors.Is(err, admin.ErrAppPaused) ||
			errors.Is(err, admin.ErrSenderDomainUnverified)
	}
	for i := 0; i < 25; i++ {
		app := f.app("race")
		domain := "race.example"
		f.verified(app, domain)
		if _, err := f.repo.SetBounceDomain(ctx, "operator", app, domain, "bounces.race.example"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.repo.SetTrackingDomain(ctx, "operator", app, domain, "links.race.example"); err != nil {
			t.Fatal(err)
		}
		operations := []func() error{
			func() error { _, err := f.repo.DeleteDomain(ctx, "operator", app, domain); return err },
			func() error { _, err := f.repo.DeleteDomain(ctx, "operator", app, domain); return err },
			func() error { _, err := f.repo.CreateDKIMKey(ctx, "operator", app, domain); return err },
			func() error {
				_, err := f.repo.SetBounceDomain(ctx, "operator", app, domain, "b2.race.example")
				return err
			},
			func() error {
				_, err := f.repo.SetTrackingDomain(ctx, "operator", app, domain, "l2.race.example")
				return err
			},
		}
		if i%3 == 0 {
			operations = append(operations, func() error { return f.repo.DeleteApp(ctx, "operator", app) })
		}
		errs := make([]error, len(operations))
		var wg sync.WaitGroup
		for n, op := range operations {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[n] = op()
			}()
		}
		wg.Wait()
		for n, err := range errs {
			if !allowed(err) {
				t.Fatalf("round %d, operation %d: %v", i, n, err)
			}
		}
		var removed bool
		if err := f.db.QueryRowContext(ctx, `SELECT removed_at IS NOT NULL FROM sender_domains WHERE app_id=$1 AND domain=$2`, app, domain).Scan(&removed); err != nil {
			t.Fatal(err)
		}
		if !removed {
			continue
		}
		if f.count(`SELECT count(*) FROM dkim_keys WHERE app_id=$1 AND state IN ('active','pending')`, app) != 0 ||
			f.count(`SELECT count(*) FROM bounce_domains WHERE app_id=$1`, app) != 0 ||
			f.count(`SELECT count(*) FROM tracking_domains WHERE app_id=$1 AND state<>'removed'`, app) != 0 ||
			f.count(`SELECT count(*) FROM sender_domains WHERE app_id=$1 AND active`, app) != 0 {
			t.Fatalf("round %d: a removed domain kept a key, bounce or tracking domain", i)
		}
	}
}
